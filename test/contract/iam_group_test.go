// Package contract verifies NebiusMocker's wire behavior with the official
// Nebius Go SDK over a real gRPC connection.
package contract

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

func startIAM(t *testing.T, opts ...testkit.Option) (*testkit.Instance, iamsvc.GroupService) {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, opts...)
	return inst, iamsvc.NewGroupService(inst.SDK(t))
}

func createGroup(t *testing.T, svc iamsvc.GroupService, parentID, name string, labels map[string]string) *iampb.Group {
	t.Helper()
	op, err := svc.Create(t.Context(), &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parentID, Name: name, Labels: labels},
		Spec:     &iampb.GroupSpec{},
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if op.ResourceID() == "" {
		t.Fatalf("Create(%q): operation has empty resource_id", name)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create(%q) wait: %v", name, err)
	}
	g, err := svc.Get(t.Context(), &iampb.GetGroupRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatalf("Get(%s): %v", op.ResourceID(), err)
	}
	return g
}

func TestGroupLifecycle(t *testing.T) {
	t.Parallel()
	inst, svc := startIAM(t, testkit.WithOperationDelay(50*time.Millisecond))
	ctx := t.Context()

	g := createGroup(t, svc, inst.TenantID, "admins", map[string]string{"team": "core"})
	if got, want := g.GetMetadata().GetResourceVersion(), int64(1); got != want {
		t.Errorf("resource_version = %d, want %d", got, want)
	}
	if got := g.GetStatus().GetState(); got != iampb.GroupStatus_ACTIVE {
		t.Errorf("status.state = %v, want ACTIVE", got)
	}

	byName, err := svc.GetByName(ctx, &iampb.GetGroupByNameRequest{ParentId: inst.TenantID, Name: "admins"})
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if diff := cmp.Diff(g, byName, protocmp.Transform()); diff != "" {
		t.Errorf("GetByName differs from Get (-get +byName):\n%s", diff)
	}

	// Update: rename and clear labels with an explicit reset mask.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatalf("add reset mask: %v", err)
	}
	op, err := svc.Update(maskCtx,
		&iampb.UpdateGroupRequest{Metadata: &commonpb.ResourceMetadata{Id: g.GetMetadata().GetId(), ParentId: inst.TenantID, Name: "platform-admins"}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Update wait: %v", err)
	}
	updated, err := svc.Get(ctx, &iampb.GetGroupRequest{Id: g.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got := updated.GetMetadata().GetName(); got != "platform-admins" {
		t.Errorf("name = %q, want platform-admins", got)
	}
	if got := updated.GetMetadata().GetLabels(); len(got) != 0 {
		t.Errorf("labels = %v, want cleared by reset mask", got)
	}
	if got := updated.GetMetadata().GetResourceVersion(); got != 2 {
		t.Errorf("resource_version = %d, want 2", got)
	}

	op, err = svc.Delete(ctx, &iampb.DeleteGroupRequest{Id: g.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Delete wait: %v", err)
	}
	_, err = svc.Get(ctx, &iampb.GetGroupRequest{Id: g.GetMetadata().GetId()})
	assertCode(t, err, codes.NotFound)
}

func TestGroupErrors(t *testing.T) {
	t.Parallel()
	inst, svc := startIAM(t)
	ctx := t.Context()
	createGroup(t, svc, inst.TenantID, "dup", nil)

	tests := []struct {
		name string
		req  *iampb.CreateGroupRequest
		want codes.Code
	}{
		{"duplicate name", &iampb.CreateGroupRequest{Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: "dup"}}, codes.AlreadyExists},
		{"unknown parent", &iampb.CreateGroupRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "tenant-e00missing00000000", Name: "x"}}, codes.NotFound},
		{"missing parent", &iampb.CreateGroupRequest{Metadata: &commonpb.ResourceMetadata{Name: "x"}}, codes.InvalidArgument},
		{"client-set id", &iampb.CreateGroupRequest{Metadata: &commonpb.ResourceMetadata{Id: "group-e00x00000000000", ParentId: inst.TenantID}}, codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := svc.Create(ctx, tt.req)
			assertCode(t, err, tt.want)
		})
	}
}

func TestGroupListPaging(t *testing.T) {
	t.Parallel()
	inst, svc := startIAM(t)
	want := map[string]bool{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		createGroup(t, svc, inst.TenantID, n, nil)
		want[n] = true
	}
	got := map[string]bool{}
	token := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		resp, err := svc.List(t.Context(), &iampb.ListGroupsRequest{ParentId: inst.TenantID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, g := range resp.GetItems() {
			if got[g.GetMetadata().GetName()] {
				t.Errorf("group %q returned twice", g.GetMetadata().GetName())
			}
			got[g.GetMetadata().GetName()] = true
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("listed groups (-want +got):\n%s", diff)
	}
}

func TestOperationConflict(t *testing.T) {
	t.Parallel()
	inst, svc := startIAM(t, testkit.WithOperationDelay(time.Hour))
	op, err := svc.Create(t.Context(), &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: "slow"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if op.Done() {
		t.Fatal("operation finished immediately, want running")
	}
	_, err = svc.Delete(t.Context(), &iampb.DeleteGroupRequest{Id: op.ResourceID()})
	assertCode(t, err, codes.FailedPrecondition)
}

func TestUnauthenticated(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthnRequired())
	inst.Token = "not-a-valid-token"
	svc := iamsvc.NewGroupService(inst.SDK(t))
	_, err := svc.List(t.Context(), &iampb.ListGroupsRequest{ParentId: inst.TenantID})
	assertCode(t, err, codes.Unauthenticated)
}

func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v, want %v (err: %v)", got, want, err)
	}
}
