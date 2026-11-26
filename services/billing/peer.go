package billing

import (
	"context"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

// policyPeer serves peer.PricingPoliciesKey to compute.
type policyPeer struct{ svc *domain.PricingPolicyService }

var _ peer.PricingPolicies = policyPeer{}

func (p policyPeer) PricingPolicy(ctx context.Context, id string) (peer.PricingPolicy, error) {
	pp, err := p.svc.Lookup(ctx, id)
	if err != nil {
		return peer.PricingPolicy{}, err
	}
	return peer.PricingPolicy{
		ID:        pp.GetMetadata().GetId(),
		ProjectID: pp.GetMetadata().GetParentId(),
		Platform:  pp.GetSpec().GetComputeInstanceSpec().GetV1().GetPlatform(),
		MaxPrice:  pp.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice(),
		State:     policyStates[pp.GetStatus().GetState()],
		Blocked:   pp.GetStatus().GetSchedulingState() == billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED,
	}, nil
}

var policyStates = map[billingpb.PricingPolicyStatus_State]peer.PricingPolicyState{
	billingpb.PricingPolicyStatus_STATE_CREATING: peer.PricingPolicyCreating,
	billingpb.PricingPolicyStatus_STATE_ACTIVE:   peer.PricingPolicyActive,
	billingpb.PricingPolicyStatus_STATE_UPDATING: peer.PricingPolicyUpdating,
	billingpb.PricingPolicyStatus_STATE_DELETING: peer.PricingPolicyDeleting,
}

// computeVMs counts the VMs of a pricing policy through the compute module's
// capability, looked up per call: 0 when compute is not enabled.
type computeVMs struct{ dir *peer.Directory }

var _ domain.RunningVMs = computeVMs{}

func (c computeVMs) RunningVMCount(ctx context.Context, projectID, policyID string) (int64, error) {
	vms, ok := peer.Lookup(c.dir, peer.PricingPolicyVMsKey)
	if !ok {
		return 0, nil
	}
	return vms.RunningVMCount(ctx, projectID, policyID)
}
