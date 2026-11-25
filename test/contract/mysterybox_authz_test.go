package contract

import (
	"bytes"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	mbsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
)

// mbPrincipal is one authenticated caller of the MysteryBox services.
type mbPrincipal struct {
	secrets  mbsvc.SecretService
	versions mbsvc.SecretVersionService
	payloads mbsvc.PayloadService
}

type mbAuthz struct {
	inst                            *testkit.Instance
	admin, editor, viewer, outsider mbPrincipal
	groups                          iamsvc.GroupService
	members                         iamsvc.GroupMembershipService
	permits                         iamsvc.AccessPermitService
}

func mbStartEnforcing(t *testing.T) mbAuthz {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()},
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{outsiderToken: outsiderID}),
	)
	as := func(token string) mbPrincipal {
		sdk := inst.SDKAs(t, token)
		return mbPrincipal{
			secrets:  mbsvc.NewSecretService(sdk),
			versions: mbsvc.NewSecretVersionService(sdk),
			payloads: mbsvc.NewPayloadService(sdk),
		}
	}
	adminSDK := inst.SDKAs(t, config.DefaultDevToken)
	return mbAuthz{
		inst:     inst,
		admin:    as(config.DefaultDevToken),
		editor:   as(config.DefaultEditorToken),
		viewer:   as(config.DefaultViewerToken),
		outsider: as(outsiderToken),
		groups:   iamsvc.NewGroupService(adminSDK),
		members:  iamsvc.NewGroupMembershipService(adminSDK),
		permits:  iamsvc.NewAccessPermitService(adminSDK),
	}
}

func TestMysteryBoxAuthzRoles(t *testing.T) {
	t.Parallel()
	a := mbStartEnforcing(t)
	ctx := t.Context()
	project := a.inst.ProjectID

	sid := mbCreateSecret(t, a.admin.secrets, project, "guarded", mbMixedPayload())
	vid := mbPrimary(t, a.admin.secrets, sid)

	// Viewer: metadata reads only.
	if _, err := a.viewer.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: sid}); err != nil {
		t.Errorf("viewer Get secret: %v", err)
	}
	if _, err := a.viewer.secrets.GetByName(ctx, &mysteryboxpb.GetSecretByNameRequest{ParentId: project, Name: "guarded"}); err != nil {
		t.Errorf("viewer GetByName: %v", err)
	}
	if _, err := a.viewer.secrets.List(ctx, &mysteryboxpb.ListSecretsRequest{ParentId: project}); err != nil {
		t.Errorf("viewer List secrets: %v", err)
	}
	if _, err := a.viewer.versions.Get(ctx, &mysteryboxpb.GetSecretVersionRequest{Id: vid}); err != nil {
		t.Errorf("viewer Get version: %v", err)
	}
	if _, err := a.viewer.versions.List(ctx, &mysteryboxpb.ListSecretVersionsRequest{ParentId: sid}); err != nil {
		t.Errorf("viewer List versions: %v", err)
	}
	_, err := a.viewer.secrets.Create(ctx, mbCreateReq(project, "by-viewer", mbMixedPayload()))
	assertCode(t, err, codes.PermissionDenied)
	_, err = a.viewer.versions.Create(ctx, &mysteryboxpb.CreateSecretVersionRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: sid},
		Spec:     &mysteryboxpb.SecretVersionSpec{Payload: mbMixedPayload()},
	})
	assertCode(t, err, codes.PermissionDenied)
	_, err = a.viewer.secrets.Delete(ctx, &mysteryboxpb.DeleteSecretRequest{Id: sid})
	assertCode(t, err, codes.PermissionDenied)

	// Editor: manages secrets and versions, never reads payloads.
	esid := mbCreateSecret(t, a.editor.secrets, project, "by-editor", mbMixedPayload())
	ev := mbCreateVersion(t, a.editor.versions, esid, []*mysteryboxpb.Payload{mbStr("k", "v")}, true)
	cur, err := a.editor.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: esid})
	if err != nil {
		t.Fatalf("editor Get: %v", err)
	}
	cur.Spec.Description = "edited"
	op, err := a.editor.secrets.Update(ctx, &mysteryboxpb.UpdateSecretRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()})
	mbDone(t, op, err)
	dop, err := a.editor.versions.Delete(ctx, &mysteryboxpb.DeleteSecretVersionRequest{Id: ev})
	mbDone(t, dop, err)
	uvop, err := a.editor.versions.Undelete(ctx, &mysteryboxpb.UndeleteSecretVersionRequest{Id: ev})
	mbDone(t, uvop, err)
	dsop, err := a.editor.secrets.Delete(ctx, &mysteryboxpb.DeleteSecretRequest{Id: esid})
	mbDone(t, dsop, err)
	usop, err := a.editor.secrets.Undelete(ctx, &mysteryboxpb.UndeleteSecretRequest{Id: esid})
	mbDone(t, usop, err)

	// Admin reads payloads.
	p, err := a.admin.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: sid})
	if err != nil || len(p.GetData()) != 2 {
		t.Errorf("admin payload Get = %v, %v", p, err)
	}
	e, err := a.admin.payloads.GetByKey(ctx, &mysteryboxpb.GetPayloadByKeyRequest{SecretId: sid, Key: "password"})
	if err != nil || e.GetData().GetStringValue() != mbSecretValue {
		t.Errorf("admin GetByKey = %v, %v", e, err)
	}

	// The operation of a payload-bearing create is no side channel for a viewer.
	mbNoSecret(t, "viewer-visible operation", op.Request())
}

