package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
)

const capMissingGroup = "capacityblockgroup-e00doesnotexist"

func TestCapacityAllowanceLifecycle(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	ctx := t.Context()
	h100 := e.groupByPlatform(t, capH100Platform).GetMetadata().GetId()
	h200 := e.groupByPlatform(t, capH200Platform).GetMetadata().GetId()
	project := e.inst.ProjectID
	byParent := func(group string) (*capacitypb.CapacityAllowance, error) {
		return e.allowances.GetByParentAndCapacityBlockGroup(ctx, &capacitypb.GetCapacityAllowanceByParentAndCapacityBlockGroupRequest{
			ParentId: project, CapacityBlockGroupId: group,
		})
	}

	// Without an allowance the project gets the default one: no ID, no limit.
	def, err := byParent(h100)
	if err != nil {
		t.Fatalf("GetByParent (default): %v", err)
	}
	if def.GetMetadata().GetId() != "" || def.Spec.Limit != nil || def.GetSpec().GetCapacityBlockGroupId() != h100 {
		t.Errorf("default allowance = %v, want empty ID and no limit", def)
	}

	// Create with limit 4.
	created := e.createAllowance(t, h100, capLimit(4))
	id := created.GetMetadata().GetId()
	if id == "" || created.GetMetadata().GetParentId() != project || created.GetMetadata().GetResourceVersion() != 1 {
		t.Fatalf("created metadata = %v", created.GetMetadata())
	}
	if created.GetSpec().GetCapacityBlockGroupId() != h100 || created.Spec.Limit == nil || created.GetSpec().GetLimit() != 4 {
		t.Errorf("created spec = %v", created.GetSpec())
	}
	if st := created.GetStatus(); st.GetState() != capacitypb.CapacityAllowanceStatus_STATE_ACTIVE || st.GetUnit() != "GPU" ||
		st.GetUsage() != 0 || st.GetUsageState() != capacitypb.CapacityAllowanceStatus_USAGE_STATE_NOT_USED {
		t.Errorf("created status = %v", st)
	}

	if got, err := byParent(h100); err != nil || got.GetMetadata().GetId() != id {
		t.Errorf("GetByParent = %v, %v, want %s", got, err, id)
	}

	// List has one entry per block group of the tenant: the created one and
	// defaults for the others.
	list, err := e.allowances.List(ctx, &capacitypb.ListCapacityAllowancesRequest{ParentId: project})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.GetItems()) != 3 {
		t.Fatalf("List returned %d, want 3", len(list.GetItems()))
	}
	for _, a := range list.GetItems() {
		isH100 := a.GetSpec().GetCapacityBlockGroupId() == h100
		if isH100 != (a.GetMetadata().GetId() == id) || isH100 != (a.Spec.Limit != nil) {
			t.Errorf("List item = %v (h100: %v)", a, isH100)
		}
	}

	byBG, err := e.allowances.ListByCapacityBlockGroup(ctx, &capacitypb.ListCapacityAllowancesByCapacityBlockGroupRequest{CapacityBlockGroupId: h100})
	if err != nil || len(byBG.GetItems()) != 1 || byBG.GetItems()[0].GetMetadata().GetId() != id {
		t.Errorf("ListByCapacityBlockGroup(h100) = %v, %v", byBG, err)
	}
	byBG, err = e.allowances.ListByCapacityBlockGroup(ctx, &capacitypb.ListCapacityAllowancesByCapacityBlockGroupRequest{CapacityBlockGroupId: h200})
	if err != nil || len(byBG.GetItems()) != 0 {
		t.Errorf("ListByCapacityBlockGroup(h200) = %v, %v, want none", byBG, err)
	}

	// Update the limit and labels: the resource version bumps.
	labels := map[string]string{"team": "ml"}
	op, err := e.allowances.Update(ctx, &capacitypb.UpdateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ResourceVersion: 1, Labels: labels},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: h100, Limit: capLimit(8)},
	})
	mustOp(t, op, err)
	got := e.getAllowance(t, id)
	if got.GetSpec().GetLimit() != 8 || got.GetMetadata().GetLabels()["team"] != "ml" || got.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("after update = %v", got)
	}

	// A limit above the group's size is accepted (the group has 16 GPUs).
	op, err = e.allowances.Update(ctx, &capacitypb.UpdateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: labels},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: h100, Limit: capLimit(1000)},
	})
	mustOp(t, op, err)
	if got = e.getAllowance(t, id); got.GetSpec().GetLimit() != 1000 || got.GetMetadata().GetResourceVersion() != 3 {
		t.Errorf("after raising above the group size = %v", got)
	}

	// X-ResetMask spec.limit clears the limit (unlimited).
	op, err = e.allowances.Update(resetCtx(ctx, t, "spec.limit"), &capacitypb.UpdateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: labels},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: h100},
	})
	mustOp(t, op, err)
	if got = e.getAllowance(t, id); got.Spec.Limit != nil || got.GetMetadata().GetResourceVersion() != 4 {
		t.Errorf("after reset of spec.limit = %v, want no limit and version 4", got)
	}

	// Move to another block group.
	op, err = e.allowances.Update(ctx, &capacitypb.UpdateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: labels},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: h200, Limit: capLimit(2)},
	})
	mustOp(t, op, err)
	if got = e.getAllowance(t, id); got.GetSpec().GetCapacityBlockGroupId() != h200 || got.GetSpec().GetLimit() != 2 {
		t.Errorf("after move = %v", got)
	}
	if old, err := byParent(h100); err != nil || old.GetMetadata().GetId() != "" {
		t.Errorf("old group GetByParent = %v, %v, want the default", old, err)
	}

	// Delete: the default (empty ID) comes back.
	op, err = e.allowances.Delete(ctx, &capacitypb.DeleteCapacityAllowanceRequest{Id: id})
	mustOp(t, op, err)
	_, err = e.allowances.Get(ctx, &capacitypb.GetCapacityAllowanceRequest{Id: id})
	capWantCode(t, err, codes.NotFound)
	if after, err := byParent(h200); err != nil || after.GetMetadata().GetId() != "" || after.Spec.Limit != nil {
		t.Errorf("after delete GetByParent = %v, %v, want the default", after, err)
	}
}

