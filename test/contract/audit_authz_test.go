package contract

import (
	"testing"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	auditsvc "github.com/nebius/gosdk/services/nebius/audit/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

const (
	auditorToken     = "tok-auditor"
	auditorPrincipal = "tenantuseraccount-e00auditor"
)

// auditClients are the audit services as seen by one caller.
type auditClients struct {
	events  auditsvc.AuditEventService
	exports auditsvc.AuditEventExportService
	groups  iamsvc.GroupService
}

func (e *auditEnv) as(t *testing.T, token string) auditClients {
	t.Helper()
	sdk := e.inst.SDKAs(t, token)
	return auditClients{
		events:  auditsvc.NewAuditEventService(sdk),
		exports: auditsvc.NewAuditEventExportService(sdk),
		groups:  iamsvc.NewGroupService(sdk),
	}
}

func startAuditEnforcing(t *testing.T) *auditEnv {
	t.Helper()
	env := startStorageAudit(t, memory.New(clock.Real{}),
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{auditorToken: auditorPrincipal}),
	)
	env.addExportBucket(t)
	return env
}

// grantAuditor makes the auditor principal a member of a new group and grants
// that group the auditor role on the tenant, all as the admin. It returns the group.
func grantAuditor(t *testing.T, env *auditEnv) *iampb.Group {
	t.Helper()
	ctx := t.Context()
	sdk := env.inst.SDK(t)
	group := createGroup(t, env.groups, env.inst.TenantID, "auditors", nil)

	op, err := iamsvc.NewGroupMembershipService(sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group.GetMetadata().GetId()},
		Spec:     &iampb.GroupMembershipSpec{MemberId: auditorPrincipal},
	})
	if err != nil {
		t.Fatalf("create membership: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("membership wait: %v", err)
	}
	op, err = iamsvc.NewAccessPermitService(sdk).Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group.GetMetadata().GetId()},
		Spec:     &iampb.AccessPermitSpec{ResourceId: env.inst.TenantID, Role: authz.RoleAuditor},
	})
	if err != nil {
		t.Fatalf("create access permit: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("access permit wait: %v", err)
	}
	return group
}

// grantRoleOn grants group the role on resourceID, as the admin.
func grantRoleOn(t *testing.T, env *auditEnv, group *iampb.Group, resourceID, role string) {
	t.Helper()
	op, err := iamsvc.NewAccessPermitService(env.inst.SDK(t)).Create(t.Context(), &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group.GetMetadata().GetId()},
		Spec:     &iampb.AccessPermitSpec{ResourceId: resourceID, Role: role},
	})
	if err != nil {
		t.Fatalf("grant %s on %s: %v", role, resourceID, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("grant %s on %s wait: %v", role, resourceID, err)
	}
}

func TestAuditAdminAllowed(t *testing.T) {
	t.Parallel()
	env := startAuditEnforcing(t)
	admin := env.as(t, config.DefaultDevToken)
	if _, err := admin.events.List(t.Context(), auditListReq(env.inst.TenantID, "")); err != nil {
		t.Errorf("admin List events: %v", err)
	}
	op, err := admin.exports.Start(t.Context(), env.startReq("", ""))
	if err != nil {
		t.Fatalf("admin Start export: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Errorf("admin Start export wait: %v", err)
	}
}

// TestAuditViewerAndEditorDenied checks that only admin and auditor read the
// audit log. A denied List is PERMISSION_DENIED, not NOT_FOUND: the authorize
// interceptor hides existence (NOT_FOUND) only for the "get" verb, and List
// carries the "list" verb. Get of an export is a "get", so it stays NOT_FOUND.
func TestAuditViewerAndEditorDenied(t *testing.T) {
	t.Parallel()
	env := startAuditEnforcing(t)
	exportID := env.startExport(t, env.startReq("", ""))

	for name, token := range map[string]string{"viewer": config.DefaultViewerToken, "editor": config.DefaultEditorToken} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := env.as(t, token)
			ctx := t.Context()

			_, err := c.events.List(ctx, auditListReq(env.inst.TenantID, ""))
			assertCode(t, err, codes.PermissionDenied)
			_, err = c.exports.Start(ctx, env.startReq("", ""))
			assertCode(t, err, codes.PermissionDenied)
			_, err = c.exports.List(ctx, &auditv2pb.ListRequest{ParentId: env.inst.TenantID, Region: auditRegion})
			assertCode(t, err, codes.PermissionDenied)
			_, err = c.exports.Cancel(ctx, &auditv2pb.CancelRequest{Id: exportID})
			assertCode(t, err, codes.PermissionDenied)
			_, err = c.exports.Get(ctx, &auditv2pb.GetRequest{Id: exportID})
			assertCode(t, err, codes.NotFound)
		})
	}
}

