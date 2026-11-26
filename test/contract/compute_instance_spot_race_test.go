package contract

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/operations"
	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

// Each pair races a Start against a max_price change spotRaceRounds times;
// the pairs run in parallel to add contention.
const (
	spotRacePairs  = 4
	spotRaceRounds = 15
)

// TestInstanceSpotStartRacesMaxPriceChange races the Start of a stopped
// instance under a policy against a change of the policy's max_price to a
// blocking price. Billing changes max_price only while no VM runs under the
// policy, and a blocked (or updating) policy lets no VM start, so exactly
// one of the two succeeds in every round.
func TestInstanceSpotStartRacesMaxPriceChange(t *testing.T) {
	t.Parallel()
	e := startSpot(t)
	var wg sync.WaitGroup
	for i := range spotRacePairs {
		name := fmt.Sprintf("race%d", i)
		policyID := e.policy(t, name, "2.000")
		spec := e.spotSpec(name, policyID)
		spec.Stopped = true
		inst := e.mustCreate(t, name, spec)
		wg.Go(func() {
			if err := e.raceStartAgainstPrice(t.Context(), inst.GetMetadata().GetId(), policyID); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func (e spotEnv) raceStartAgainstPrice(ctx context.Context, instanceID, policyID string) error {
	for round := range spotRaceRounds {
		blockReq, err := e.priceRequest(ctx, policyID, "1.000") // below the 1.250 spot price
		if err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		var startOp, priceOp operations.Operation
		var startErr, priceErr error
		begin := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-begin
			startOp, startErr = e.instances.Start(ctx, &computepb.StartInstanceRequest{Id: instanceID})
		})
		wg.Go(func() {
			<-begin
			priceOp, priceErr = e.policies.Update(ctx, blockReq)
		})
		close(begin)
		wg.Wait()
		switch {
		case startErr == nil && priceErr == nil:
			return fmt.Errorf("round %d: both the Start of %s and the max_price change of %s succeeded", round, instanceID, policyID)
		case startErr != nil && priceErr != nil:
			return fmt.Errorf("round %d: both failed: start: %w; max_price: %w", round, startErr, priceErr)
		}
		if err := e.resetRace(ctx, instanceID, policyID, startOp, priceOp); err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
	}
	return nil
}

// priceRequest is an update of the policy's max_price to price.
func (e spotEnv) priceRequest(ctx context.Context, policyID, price string) (*billingpb.UpdatePricingPolicyRequest, error) {
	p, err := e.policies.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: policyID})
	if err != nil {
		return nil, fmt.Errorf("get policy: %w", err)
	}
	spec := proto.CloneOf(p.GetSpec())
	spec.Pricing = &billingpb.PricingMethod{Versions: &billingpb.PricingMethod_MaxPriceV1{MaxPriceV1: &billingpb.MaxPriceV1{MaxPrice: price}}}
	return &billingpb.UpdatePricingPolicyRequest{Metadata: p.GetMetadata(), Spec: spec}, nil
}

// resetRace waits for the winner's operation, stops the instance if it
// started, and restores max_price "2.000".
func (e spotEnv) resetRace(ctx context.Context, instanceID, policyID string, startOp, priceOp operations.Operation) error {
	if startOp != nil {
		if _, err := startOp.Wait(ctx); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		if err := e.stop(ctx, instanceID); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
	}
	if priceOp != nil {
		if _, err := priceOp.Wait(ctx); err != nil {
			return fmt.Errorf("max_price change: %w", err)
		}
	}
	req, err := e.priceRequest(ctx, policyID, "2.000")
	if err != nil {
		return err
	}
	op, err := e.policies.Update(ctx, req)
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		return fmt.Errorf("restore max_price: %w", err)
	}
	return nil
}
