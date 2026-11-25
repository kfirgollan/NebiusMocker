package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

const (
	outsiderToken = "outsider-token"
	outsiderID    = "serviceaccount-e00mock00000000outsider"
)

type authzClients struct {
	inst                            *testkit.Instance
	admin, editor, viewer, outsider iamsvc.GroupService
	adminPermits                    iamsvc.AccessPermitService
	adminMembers, editorMembers     iamsvc.GroupMembershipService
}

func startEnforcing(t *testing.T, opts ...testkit.Option) authzClients {
	t.Helper()
	opts = append([]testkit.Option{
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{outsiderToken: outsiderID}),
	}, opts...)
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, opts...)
	admin := inst.SDKAs(t, config.DefaultDevToken)
	editor := inst.SDKAs(t, config.DefaultEditorToken)
	return authzClients{
		inst:          inst,
		admin:         iamsvc.NewGroupService(admin),
		editor:        iamsvc.NewGroupService(editor),
		viewer:        iamsvc.NewGroupService(inst.SDKAs(t, config.DefaultViewerToken)),
		outsider:      iamsvc.NewGroupService(inst.SDKAs(t, outsiderToken)),
		adminPermits:  iamsvc.NewAccessPermitService(admin),
		adminMembers:  iamsvc.NewGroupMembershipService(admin),
		editorMembers: iamsvc.NewGroupMembershipService(editor),
	}
}

func createReq(tenantID, name string) *iampb.CreateGroupRequest {
	return &iampb.CreateGroupRequest{Metadata: &commonpb.ResourceMetadata{ParentId: tenantID, Name: name}}
}

func TestRolesOnTenant(t *testing.T) {
	t.Parallel()
	c := startEnforcing(t)
	ctx := t.Context()
	g := createGroup(t, c.admin, c.inst.TenantID, "by-admin", nil)
	id := g.GetMetadata().GetId()

	// Viewer: read yes, write no.
	if _, err := c.viewer.Get(ctx, &iampb.GetGroupRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := c.viewer.List(ctx, &iampb.ListGroupsRequest{ParentId: c.inst.TenantID}); err != nil {
		t.Errorf("viewer List: %v", err)
	}
	_, err := c.viewer.Create(ctx, createReq(c.inst.TenantID, "by-viewer"))
	assertCode(t, err, codes.PermissionDenied)
	_, err = c.viewer.Delete(ctx, &iampb.DeleteGroupRequest{Id: id})
	assertCode(t, err, codes.PermissionDenied)

	// Editor: may manage groups…
	createGroup(t, c.editor, c.inst.TenantID, "by-editor", nil)
	// …but not grant access.
	_, err = c.editorMembers.Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: id},
		Spec:     &iampb.GroupMembershipSpec{MemberId: outsiderID},
	})
	assertCode(t, err, codes.PermissionDenied)
}

func TestOutsiderSeesNothing(t *testing.T) {
	t.Parallel()
	c := startEnforcing(t)
	g := createGroup(t, c.admin, c.inst.TenantID, "secret", nil)

	// Denied reads look like NOT_FOUND: existence is not leaked.
	_, err := c.outsider.Get(t.Context(), &iampb.GetGroupRequest{Id: g.GetMetadata().GetId()})
	assertCode(t, err, codes.NotFound)
	_, err = c.outsider.Create(t.Context(), createReq(c.inst.TenantID, "x"))
	assertCode(t, err, codes.PermissionDenied)
}

// TestGrantThroughIAM grants the outsider editor on a single group via a
// membership plus an access permit, then revokes it.
func TestGrantThroughIAM(t *testing.T) {
	t.Parallel()
	c := startEnforcing(t)
	ctx := t.Context()
	target := createGroup(t, c.admin, c.inst.TenantID, "target", nil)
	targetID := target.GetMetadata().GetId()
	deployers := createGroup(t, c.admin, c.inst.TenantID, "deployers", nil)

	op, err := c.adminMembers.Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: deployers.GetMetadata().GetId()},
		Spec:     &iampb.GroupMembershipSpec{MemberId: outsiderID},
	})
	if err != nil {
		t.Fatalf("create membership: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("membership wait: %v", err)
	}
	op, err = c.adminPermits.Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: deployers.GetMetadata().GetId()},
		Spec:     &iampb.AccessPermitSpec{ResourceId: targetID, Role: authz.RoleEditor},
	})
	if err != nil {
		t.Fatalf("create permit: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("permit wait: %v", err)
	}
	permitID := op.ResourceID()

	// The grant covers the target group only.
	if _, err := c.outsider.Get(ctx, &iampb.GetGroupRequest{Id: targetID}); err != nil {
		t.Fatalf("outsider Get on granted group: %v", err)
	}
	_, err = c.outsider.Get(ctx, &iampb.GetGroupRequest{Id: deployers.GetMetadata().GetId()})
	assertCode(t, err, codes.NotFound)
	_, err = c.outsider.Create(ctx, createReq(c.inst.TenantID, "x"))
	assertCode(t, err, codes.PermissionDenied)

	// Member counts reflect the membership.
	d, err := c.admin.Get(ctx, &iampb.GetGroupRequest{Id: deployers.GetMetadata().GetId()})
	if err != nil || d.GetStatus().GetServiceAccountsCount() != 1 {
		t.Fatalf("deployers status = %v, %v; want 1 service account", d.GetStatus(), err)
	}

	// Revoking the permit removes access.
	op, err = c.adminPermits.Delete(ctx, &iampb.DeleteAccessPermitRequest{Id: permitID})
	if err != nil {
		t.Fatalf("delete permit: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("delete permit wait: %v", err)
	}
	_, err = c.outsider.Get(ctx, &iampb.GetGroupRequest{Id: targetID})
	assertCode(t, err, codes.NotFound)
}

func TestPollingOwnDeleteOperation(t *testing.T) {
	t.Parallel()
	// With a delay the creator polls the operation while — and after — its
	// resource is deleted; the creator rule must keep allowing it.
	c := startEnforcing(t, testkit.WithOperationDelay(50*time.Millisecond))
	g := createGroup(t, c.editor, c.inst.TenantID, "short-lived", nil)
	op, err := c.editor.Delete(t.Context(), &iampb.DeleteGroupRequest{Id: g.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Wait on own delete operation: %v", err)
	}
}

func TestAuditModeLogsButAllows(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthz(authz.ModeAudit))
	viewer := iamsvc.NewGroupService(inst.SDKAs(t, config.DefaultViewerToken))
	if _, err := viewer.Create(t.Context(), createReq(inst.TenantID, "allowed-in-audit")); err != nil {
		t.Fatalf("viewer Create in audit mode: %v", err)
	}
}

func TestAnonymousUnderEnforce(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthz(authz.ModeEnforce))
	anon := iamsvc.NewGroupService(inst.SDKAs(t, ""))
	_, err := anon.List(t.Context(), &iampb.ListGroupsRequest{ParentId: inst.TenantID})
	assertCode(t, err, codes.Unauthenticated)
}