func TestAuditAuditorRole(t *testing.T) {
	t.Parallel()
	env := startAuditEnforcing(t)
	ctx := t.Context()
	aud := env.as(t, auditorToken)

	// Before the grant the principal has nothing.
	_, err := aud.events.List(ctx, auditListReq(env.inst.TenantID, ""))
	assertCode(t, err, codes.PermissionDenied)

	grantAuditor(t, env)
	exportID := env.startExport(t, env.startReq("", "")) // started by the admin

	if _, err := aud.events.List(ctx, auditListReq(env.inst.TenantID, "")); err != nil {
		t.Errorf("auditor List events: %v", err)
	}
	// Audit rights alone do not let the auditor write into the bucket: that
	// takes storage.objects.create on it (the storage.uploader role).
	_, err = aud.exports.Start(ctx, env.startReq("", ""))
	assertCode(t, err, codes.PermissionDenied)

	if _, err := aud.exports.Get(ctx, &auditv2pb.GetRequest{Id: exportID}); err != nil {
		t.Errorf("auditor Get export: %v", err)
	}
	_, err = aud.exports.Cancel(ctx, &auditv2pb.CancelRequest{Id: exportID})
	assertCode(t, err, codes.FailedPrecondition) // allowed to try; the export is already DONE
	if _, err := aud.exports.List(ctx, &auditv2pb.ListRequest{ParentId: env.inst.TenantID, Region: auditRegion}); err != nil {
		t.Errorf("auditor List exports: %v", err)
	}

	// The auditor reads like a viewer but changes nothing outside audit.
	if _, err := aud.groups.List(ctx, &iampb.ListGroupsRequest{ParentId: env.inst.TenantID}); err != nil {
		t.Errorf("auditor List groups: %v", err)
	}
	_, err = aud.groups.Create(ctx, createReq(env.inst.TenantID, "by-auditor"))
	assertCode(t, err, codes.PermissionDenied)
}

// TestAuditExportNeedsBucketWrite checks that an auditor who is also allowed
// to create objects in the bucket can export into it.
func TestAuditExportNeedsBucketWrite(t *testing.T) {
	t.Parallel()
	env := startAuditEnforcing(t)
	ctx := t.Context()
	aud := env.as(t, auditorToken)
	group := grantAuditor(t, env)

	_, err := aud.exports.Start(ctx, env.startReq("", ""))
	assertCode(t, err, codes.PermissionDenied)

	grantRoleOn(t, env, group, env.bucketID, authz.RoleEditor)
	op, err := aud.exports.Start(ctx, env.startReq("", ""))
	if err != nil {
		t.Fatalf("auditor with bucket write Start export: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Start export wait: %v", err)
	}
	if got := env.getExport(t, op.ResourceID()).GetStatus().GetState(); got != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Errorf("state = %v, want DONE", got)
	}
}

// TestAuditDenialsAreAudited checks that an enforced denial leaves an ERROR
// event naming the caller.
func TestAuditDenialsAreAudited(t *testing.T) {
	t.Parallel()
	env := startAuditEnforcing(t)
	viewer := env.as(t, config.DefaultViewerToken)
	_, err := viewer.groups.Create(t.Context(), createReq(env.inst.TenantID, "denied-group"))
	assertCode(t, err, codes.PermissionDenied)

	evs := env.list(t, "status='ERROR' AND authentication.subject.tenant_user_id='"+config.DefaultViewerPrincipal+"'")
	var found *auditv2pb.AuditEvent
	for _, ev := range evs {
		if ev.GetAction() == "CREATE" {
			found = ev
		}
	}
	if found == nil {
		t.Fatalf("no CREATE ERROR event for the viewer; got %v", evs)
	}
	if got := found.GetResponse().GetStatusCode(); got != code.Code_PERMISSION_DENIED {
		t.Errorf("response.status_code = %v, want PERMISSION_DENIED", got)
	}
	if got := found.GetSource(); got != "nebius.iam.v1.GroupService/Create" {
		t.Errorf("source = %q, want nebius.iam.v1.GroupService/Create", got)
	}
	if got := found.GetStatus(); got != auditv2pb.Status_ERROR {
		t.Errorf("status = %v, want ERROR", got)
	}
	if got := found.GetAuthentication().GetSubject().GetName(); got != config.DefaultViewerPrincipal {
		t.Errorf("subject.name = %q, want %q", got, config.DefaultViewerPrincipal)
	}
	if got := found.GetResource().GetHierarchy(); len(got) == 0 || got[0].GetId() != env.inst.TenantID {
		t.Errorf("hierarchy = %v, want the tenant first", got)
	}
	// Nothing was created, so no group event of the successful kind exists.
	if got := env.list(t, "status='DONE' AND action='CREATE' AND resource.metadata.type='group'"); len(got) != 0 {
		t.Errorf("DONE group CREATE events = %d, want 0", len(got))
	}
}
