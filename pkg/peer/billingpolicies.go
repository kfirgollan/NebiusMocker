package peer

import "context"

// PricingPoliciesKey is the capability provided by the billing module: the
// pricing policies (maximum prices) that preemptible compute instances
// reference in spec.spot_pricing_policy.
var PricingPoliciesKey = NewKey[PricingPolicies]("billing.pricing-policies")

// PricingPolicies reads billing pricing policies for compute. It is an
// internal call: no IAM checks.
type PricingPolicies interface {
	// PricingPolicy returns the policy, or errs.NotFound when it does not
	// exist.
	PricingPolicy(ctx context.Context, id string) (PricingPolicy, error)
}

// PricingPolicyState is the lifecycle state of a pricing policy
// (nebius.billing.v1.PricingPolicyStatus.State).
type PricingPolicyState int

// Pricing policy states.
const (
	PricingPolicyCreating PricingPolicyState = iota + 1
	PricingPolicyActive
	PricingPolicyUpdating
	PricingPolicyDeleting
)

// PricingPolicy is what compute needs to know about a pricing policy.
type PricingPolicy struct {
	ID string
	// ProjectID is the policy's parent: only instances of that project may
	// reference it.
	ProjectID string
	// Platform is the compute platform the policy prices
	// (spec.compute_instance_spec.v1.platform).
	Platform string
	// MaxPrice is the per-GPU hourly maximum in USD, e.g. "3.515".
	MaxPrice string
	State    PricingPolicyState
	// Blocked reports status.scheduling_state BLOCKED: max_price is below the
	// current spot price, so no VM may start under the policy.
	Blocked bool
}

// PricingPolicyVMsKey is the capability provided by the compute module: how
// many instances run under a pricing policy (the policy's
// status.running_vm_count). The count comes from compute's own store, so it
// stays right across restarts.
var PricingPolicyVMsKey = NewKey[PricingPolicyVMs]("compute.pricing-policy-vms")

// PricingPolicyVMs counts the compute instances running under a pricing
// policy. It is an internal call: no IAM checks.
type PricingPolicyVMs interface {
	// RunningVMCount returns the number of instances of projectID whose
	// spec.spot_pricing_policy.id is policyID and that are running (or
	// starting, or stopping).
	RunningVMCount(ctx context.Context, projectID, policyID string) (int64, error)
}
