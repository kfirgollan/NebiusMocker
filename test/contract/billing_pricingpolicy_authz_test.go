package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	billingsvc "github.com/nebius/gosdk/services/nebius/billing/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func startBillingEnforcing(t *testing.T) *billingEnv {
	t.Helper()
	return startBilling(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
}

// policiesAs returns the pricing policy client authenticated with the token.
func (e *billingEnv) policiesAs(t *testing.T, token string) billingsvc.PricingPolicyService {
	t.Helper()
	return billingsvc.NewPricingPolicyService(e.inst.SDKAs(t, token))
}

func TestBillingPricingPolicyViewerReadsButCannotWrite(t *testing.T) {
	t.Parallel()
	e := startBillingEnforcing(t)
	ctx := t.Context()
	p := e.createPolicy(t, "guarded", "2.500")
	id := p.GetMetadata().GetId()
	viewer := e.policiesAs(t, config.DefaultViewerToken)

	if _, err := viewer.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: id}); err != nil {
		t.Errorf("Get: %v", err)
	}
	if _, err := viewer.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.inst.ProjectID, Name: "guarded"}); err != nil {
		t.Errorf("GetByName: %v", err)
	}
	if l, err := viewer.List(ctx, &billingpb.ListPricingPoliciesRequest{ParentId: e.inst.ProjectID}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("List = %v, %v", l, err)
	}

	op, err := viewer.Create(ctx, createPolicyRequest(e.inst.ProjectID, "byviewer", "2.000"))
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = viewer.Update(ctx, &billingpb.UpdatePricingPolicyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "renamed"}, Spec: policySpec("gpu-h100-sxm", "2.500"),
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = viewer.Delete(ctx, &billingpb.DeletePricingPolicyRequest{Id: id})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)

	got, err := e.policies.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: id})
	if err != nil || got.GetMetadata().GetName() != "guarded" {
		t.Errorf("policy after denied writes = %v, %v", got, err)
	}
}

func TestBillingPricingPolicyEditorWrites(t *testing.T) {
	t.Parallel()
	e := startBillingEnforcing(t)
	ctx := t.Context()
	editor := e.policiesAs(t, config.DefaultEditorToken)

	op, err := editor.Create(ctx, createPolicyRequest(e.inst.ProjectID, "byeditor", "2.000"))
	kmsMustWait(t, op, err)
	id := op.ResourceID()
	if l, err := editor.List(ctx, &billingpb.ListPricingPoliciesRequest{ParentId: e.inst.ProjectID}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("List = %v, %v", l, err)
	}
	op, err = editor.Update(ctx, &billingpb.UpdatePricingPolicyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "byeditor2"}, Spec: policySpec("gpu-h100-sxm", "3.000"),
	})
	kmsMustWait(t, op, err)
	got, err := editor.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: id})
	if err != nil || got.GetMetadata().GetName() != "byeditor2" || got.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice() != "3.000" {
		t.Errorf("policy after update = %v, %v", got, err)
	}
	op, err = editor.Delete(ctx, &billingpb.DeletePricingPolicyRequest{Id: id})
	kmsMustWait(t, op, err)
}

func TestBillingPricingPolicyAdminAndUnauthenticated(t *testing.T) {
	t.Parallel()
	e := startBillingEnforcing(t)
	ctx := t.Context()
	admin := e.policies
	op, err := admin.Create(ctx, createPolicyRequest(e.inst.ProjectID, "byadmin", "2.000"))
	kmsMustWait(t, op, err)

	stranger := e.policiesAs(t, "no-such-token")
	_, err = stranger.List(ctx, &billingpb.ListPricingPoliciesRequest{ParentId: e.inst.ProjectID})
	kmsWantCode(t, err, codes.Unauthenticated)
	_, err = stranger.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: op.ResourceID()})
	kmsWantCode(t, err, codes.Unauthenticated)
	op, err = stranger.Create(ctx, createPolicyRequest(e.inst.ProjectID, "bystranger", "2.000"))
	kmsWantCode(t, kmsWait(ctx, op, err), codes.Unauthenticated)
}
