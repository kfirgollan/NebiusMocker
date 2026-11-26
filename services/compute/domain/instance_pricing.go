package domain

import (
	"context"
	"fmt"
	"strings"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Pricing models (spec.pricing_model, docs compute/virtual-machines/preemptible
// and signup-billing/pricing-policy):
//   - on_demand: a regular VM;
//   - follows_spot_price: a preemptible VM that pays the current spot price;
//   - spot_pricing_policy: a preemptible VM that pays the spot price up to the
//     max_price of a billing pricing policy of its project and platform. It
//     can start only while the policy is ACTIVE and its scheduling is
//     ALLOWED (max_price at or above the spot price). A create under a
//     BLOCKED policy fails but leaves the VM STOPPED.
//
// While the VM runs, billing counts it in the policy's running_vm_count
// (RunningVMCount), which keeps the policy's max_price and the policy itself
// from changing. Spot prices are constant in NebiusMocker, so a running VM is
// never preempted (docs/services/compute.md).

// PricingPolicies finds the billing pricing policies capability at call time
// (the billing module may be disabled).
type PricingPolicies interface {
	PricingPolicies() (peer.PricingPolicies, bool)
}

const (
	kindPricingPolicy = "pricingpolicy"
	spotPolicyField   = "spec.spot_pricing_policy.id"
)

// checkPricingModel checks spec.pricing_model against spec.preemptible.
func checkPricingModel(spec *computepb.InstanceSpec) error {
	preemptible := spec.GetPreemptible() != nil
	onlyPreemptible := func(field string) error {
		return errs.BadRequest(errs.Violation{Field: field, Message: "only preemptible instances (spec.preemptible) are priced by the spot market"})
	}
	switch spec.GetPricingModel().(type) {
	case *computepb.InstanceSpec_OnDemand:
		if preemptible {
			return errs.BadRequest(errs.Violation{Field: "spec.on_demand", Message: "a preemptible instance uses follows_spot_price or spot_pricing_policy"})
		}
	case *computepb.InstanceSpec_FollowsSpotPrice:
		if !preemptible {
			return onlyPreemptible("spec.follows_spot_price")
		}
	case *computepb.InstanceSpec_SpotPricingPolicy:
		id := spec.GetSpotPricingPolicy().GetId()
		switch {
		case !preemptible:
			return onlyPreemptible("spec.spot_pricing_policy")
		case id == "":
			return errs.BadRequest(errs.Violation{Field: spotPolicyField, Message: "required"})
		case !strings.HasPrefix(id, kindPricingPolicy+"-"):
			return errs.BadRequest(errs.Violation{Field: spotPolicyField, Message: "must be a pricing policy ID"})
		}
	}
	return nil
}

// checkPricingPolicy validates a new reference to a pricing policy (on
// create, or an update that changes it): the policy exists, belongs to the
// instance's project, prices the instance's platform and is not being
// deleted. An unchanged reference is not checked again, so a stopped VM
// whose policy was deleted can still be updated (docs: select another
// policy before starting it).
func (s *InstanceService) checkPricingPolicy(ctx context.Context, projectID string, prev, next *computepb.InstanceSpec) error {
	id := next.GetSpotPricingPolicy().GetId()
	if id == "" || id == prev.GetSpotPricingPolicy().GetId() {
		return nil
	}
	pol, err := s.pricingPolicy(ctx, id)
	if err != nil {
		return err
	}
	platform := next.GetResources().GetPlatform()
	switch {
	case pol.ProjectID != projectID:
		return errs.BadRequest(errs.Violation{Field: spotPolicyField, Message: fmt.Sprintf(
			"pricing policy %s belongs to project %s: only instances of that project may use it", id, pol.ProjectID)})
	case pol.Platform != platform:
		return errs.BadRequest(errs.Violation{Field: spotPolicyField, Message: fmt.Sprintf(
			"pricing policy %s applies to platform %s, not %s", id, pol.Platform, platform)})
	case pol.State == peer.PricingPolicyDeleting:
		return errs.BadState(id, "the pricing policy is being deleted: it accepts no new VMs")
	}
	return nil
}

// admitRun checks that an instance with spec may start under its pricing
// policy, if it has one: the policy is ACTIVE and its scheduling ALLOWED.
func (s *InstanceService) admitRun(ctx context.Context, spec *computepb.InstanceSpec) error {
	id := spec.GetSpotPricingPolicy().GetId()
	if id == "" {
		return nil
	}
	pol, err := s.pricingPolicy(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case pol.State == peer.PricingPolicyCreating || pol.State == peer.PricingPolicyUpdating:
		return errs.Conflict(id, "the pricing policy is not ready (its operation is still running): retry when it is ACTIVE")
	case pol.State == peer.PricingPolicyDeleting:
		return errs.BadState(id, "the pricing policy is being deleted: select another pricing policy before starting the instance")
	case pol.Blocked:
		return errs.BadState(id, fmt.Sprintf(
			"pricing policy %s blocks scheduling: its max_price %s USD is below the current spot price; "+
				"raise max_price, select another pricing policy or wait for the spot price to fall", id, pol.MaxPrice))
	}
	return nil
}

// pricingPolicy reads a pricing policy from the billing module.
func (s *InstanceService) pricingPolicy(ctx context.Context, id string) (peer.PricingPolicy, error) {
	pp, ok := s.d.Pricing.PricingPolicies()
	if !ok {
		return peer.PricingPolicy{}, errs.BadRequest(errs.Violation{
			Field: spotPolicyField, Message: "the billing module is not enabled, so pricing policies are not available",
		})
	}
	pol, err := pp.PricingPolicy(ctx, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return peer.PricingPolicy{}, fmt.Errorf("%s: pricing policy %s does not exist (select another pricing policy): %w", spotPolicyField, id, err)
	}
	return pol, err
}

// RunningVMCount implements peer.PricingPolicyVMs: the instances of a
// project running under a pricing policy, counted from the store.
func (s *InstanceService) RunningVMCount(ctx context.Context, projectID, policyID string) (int64, error) {
	var n int64
	token := ""
	for {
		page, err := s.d.Instances.List(ctx, projectID, maxPageSize, token)
		if err != nil {
			return 0, err
		}
		for _, inst := range page.Items {
			if inst.GetSpec().GetSpotPricingPolicy().GetId() == policyID && runsOrWillRun(inst) {
				n++
			}
		}
		if page.NextPageToken == "" {
			return n, nil
		}
		token = page.NextPageToken
	}
}

// runsOrWillRun reports whether an instance runs: RUNNING, or on its way in
// or out of it. A CREATING or UPDATING instance runs unless its spec stops
// it (an instance stopped with Stop, under an update, is counted until the
// update completes).
func runsOrWillRun(inst *computepb.Instance) bool {
	switch inst.GetStatus().GetState() {
	case computepb.InstanceStatus_STARTING, computepb.InstanceStatus_RUNNING, computepb.InstanceStatus_STOPPING:
		return true
	case computepb.InstanceStatus_CREATING, computepb.InstanceStatus_UPDATING:
		return !inst.GetSpec().GetStopped()
	case computepb.InstanceStatus_UNSPECIFIED, computepb.InstanceStatus_STOPPED, computepb.InstanceStatus_DELETING, computepb.InstanceStatus_ERROR:
		return false
	}
	return false
}