func (e *capacityEnv) getAllowance(t *testing.T, id string) *capacitypb.CapacityAllowance {
	t.Helper()
	a, err := e.allowances.Get(t.Context(), &capacitypb.GetCapacityAllowanceRequest{Id: id})
	if err != nil {
		t.Fatalf("get allowance %s: %v", id, err)
	}
	return a
}

func TestCapacityAllowanceUnlimited(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	a := e.createAllowance(t, e.groupByPlatform(t, capH200Platform).GetMetadata().GetId(), nil)
	if a.Spec.Limit != nil {
		t.Errorf("limit = %d, want unset (unlimited)", a.GetSpec().GetLimit())
	}
}

func TestCapacityAllowanceErrors(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	ctx := t.Context()
	h100 := e.groupByPlatform(t, capH100Platform).GetMetadata().GetId()
	h200 := e.groupByPlatform(t, capH200Platform).GetMetadata().GetId()
	id := e.createAllowance(t, h100, capLimit(4)).GetMetadata().GetId()

	create := func(parent, group string) error {
		op, err := e.allowances.Create(ctx, &capacitypb.CreateCapacityAllowanceRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: parent},
			Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: group, Limit: capLimit(1)},
		})
		return kmsWait(ctx, op, err)
	}
	// One allowance per (project, group).
	capWantCode(t, create(e.inst.ProjectID, h100), codes.AlreadyExists)
	capWantCode(t, create(e.inst.ProjectID, capMissingGroup), codes.NotFound)
	// The parent must be a project.
	capWantCode(t, create(e.inst.TenantID, h200), codes.InvalidArgument)

	_, err := e.allowances.Get(ctx, &capacitypb.GetCapacityAllowanceRequest{Id: "capacityallowance-e00doesnotexist"})
	capWantCode(t, err, codes.NotFound)
	op, err := e.allowances.Delete(ctx, &capacitypb.DeleteCapacityAllowanceRequest{Id: "capacityallowance-e00doesnotexist"})
	capWantCode(t, kmsWait(ctx, op, err), codes.NotFound)
	_, err = e.allowances.GetByParentAndCapacityBlockGroup(ctx, &capacitypb.GetCapacityAllowanceByParentAndCapacityBlockGroupRequest{
		ParentId: e.inst.ProjectID, CapacityBlockGroupId: capMissingGroup,
	})
	capWantCode(t, err, codes.NotFound)
	_, err = e.allowances.ListByCapacityBlockGroup(ctx, &capacitypb.ListCapacityAllowancesByCapacityBlockGroupRequest{CapacityBlockGroupId: capMissingGroup})
	capWantCode(t, err, codes.NotFound)
	_, err = e.allowances.List(ctx, &capacitypb.ListCapacityAllowancesRequest{ParentId: e.inst.ProjectID, PageSize: -1})
	capWantCode(t, err, codes.InvalidArgument)

	// A stale resource_version is a failed precondition.
	op, err = e.allowances.Update(ctx, &capacitypb.UpdateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ResourceVersion: 99},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: h100, Limit: capLimit(5)},
	})
	capWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)
	if got := e.getAllowance(t, id); got.GetSpec().GetLimit() != 4 {
		t.Errorf("limit = %d after a refused update, want 4", got.GetSpec().GetLimit())
	}

	// Moving onto a group where the project already has an allowance.
	other := e.createAllowance(t, h200, capLimit(2)).GetMetadata().GetId()
	op, err = e.allowances.Update(ctx, &capacitypb.UpdateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: other},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: h100, Limit: capLimit(2)},
	})
	capWantCode(t, kmsWait(ctx, op, err), codes.AlreadyExists)
	// Moving onto an unknown group.
	op, err = e.allowances.Update(ctx, &capacitypb.UpdateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: other},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: capMissingGroup},
	})
	capWantCode(t, kmsWait(ctx, op, err), codes.NotFound)
}