func TestMysteryBoxAuthzOutsider(t *testing.T) {
	t.Parallel()
	a := mbStartEnforcing(t)
	ctx := t.Context()
	project := a.inst.ProjectID
	sid := mbCreateSecret(t, a.admin.secrets, project, "hidden", mbMixedPayload())
	vid := mbPrimary(t, a.admin.secrets, sid)

	_, err := a.outsider.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: sid})
	mbAssertDenied(t, err)
	_, err = a.outsider.versions.Get(ctx, &mysteryboxpb.GetSecretVersionRequest{Id: vid})
	mbAssertDenied(t, err)
	_, err = a.outsider.secrets.List(ctx, &mysteryboxpb.ListSecretsRequest{ParentId: project})
	mbAssertDenied(t, err)
	_, err = a.outsider.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: sid})
	mbAssertDenied(t, err)
	_, err = a.outsider.secrets.Create(ctx, mbCreateReq(project, "by-outsider", mbMixedPayload()))
	assertCode(t, err, codes.PermissionDenied)
}

func mbAssertDenied(t *testing.T, err error) {
	t.Helper()
	if c := status.Code(err); c != codes.PermissionDenied && c != codes.NotFound {
		t.Fatalf("code = %v, want PermissionDenied or NotFound (err: %v)", c, err)
	}
}

// TestMysteryBoxPayloadViewerRole grants the outsider the payload-viewer role
// on the project through a group, which unlocks payloads but not metadata.
func TestMysteryBoxPayloadViewerRole(t *testing.T) {
	t.Parallel()
	a := mbStartEnforcing(t)
	ctx := t.Context()
	project := a.inst.ProjectID
	sid := mbCreateSecret(t, a.admin.secrets, project, "payload-only", mbMixedPayload())

	_, err := a.outsider.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: sid})
	mbAssertDenied(t, err)

	gop, err := a.groups.Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: a.inst.TenantID, Name: "payload-readers"},
		Spec:     &iampb.GroupSpec{},
	})
	gid := mbDone(t, gop, err)
	mop, err := a.members.Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.GroupMembershipSpec{MemberId: outsiderID},
	})
	mbDone(t, mop, err)
	pop, err := a.permits.Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.AccessPermitSpec{ResourceId: project, Role: authz.RoleMysteryBoxPayloadViewer},
	})
	mbDone(t, pop, err)

	p, err := a.outsider.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: sid})
	if err != nil || len(p.GetData()) != 2 {
		t.Fatalf("payload-viewer Get = %v, %v", p, err)
	}
	e, err := a.outsider.payloads.GetByKey(ctx, &mysteryboxpb.GetPayloadByKeyRequest{SecretId: sid, Key: "blob"})
	if err != nil || !bytes.Equal(e.GetData().GetBinaryValue(), mbBinary) {
		t.Errorf("payload-viewer GetByKey = %v, %v", e, err)
	}

	// The role grants nothing else.
	_, err = a.outsider.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: sid})
	mbAssertDenied(t, err)
	_, err = a.outsider.secrets.Create(ctx, mbCreateReq(project, "by-payload-viewer", mbMixedPayload()))
	assertCode(t, err, codes.PermissionDenied)
}

// TestMysteryBoxPayloadDenied: callers who may see the secret but lack
// mysterybox.payload.read are told PermissionDenied, not NotFound.
func TestMysteryBoxPayloadDenied(t *testing.T) {
	t.Parallel()
	a := mbStartEnforcing(t)
	ctx := t.Context()
	sid := mbCreateSecret(t, a.admin.secrets, a.inst.ProjectID, "guarded", mbMixedPayload())
	for name, p := range map[string]mbPrincipal{"viewer": a.viewer, "editor": a.editor} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := p.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: sid})
			assertCode(t, err, codes.PermissionDenied)
			_, err = p.payloads.GetByKey(ctx, &mysteryboxpb.GetPayloadByKeyRequest{SecretId: sid, Key: "password"})
			assertCode(t, err, codes.PermissionDenied)
		})
	}
}
