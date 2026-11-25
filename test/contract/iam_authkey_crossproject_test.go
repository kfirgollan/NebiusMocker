package contract

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

const (
	crossProjEditorToken = "cross-project-editor-token"
	crossProjEditorID    = "tenantuseraccount-e00mock0000000crossedit"
)

// TestAuthKeyCrossProjectRefused proves the rule that an auth public key lives
// in its service account's project: an editor of another existing project
// cannot upload a key for a service account of the default project into the
// project it controls, so it cannot mint credentials for that service account.
// The key is not created, and the refused key material authenticates nothing.
func TestAuthKeyCrossProjectRefused(t *testing.T) {
	t.Parallel()
	e := saAuthStartIAM(t,
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{crossProjEditorToken: crossProjEditorID}),
		testkit.WithProjects(config.ProjectSeed{Name: "other"}),
	)
	ctx := t.Context()
	if len(e.inst.Projects) < 2 {
		t.Fatalf("seeded projects = %v, want the default and %q", e.inst.Projects, "other")
	}
	other := e.inst.Projects[1].ID
	if other == e.inst.ProjectID {
		t.Fatalf("second seeded project %s is the default project", other)
	}

	// The victim service account lives in the default project.
	saID := e.newSA(t, "victim")

	// The admin makes the caller editor of the other project only.
	crossProjGrantEditor(t, e, other)
	editor := e.inst.SDKAs(t, crossProjEditorToken)

	// The grant works: the editor can create service accounts in the other
	// project, but not in the default one.
	if created := createServiceAccount(t, iamsvc.NewServiceAccountService(editor), other, "own-sa"); created.GetMetadata().GetParentId() != other {
		t.Fatalf("editor's service account parent = %s, want %s", created.GetMetadata().GetParentId(), other)
	}
	if _, err := iamsvc.NewServiceAccountService(editor).Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: "not-allowed"},
		Spec:     &iampb.ServiceAccountSpec{},
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("editor of %s creating in the default project: %v, want PermissionDenied", other, err)
	}

	// The attack: a key for the default project's service account, parented
	// in the project the editor controls. Authz passes (create on `other`),
	// the key rule refuses it.
	attacker := saAuthGenerate(t)
	attackReq := &iampb.CreateAuthPublicKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: other, Name: "stolen"},
		Spec:     &iampb.AuthPublicKeySpec{Account: authKeyAccount(saID), Data: saAuthPublicPEM(t, attacker)},
	}
	_, err := iamsvc.NewAuthPublicKeyService(editor).Create(ctx, attackReq)
	authKeyWantInvalid(t, "editor of another project creates a key for "+saID, err, "metadata.parent_id")
	// The rule is not a permission check: the admin is refused the same way.
	_, err = e.keys.Create(ctx, attackReq)
	authKeyWantInvalid(t, "admin creates a key in another project for "+saID, err, "metadata.parent_id")

	// No key was created, in either project or for the account.
	for _, parent := range []string{other, e.inst.ProjectID} {
		resp, err := e.keys.List(ctx, &iampb.ListAuthPublicKeyRequest{ParentId: parent})
		if err != nil {
			t.Fatalf("List keys in %s: %v", parent, err)
		}
		if n := len(resp.GetItems()); n != 0 {
			t.Errorf("keys in %s = %d (%v), want none", parent, n, resp.GetItems())
		}
	}

	// The refused key material mints no token: not with the account's real
	// key ID (the signature does not match), and not with an invented one.
	legit := e.newKey(t, saID, nil)
	byAccount, err := e.keys.ListByAccount(ctx, &iampb.ListAuthPublicKeyByAccountRequest{Account: authKeyAccount(saID)})
	if err != nil {
		t.Fatalf("ListByAccount: %v", err)
	}
	if items := byAccount.GetItems(); len(items) != 1 || items[0].GetMetadata().GetId() != legit.id {
		t.Fatalf("keys of %s = %v, want only %s", saID, items, legit.id)
	}
	exp := clock.Real{}.Now().Add(5 * time.Minute)
	for _, kid := range []string{legit.id, "publickey-e00mock0000000000stolen"} {
		assertion := saAuthSign(t, jwt.SigningMethodRS256, attacker, kid, saAuthClaims(saID, exp))
		if _, err := saAuthExchange(t, e.inst, saAuthForm(assertion)); status.Code(err) != codes.Unauthenticated {
			t.Errorf("exchange of the refused key's JWT (kid %s): %v, want Unauthenticated", kid, err)
		}
		if r := saAuthPostForm(t, e.inst.Addr, saAuthForm(assertion)); r.status != 400 || r.str("error") != "invalid_grant" {
			t.Errorf("HTTP exchange of the refused key's JWT (kid %s) = %d %v, want 400 invalid_grant", kid, r.status, r.body)
		}
	}
	// The legitimate key still works, so the refusals above are about the
	// refused key, not a broken exchange.
	saAuthMustExchange(t, e.inst, saAuthForm(legit.assertion(t, exp)))
}

// crossProjGrantEditor gives crossProjEditorID the editor role on project
// through a new group with an access permit on that project.
func crossProjGrantEditor(t *testing.T, e *saAuthEnv, project string) {
	t.Helper()
	ctx := t.Context()
	gOp, err := iamsvc.NewGroupService(e.admin).Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: "other-project-editors"},
		Spec:     &iampb.GroupSpec{},
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := gOp.Wait(ctx); err != nil {
		t.Fatalf("create group wait: %v", err)
	}
	groupID := gOp.ResourceID()
	pOp, err := iamsvc.NewAccessPermitService(e.admin).Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: groupID},
		Spec:     &iampb.AccessPermitSpec{ResourceId: project, Role: authz.RoleEditor},
	})
	if err != nil {
		t.Fatalf("create permit: %v", err)
	}
	if _, err := pOp.Wait(ctx); err != nil {
		t.Fatalf("create permit wait: %v", err)
	}
	mOp, err := iamsvc.NewGroupMembershipService(e.admin).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: groupID},
		Spec:     &iampb.GroupMembershipSpec{MemberId: crossProjEditorID},
	})
	if err != nil {
		t.Fatalf("create membership: %v", err)
	}
	if _, err := mOp.Wait(ctx); err != nil {
		t.Fatalf("create membership wait: %v", err)
	}
}
