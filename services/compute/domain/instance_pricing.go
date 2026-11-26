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

// whileRunnable runs start, which writes the instance's move to running,
// directly when spec has no pricing policy, or under the policy's admission
// (peer.PricingPolicies.Admit) when the policy lets VMs start: ACTIVE and
// scheduling ALLOWED. Billing changes max_price and deletes policies under
// the same lock after counting running VMs, so a start and such a change
// never both succeed. refused is why the policy kept the instance from
// starting (start did not run); err is start's own error.
//
// Lock order: the compute NIC lock (held by Start and Update), then the
// billing policy lock, then compute's store and the peers start calls (vpc,
// capacity, quotas). Billing, under its lock, only reads compute's store
// (RunningVMCount), so no path takes them in the opposite order. Never call
// whileRunnable inside a store transaction.
func (s *InstanceService) whileRunnable(ctx context.Context, spec *computepb.InstanceSpec, start func(ctx context.Context) error) (refused, err error) {
	id := spec.GetSpotPricingPolicy().GetId()
	if id == "" {
		return nil, start(ctx)
	}
	pp, ok := s.d.Pricing.PricingPolicies()
	if !ok {
		return errBillingDisabled(), nil
	}
	var started bool
	admitErr := pp.Admit(ctx, id, func(ctx context.Context, pol peer.PricingPolicy) error {
		if err := runnable(id, pol); err != nil {
			return err
		}
		started = true
		return start(ctx)
	})
	switch {
	case started:
		return nil, admitErr
	case errs.IsKind(admitErr, errs.KindNotFound):
		return policyNotFound(id, admitErr), nil
	default:
		return admitErr, nil
	}
}

// runnable checks that VMs may start under pol.
func runnable(id string, pol peer.PricingPolicy) error {
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

func errBillingDisabled() error {
	return errs.BadRequest(errs.Violation{
		Field: spotPolicyField, Message: "the billing module is not enabled, so pricing policies are not available",
	})
}

func policyNotFound(id string, err error) error {
	return fmt.Errorf("%s: pricing policy %s does not exist (select another pricing policy): %w", spotPolicyField, id, err)
}

// pricingPolicy reads a pricing policy from the billing module.
func (s *InstanceService) pricingPolicy(ctx context.Context, id string) (peer.PricingPolicy, error) {
	pp, ok := s.d.Pricing.PricingPolicies()
	if !ok {
		return peer.PricingPolicy{}, errBillingDisabled()
	}
	pol, err := pp.PricingPolicy(ctx, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return peer.PricingPolicy{}, policyNotFound(id, err)
	}
	return pol, err
}

// RunningVMCount implements peer.PricingPolicyVMs: the instances of a
// project running under a pricing policy, counted from the store. Billing
// calls it while holding its policy lock, so it must only read the store:
// never take the NIC lock or another compute lock, or call a peer (see the
// lock order at whileRunnable).
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
