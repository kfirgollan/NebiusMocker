package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// --- AccessKeyService *ByAwsId ---

func TestAccessKeyByAwsIDMutations(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	admin := inst.SDK(t)
	editor := iamv2svc.NewAccessKeyService(inst.SDKAs(t, config.DefaultEditorToken))
	viewer := iamv2svc.NewAccessKeyService(inst.SDKAs(t, config.DefaultViewerToken))
	keys := iamv2svc.NewAccessKeyService(admin)
	ctx := t.Context()

	sa := createServiceAccount(t, iamsvc.NewServiceAccountService(admin), inst.ProjectID, "uploader")
	key := createAccessKey(t, keys, inst.ProjectID, sa.GetMetadata().GetId(), iamv2pb.SecretDeliveryMode_INLINE)
	aws := key.GetStatus().GetAwsAccessKeyId()
	state := func() iamv2pb.AccessKeyStatus_State {
		t.Helper()
		k, err := keys.GetByAwsId(ctx, &iamv2pb.GetAccessKeyByAwsIdRequest{AwsAccessKeyId: aws})
		if err != nil {
			t.Fatalf("GetByAwsId: %v", err)
		}
		return k.GetStatus().GetState()
	}

	// Viewers may read but not mutate, whatever the key is addressed by.
	_, err := viewer.DeactivateByAwsId(ctx, &iamv2pb.DeactivateAccessKeyByAwsIdRequest{AwsAccessKeyId: aws})
	assertCode(t, err, codes.PermissionDenied)
	_, err = viewer.ActivateByAwsId(ctx, &iamv2pb.ActivateAccessKeyByAwsIdRequest{AwsAccessKeyId: aws})
	assertCode(t, err, codes.PermissionDenied)
	_, err = viewer.DeleteByAwsId(ctx, &iamv2pb.DeleteAccessKeyByAwsIdRequest{AwsAccessKeyId: aws})
	assertCode(t, err, codes.PermissionDenied)
	if got := state(); got != iamv2pb.AccessKeyStatus_ACTIVE {
		t.Fatalf("state after denied calls = %v, want ACTIVE", got)
	}

	// Editors may deactivate and reactivate.
	op, err := editor.DeactivateByAwsId(ctx, &iamv2pb.DeactivateAccessKeyByAwsIdRequest{AwsAccessKeyId: aws})
	if err != nil {
		t.Fatalf("DeactivateByAwsId: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("DeactivateByAwsId wait: %v", err)
	}
	if got := state(); got != iamv2pb.AccessKeyStatus_INACTIVE {
		t.Fatalf("state after deactivate = %v, want INACTIVE", got)
	}
	op, err = editor.ActivateByAwsId(ctx, &iamv2pb.ActivateAccessKeyByAwsIdRequest{AwsAccessKeyId: aws})
	if err != nil {
		t.Fatalf("ActivateByAwsId: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("ActivateByAwsId wait: %v", err)
	}
	if got := state(); got != iamv2pb.AccessKeyStatus_ACTIVE {
		t.Fatalf("state after activate = %v, want ACTIVE", got)
	}

	// Unknown and empty AWS IDs.
	_, err = keys.DeleteByAwsId(ctx, &iamv2pb.DeleteAccessKeyByAwsIdRequest{AwsAccessKeyId: "NMKUNKNOWN"})
	assertCode(t, err, codes.NotFound)
	_, err = keys.ActivateByAwsId(ctx, &iamv2pb.ActivateAccessKeyByAwsIdRequest{AwsAccessKeyId: "NMKUNKNOWN"})
	assertCode(t, err, codes.NotFound)
	_, err = keys.DeactivateByAwsId(ctx, &iamv2pb.DeactivateAccessKeyByAwsIdRequest{AwsAccessKeyId: "NMKUNKNOWN"})
	assertCode(t, err, codes.NotFound)

	// Delete removes the key by ID and by AWS ID lookups alike.
	op, err = keys.DeleteByAwsId(ctx, &iamv2pb.DeleteAccessKeyByAwsIdRequest{AwsAccessKeyId: aws})
	if err != nil {
		t.Fatalf("DeleteByAwsId: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("DeleteByAwsId wait: %v", err)
	}
	_, err = keys.Get(ctx, &iamv2pb.GetAccessKeyRequest{Id: key.GetMetadata().GetId()})
	assertCode(t, err, codes.NotFound)
	_, err = keys.GetByAwsId(ctx, &iamv2pb.GetAccessKeyByAwsIdRequest{AwsAccessKeyId: aws})
	assertCode(t, err, codes.NotFound)
}

// --- v2 TenantService.Update ---

func TestTenantV2Update(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	ctx := t.Context()
	tenant := env.inst.TenantID
	admin := iamv2svc.NewTenantService(env.admin)

	// Viewers and outsiders cannot update.
	req := &iamv2pb.UpdateTenantRequest{Metadata: &commonpb.ResourceMetadata{Id: tenant, Name: "acme"}}
	_, err := iamv2svc.NewTenantService(env.viewer).Update(ctx, req)
	assertCode(t, err, codes.PermissionDenied)
	_, err = iamv2svc.NewTenantService(env.outsider).Update(ctx, req)
	assertCode(t, err, codes.PermissionDenied)

	// name_prefix is rejected, not ignored.
	_, err = admin.Update(ctx, &iamv2pb.UpdateTenantRequest{Metadata: req.GetMetadata(), NamePrefix: "acme-"})
	assertCode(t, err, codes.InvalidArgument)
	_, err = admin.Update(ctx, &iamv2pb.UpdateTenantRequest{Metadata: &commonpb.ResourceMetadata{Name: "acme"}})
	assertCode(t, err, codes.InvalidArgument)
	_, err = admin.Update(ctx, &iamv2pb.UpdateTenantRequest{Metadata: &commonpb.ResourceMetadata{Id: tenant, Name: "acme", ResourceVersion: 99}})
	assertCode(t, err, codes.Aborted)

	// Rename and label the seeded tenant (editors hold the update verb).
	editor := iamv2svc.NewTenantService(env.inst.SDKAs(t, config.DefaultEditorToken))
	before, err := admin.Get(ctx, &iamv2pb.GetTenantRequest{Id: tenant})
	if err != nil {
		t.Fatal(err)
	}
	op, err := editor.Update(ctx, &iamv2pb.UpdateTenantRequest{
		Metadata: &commonpb.ResourceMetadata{Id: tenant, Name: "acme", Labels: map[string]string{"team": "core"}},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Update wait: %v", err)
	}
	after, err := admin.Get(ctx, &iamv2pb.GetTenantRequest{Id: tenant})
	if err != nil {
		t.Fatalf("Get by ID after rename: %v", err)
	}
	md := after.GetMetadata()
	if md.GetName() != "acme" || md.GetLabels()["team"] != "core" || md.GetResourceVersion() != before.GetMetadata().GetResourceVersion()+1 ||
		after.GetStatus().GetTenantState() != iamv2pb.TenantStatus_ACTIVE {
		t.Fatalf("after update = %v", after)
	}
	byName, err := admin.GetByName(ctx, &iamv2pb.GetTenantByNameRequest{Name: "acme"})
	if err != nil || byName.GetMetadata().GetId() != tenant {
		t.Fatalf("GetByName(acme) = %v, %v", byName.GetMetadata(), err)
	}
	_, err = admin.GetByName(ctx, &iamv2pb.GetTenantByNameRequest{Name: "default"})
	assertCode(t, err, codes.NotFound)

	// v1 reads reflect the change too.
	v1 := iamsvc.NewTenantService(env.admin)
	g, err := v1.Get(ctx, &iampb.GetTenantRequest{Id: tenant})
	if err != nil || g.GetMetadata().GetName() != "acme" {
		t.Fatalf("v1 Get = %v, %v", g.GetMetadata(), err)
	}
	l, err := v1.List(ctx, &iampb.ListTenantsRequest{})
	if err != nil || len(l.GetItems()) != 1 || l.GetItems()[0].GetMetadata().GetName() != "acme" {
		t.Fatalf("v1 List = %v, %v", l.GetItems(), err)
	}

	// An X-ResetMask on labels clears them; the name is kept.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	op, err = admin.Update(maskCtx, &iamv2pb.UpdateTenantRequest{Metadata: &commonpb.ResourceMetadata{Id: tenant, Name: "acme"}})
	if err != nil {
		t.Fatalf("Update with reset mask: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Update with reset mask wait: %v", err)
	}
	cleared, err := admin.Get(ctx, &iamv2pb.GetTenantRequest{Id: tenant})
	if err != nil || len(cleared.GetMetadata().GetLabels()) != 0 || cleared.GetMetadata().GetName() != "acme" {
		t.Fatalf("after reset = %v, %v", cleared.GetMetadata(), err)
	}

	// The seeded tenant is still found by ID through the project listing.
	projects, err := iamsvc.NewProjectService(env.admin).List(ctx, &iampb.ListProjectsRequest{ParentId: tenant})
	if err != nil || len(projects.GetItems()) == 0 {
		t.Fatalf("projects of the renamed tenant = %v, %v", projects.GetItems(), err)
	}
}

// --- v1 SessionManagementService.Revoke ---

func TestSessionRevoke(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	ctx := t.Context()
	revoke := func(sdk iamsvc.SessionManagementService, req *iampb.RevokeSessionRequest) error {
		_, err := sdk.Revoke(ctx, req)
		return err
	}
	admin := iamsvc.NewSessionManagementService(env.admin)
	editor := iamsvc.NewSessionManagementService(env.inst.SDKAs(t, config.DefaultEditorToken))
	viewer := iamsvc.NewSessionManagementService(env.inst.SDKAs(t, config.DefaultViewerToken))

	exchange := func(k saAuthKey) string {
		t.Helper()
		return saAuthMustExchange(t, env.inst, saAuthForm(k.assertion(t, clock.Real{}.Now().Add(5*time.Minute))))
	}
	saA, saB := env.newSA(t, "alpha"), env.newSA(t, "beta")
	keyA, keyB := env.newKey(t, saA, nil), env.newKey(t, saB, nil)
	tokenA, tokenA2, tokenB := exchange(keyA), exchange(keyA), exchange(keyB)
	for _, tok := range []string{tokenA, tokenA2, tokenB} {
		if _, err := saAuthProfileID(t, env.inst.SDKAs(t, tok)); err != nil {
			t.Fatalf("fresh token rejected: %v", err)
		}
	}

	// Argument errors.
	assertCode(t, revoke(admin, &iampb.RevokeSessionRequest{}), codes.InvalidArgument)
	assertCode(t, revoke(admin, &iampb.RevokeSessionRequest{Revoke: &iampb.RevokeSessionRequest_AllMyActive{AllMyActive: false}}), codes.InvalidArgument)
	assertCode(t, revoke(admin, &iampb.RevokeSessionRequest{Revoke: &iampb.RevokeSessionRequest_ServiceAccountId{ServiceAccountId: "serviceaccount-e00nope"}}), codes.NotFound)
	assertCode(t, revoke(admin, &iampb.RevokeSessionRequest{Revoke: &iampb.RevokeSessionRequest_TenantUserAccountId{TenantUserAccountId: "tenantuseraccount-e00nope"}}), codes.NotFound)

	// Only the admin role holds the custom revokeSessions verb.
	saReq := &iampb.RevokeSessionRequest{Revoke: &iampb.RevokeSessionRequest_ServiceAccountId{ServiceAccountId: saA}}
	tuaReq := &iampb.RevokeSessionRequest{Revoke: &iampb.RevokeSessionRequest_TenantUserAccountId{TenantUserAccountId: config.DefaultViewerPrincipal}}
	for name, svc := range map[string]iamsvc.SessionManagementService{"editor": editor, "viewer": viewer} {
		if err := revoke(svc, saReq); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s revoking a service account: code %v, want PermissionDenied (%v)", name, status.Code(err), err)
		}
		if err := revoke(svc, tuaReq); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s revoking a tenant user account: code %v, want PermissionDenied (%v)", name, status.Code(err), err)
		}
	}
	if _, err := saAuthProfileID(t, env.inst.SDKAs(t, tokenA)); err != nil {
		t.Fatalf("token revoked by a denied call: %v", err)
	}

	// Revoking a service account denies all of its tokens and no others.
	if err := revoke(admin, saReq); err != nil {
		t.Fatalf("Revoke service account: %v", err)
	}
	saAuthWantTokenRejected(t, env.inst, tokenA)
	saAuthWantTokenRejected(t, env.inst, tokenA2)
	if got, err := saAuthProfileID(t, env.inst.SDKAs(t, tokenB)); err != nil || got != saB {
		t.Fatalf("other service account's token = %q, %v; want still valid", got, err)
	}
	// A new exchange after the revocation yields a working token.
	fresh := exchange(keyA)
	if got, err := saAuthProfileID(t, env.inst.SDKAs(t, fresh)); err != nil || got != saA {
		t.Fatalf("token issued after revoke = %q, %v; want valid", got, err)
	}

	// all_my_active needs no permission: a service account with no roles
	// revokes its own tokens (only its own).
	mine := &iampb.RevokeSessionRequest{Revoke: &iampb.RevokeSessionRequest_AllMyActive{AllMyActive: true}}
	own := iamsvc.NewSessionManagementService(env.inst.SDKAs(t, tokenB))
	tokenB2 := exchange(keyB)
	if err := revoke(own, mine); err != nil {
		t.Fatalf("Revoke all_my_active: %v", err)
	}
	saAuthWantTokenRejected(t, env.inst, tokenB)
	saAuthWantTokenRejected(t, env.inst, tokenB2)
	if got, err := saAuthProfileID(t, env.inst.SDKAs(t, fresh)); err != nil || got != saA {
		t.Fatalf("service account A token after B's all_my_active = %q, %v; want valid", got, err)
	}

	// Static dev tokens are configuration, not sessions: revoking their
	// principals succeeds and leaves them valid.
	if err := revoke(admin, mine); err != nil {
		t.Fatalf("admin Revoke all_my_active: %v", err)
	}
	if err := revoke(admin, &iampb.RevokeSessionRequest{Revoke: &iampb.RevokeSessionRequest_TenantUserAccountId{TenantUserAccountId: config.DefaultViewerPrincipal}}); err != nil {
		t.Fatalf("Revoke tenant user account: %v", err)
	}
	if _, err := iamsvc.NewServiceAccountService(env.admin).List(ctx, &iampb.ListServiceAccountRequest{ParentId: env.inst.ProjectID}); err != nil {
		t.Fatalf("admin static token after Revoke = %v, want still valid", err)
	}
	if _, err := iamsvc.NewTenantService(env.inst.SDKAs(t, config.DefaultViewerToken)).List(ctx, &iampb.ListTenantsRequest{}); err != nil {
		t.Fatalf("viewer static token after Revoke = %v, want still valid", err)
	}
}
