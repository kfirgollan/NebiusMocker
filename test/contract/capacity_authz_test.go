package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func startCapacityEnforcing(t *testing.T) *capacityEnv {
	t.Helper()
	return startCapacity(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
}

func TestCapacityViewerReadsButCannotWrite(t *testing.T) {
	t.Parallel()
	e := startCapacityEnforcing(t)
	ctx := t.Context()
	h100 := e.groupByPlatform(t, capH100Platform).GetMetadata().GetId()
	h200 := e.groupByPlatform(t, capH200Platform).GetMetadata().GetId()
	aid := e.createAllowance(t, h100, capLimit(4)).GetMetadata().GetId()
	v := e.capacityAs(t, config.DefaultViewerToken)

	// Reads work, including intervals and resource advice.
	if _, err := v.groups.Get(ctx, &capacitypb.GetCapacityBlockGroupRequest{Id: h100}); err != nil {
		t.Errorf("block group Get: %v", err)
	}
	if l, err := v.groups.List(ctx, &capacitypb.ListCapacityBlockGroupsRequest{ParentId: e.inst.TenantID}); err != nil || len(l.GetItems()) != 3 {
		t.Errorf("block group List = %v, %v", l, err)
	}
	if _, err := v.groups.GetByResourceAffinity(ctx, &capacitypb.GetCapacityBlockGroupByResourceAffinityRequest{
		ParentId: e.inst.TenantID, Region: "eu-north1", ResourceAffinity: affinity(capH200Platform, ""),
	}); err != nil {
		t.Errorf("block group GetByResourceAffinity: %v", err)
	}
	if _, err := v.groups.ListResources(ctx, &capacitypb.ListCapacityBlockGroupResourcesRequest{Id: h100}); err != nil {
		t.Errorf("block group ListResources: %v", err)
	}
	ivs, err := v.intervals.List(ctx, &capacitypb.ListCapacityIntervalsRequest{ParentId: h100})
	if err != nil || len(ivs.GetItems()) != 2 {
		t.Fatalf("interval List = %v, %v", ivs, err)
	}
	if _, err := v.intervals.Get(ctx, &capacitypb.GetCapacityIntervalRequest{Id: ivs.GetItems()[0].GetMetadata().GetId()}); err != nil {
		t.Errorf("interval Get: %v", err)
	}
	if l, err := v.advice.List(ctx, &capacitypb.ListResourceAdviceRequest{ParentId: e.inst.TenantID}); err != nil || len(l.GetItems()) != 6 {
		t.Errorf("advice List = %v, %v", l, err)
	}
	if _, err := v.allowances.Get(ctx, &capacitypb.GetCapacityAllowanceRequest{Id: aid}); err != nil {
		t.Errorf("allowance Get: %v", err)
	}
	if _, err := v.allowances.GetByParentAndCapacityBlockGroup(ctx, &capacitypb.GetCapacityAllowanceByParentAndCapacityBlockGroupRequest{
		ParentId: e.inst.ProjectID, CapacityBlockGroupId: h100,
	}); err != nil {
		t.Errorf("allowance GetByParent: %v", err)
	}
	if l, err := v.allowances.List(ctx, &capacitypb.ListCapacityAllowancesRequest{ParentId: e.inst.ProjectID}); err != nil || len(l.GetItems()) != 3 {
		t.Errorf("allowance List = %v, %v", l, err)
	}
	if l, err := v.allowances.ListByCapacityBlockGroup(ctx, &capacitypb.ListCapacityAllowancesByCapacityBlockGroupRequest{CapacityBlockGroupId: h100}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("allowance ListByCapacityBlockGroup = %v, %v", l, err)
	}

	// Writes do not.
	op, err := v.allowances.Create(ctx, &capacitypb.CreateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: h200, Limit: capLimit(1)},
	})
	capWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = v.allowances.Update(ctx, &capacitypb.UpdateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: aid},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: h100, Limit: capLimit(9)},
	})
	capWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = v.allowances.Delete(ctx, &capacitypb.DeleteCapacityAllowanceRequest{Id: aid})
	capWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	if got := e.getAllowance(t, aid); got.GetSpec().GetLimit() != 4 {
		t.Errorf("limit = %d after denied writes, want 4", got.GetSpec().GetLimit())
	}
}

func TestCapacityEditorAndAdminWrite(t *testing.T) {
	t.Parallel()
	e := startCapacityEnforcing(t)
	ctx := t.Context()
	h100 := e.groupByPlatform(t, capH100Platform).GetMetadata().GetId()
	h200 := e.groupByPlatform(t, capH200Platform).GetMetadata().GetId()

	for token, group := range map[string]string{config.DefaultEditorToken: h100, config.DefaultDevToken: h200} {
		c := e.capacityAs(t, token)
		op, err := c.allowances.Create(ctx, &capacitypb.CreateCapacityAllowanceRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID},
			Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: group, Limit: capLimit(2)},
		})
		kmsMustWait(t, op, err)
		id := op.ResourceID()
		op, err = c.allowances.Update(ctx, &capacitypb.UpdateCapacityAllowanceRequest{
			Metadata: &commonpb.ResourceMetadata{Id: id},
			Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: group, Limit: capLimit(3)},
		})
		kmsMustWait(t, op, err)
		if got := e.getAllowance(t, id); got.GetSpec().GetLimit() != 3 {
			t.Errorf("%s: limit = %d, want 3", token, got.GetSpec().GetLimit())
		}
		op, err = c.allowances.Delete(ctx, &capacitypb.DeleteCapacityAllowanceRequest{Id: id})
		kmsMustWait(t, op, err)
	}
}

func TestCapacityUnknownTokenRejected(t *testing.T) {
	t.Parallel()
	e := startCapacityEnforcing(t)
	c := e.capacityAs(t, "not-a-token")
	_, err := c.groups.List(t.Context(), &capacitypb.ListCapacityBlockGroupsRequest{ParentId: e.inst.TenantID})
	capWantCode(t, err, codes.Unauthenticated)
	_, err = c.advice.List(t.Context(), &capacitypb.ListResourceAdviceRequest{ParentId: e.inst.TenantID})
	capWantCode(t, err, codes.Unauthenticated)
}
