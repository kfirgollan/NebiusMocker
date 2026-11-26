package contract

import (
	"slices"
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// nvlSpec is a GB200 (or GB300) instance spec in the given NVL group.
func (c instClients) nvlSpec(name, platform, groupID string) *computepb.InstanceSpec {
	s := c.gpuSpec(name)
	s.Resources = preset(platform, "4gpu-128vcpu-1600gb")
	s.NvlInstanceGroupId = groupID
	s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_ALWAYS
	return s
}

func (c instClients) nvlGroup(t *testing.T, groups computesvc.NVLInstanceGroupService, name string, typ computepb.NVLInstanceGroupSpec_NVLInstanceGroupType, size int64) string {
	t.Helper()
	op, err := groups.Create(t.Context(), &computepb.CreateNVLInstanceGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: name},
		Spec:     &computepb.NVLInstanceGroupSpec{Type: typ, Size: size},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	return op.ResourceID()
}

func (c instClients) nvlMembers(t *testing.T, groupID string, size int64, token string) (ids []string, next string, err error) {
	t.Helper()
	var resp *computepb.ListInstancesResponse
	resp, err = c.instances.ListInstancesByNVLInstanceGroup(t.Context(), &computepb.ListInstancesByNVLInstanceGroupRequest{
		NvlInstanceGroupId: groupID, PageSize: size, PageToken: token,
	})
	if err != nil {
		return nil, "", err
	}
	for _, i := range resp.GetItems() {
		ids = append(ids, i.GetMetadata().GetId())
	}
	return ids, resp.GetNextPageToken(), nil
}

func TestInstanceNVLGroup(t *testing.T) {
	t.Parallel()
	c := startInstances(t)
	ctx := t.Context()
	groups := computesvc.NewNVLInstanceGroupService(c.env.Inst.SDK(t))
	groupID := c.nvlGroup(t, groups, "gb200", computepb.NVLInstanceGroupSpec_GB200, 2)
	members := func() map[string]*computepb.NVLInstanceGroupStatus_InstanceInfo {
		g, err := groups.Get(ctx, &computepb.GetNVLInstanceGroupRequest{Id: groupID})
		if err != nil {
			t.Fatal(err)
		}
		return g.GetStatus().GetInstances()
	}

	spec := c.nvlSpec("n1", "gpu-gb200", groupID)
	first := c.mustCreate(t, "n1", spec)
	id1 := first.GetMetadata().GetId()
	protoEqual(t, "spec echo", first.GetSpec(), spec)
	if first.GetSpec().GetNvlInstanceGroupId() != groupID || first.GetSpec().GetRecoveryPolicy() != computepb.InstanceRecoveryPolicy_ALWAYS {
		t.Fatalf("spec = %v", first.GetSpec())
	}
	if m := members(); len(m) != 1 || m[id1].GetInstanceState() != computepb.InstanceStatus_RUNNING {
		t.Fatalf("group instances = %v, want %s RUNNING", m, id1)
	}

	second := c.mustCreate(t, "n2", c.nvlSpec("n2", "gpu-gb200", groupID))
	id2 := second.GetMetadata().GetId()

	// ListInstancesByNVLInstanceGroup, paged in creation order.
	page1, next, err := c.nvlMembers(t, groupID, 1, "")
	if err != nil || !slices.Equal(page1, []string{id1}) || next == "" {
		t.Fatalf("page 1 = %v, next %q, err %v", page1, next, err)
	}
	page2, next, err := c.nvlMembers(t, groupID, 1, next)
	if err != nil || !slices.Equal(page2, []string{id2}) || next != "" {
		t.Fatalf("page 2 = %v, next %q, err %v", page2, next, err)
	}
	_, _, err = c.nvlMembers(t, "computenvlinstancegroup-e00doesnotexist", 0, "")
	wantCode(t, "list unknown group", err, codes.NotFound, "")

	// The group is full, and a failed create leaves nothing behind.
	_, err = c.create(ctx, "n3", c.nvlSpec("n3", "gpu-gb200", groupID))
	wantCode(t, "third instance in a size-2 group", err, codes.FailedPrecondition, "")
	_, err = c.instances.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "n3"})
	wantCode(t, "GetByName n3 after a failed create", err, codes.NotFound, "")
	if m := members(); len(m) != 2 {
		t.Errorf("group instances = %v, want 2", m)
	}

	// Platform and recovery policy checks.
	_, err = c.create(ctx, "wrong", c.nvlSpec("wrong", "gpu-gb300", groupID))
	wantCode(t, "gb300 instance in a GB200 group", err, codes.InvalidArgument, "nvl_instance_group_id")
	cpu := c.cpuSpec("cpu")
	cpu.RecoveryPolicy = computepb.InstanceRecoveryPolicy_ALWAYS
	_, err = c.create(ctx, "cpu", cpu)
	wantCode(t, "recovery ALWAYS without a group", err, codes.InvalidArgument, "recovery_policy")

	// The group cannot change on update.
	other := c.nvlGroup(t, groups, "other", computepb.NVLInstanceGroupSpec_GB200, 2)
	err = c.update(ctx, t, c.get(t, id1), func(s *computepb.InstanceSpec) { s.NvlInstanceGroupId = other })
	wantCode(t, "change nvl_instance_group_id", err, codes.InvalidArgument, "nvl_instance_group_id")

	// A group with members cannot be deleted.
	delOp, err := groups.Delete(ctx, &computepb.DeleteNVLInstanceGroupRequest{Id: groupID})
	if err == nil {
		_, err = delOp.Wait(ctx)
	}
	wantCode(t, "delete group with members", err, codes.FailedPrecondition, "")

	c.mustDelete(t, id1)
	c.mustDelete(t, id2)
	if m := members(); len(m) != 0 {
		t.Errorf("group instances after delete = %v", m)
	}
	delOp, err = groups.Delete(ctx, &computepb.DeleteNVLInstanceGroupRequest{Id: groupID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delOp.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = groups.Get(ctx, &computepb.GetNVLInstanceGroupRequest{Id: groupID})
	wantCode(t, "get deleted group", err, codes.NotFound, "")
}

func TestInstanceNVLGroupAuthz(t *testing.T) {
	t.Parallel()
	c := startInstances(t, testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{platformOutsiderToken: platformOutsiderID}))
	viewer := instClientsFor(t, c.env, config.DefaultViewerToken)
	outsider := instClientsFor(t, c.env, platformOutsiderToken)
	groups := computesvc.NewNVLInstanceGroupService(c.env.Inst.SDK(t))
	groupID := c.nvlGroup(t, groups, "authz", computepb.NVLInstanceGroupSpec_GB200, 2)
	inst := c.mustCreate(t, "n1", c.nvlSpec("n1", "gpu-gb200", groupID))

	ids, _, err := viewer.nvlMembers(t, groupID, 0, "")
	if err != nil || !slices.Equal(ids, []string{inst.GetMetadata().GetId()}) {
		t.Fatalf("viewer list = %v, %v", ids, err)
	}
	_, _, err = outsider.nvlMembers(t, groupID, 0, "")
	wantCode(t, "outsider list", err, codes.PermissionDenied, "")
}
