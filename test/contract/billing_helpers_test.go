package contract

import (
	"testing"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	billingsvc "github.com/nebius/gosdk/services/nebius/billing/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/billing"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// billingEnv is a mocker with IAM, compute and billing.
type billingEnv struct {
	inst     *testkit.Instance
	policies billingsvc.PricingPolicyService
}

func startBilling(t *testing.T, opts ...testkit.Option) *billingEnv {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), billing.New()}, opts...)
	return &billingEnv{inst: inst, policies: billingsvc.NewPricingPolicyService(inst.SDK(t))}
}

// policySpec is the spec of a pricing policy for platform at maxPrice.
func policySpec(platform, maxPrice string) *billingpb.PricingPolicySpec {
	return &billingpb.PricingPolicySpec{
		ComputeInstanceSpec: &billingpb.ComputeInstanceSpec{Versions: &billingpb.ComputeInstanceSpec_V1{
			V1: &billingpb.ComputeInstanceSpecV1{Platform: platform},
		}},
		Pricing: &billingpb.PricingMethod{Versions: &billingpb.PricingMethod_MaxPriceV1{
			MaxPriceV1: &billingpb.MaxPriceV1{MaxPrice: maxPrice},
		}},
	}
}

func createPolicyRequest(parentID, name, maxPrice string) *billingpb.CreatePricingPolicyRequest {
	return &billingpb.CreatePricingPolicyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parentID, Name: name},
		Spec:     policySpec("gpu-h100-sxm", maxPrice),
	}
}

// createPolicy creates a gpu-h100-sxm policy in the seeded project and
// returns it as read back.
func (e *billingEnv) createPolicy(t *testing.T, name, maxPrice string) *billingpb.PricingPolicy {
	t.Helper()
	op, err := e.policies.Create(t.Context(), createPolicyRequest(e.inst.ProjectID, name, maxPrice))
	id := mustOp(t, op, err)
	p, err := e.policies.Get(t.Context(), &billingpb.GetPricingPolicyRequest{Id: id})
	if err != nil {
		t.Fatalf("get pricing policy: %v", err)
	}
	return p
}
