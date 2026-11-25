package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	aisvc "github.com/nebius/gosdk/services/nebius/ai/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	mbsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
)

const (
	aiGranteeToken = "ai-grantee-token"
	aiGranteeID    = "serviceaccount-e00mock0000000aigrant"
)

// aiSecretFixture is a project with a MysteryBox secret holding AUTH_TOKEN.
type aiSecretFixture struct {
	env      *aiEnv
	secretID string
}

func aiStartWithSecret(t *testing.T, opts ...testkit.Option) aiSecretFixture {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithTokens(map[string]string{aiGranteeToken: aiGranteeID})}, opts...)
	env := startAI(t, []mocker.Module{mysterybox.New()}, opts...)
	mb := mbsvc.NewSecretService(env.inst.SDKAs(t, config.DefaultDevToken))
	sid := mbCreateSecret(t, mb, env.inst.ProjectID, "ai-token", []*mysteryboxpb.Payload{mbStr("AUTH_TOKEN", "box-token")})
	return aiSecretFixture{env: env, secretID: sid}
}

func (f aiSecretFixture) as(t *testing.T, token string) aisvc.EndpointService {
	t.Helper()
	return aisvc.NewEndpointService(f.env.inst.SDKAs(t, token))
}

// authSpec authenticates the endpoint with the secret.
func (f aiSecretFixture) authSpec() *aipb.EndpointSpec {
	spec := validSpec()
	spec.AuthToken = ""
	spec.AuthTokenMysteryboxSecret = &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: f.secretID}
	return spec
}

// envSpec passes the secret to the workload as an environment variable.
func (f aiSecretFixture) envSpec() *aipb.EndpointSpec {
	spec := validSpec()
	spec.EnvironmentVariables = []*aipb.EndpointSpec_EnvironmentVariable{
		{Name: "TOKEN", MysteryboxSecret: &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: f.secretID}},
	}
	return spec
}

// aiGrantRole gives principal role on resourceID the way a user does: a
// group, a membership and an access permit, all created through IAM.
func aiGrantRole(t *testing.T, f aiSecretFixture, principalID, resourceID, role string) {
	t.Helper()
	sdk := f.env.inst.SDKAs(t, config.DefaultDevToken)
	groups := iamsvc.NewGroupService(sdk)
	gop, err := groups.Create(t.Context(), &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: f.env.inst.TenantID, Name: "grant-" + strings.ReplaceAll(role, ".", "-")},
		Spec:     &iampb.GroupSpec{},
	})
	gid := mbDone(t, gop, err)
	mop, err := iamsvc.NewGroupMembershipService(sdk).Create(t.Context(), &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.GroupMembershipSpec{MemberId: principalID},
	})
	mbDone(t, mop, err)
	pop, err := iamsvc.NewAccessPermitService(sdk).Create(t.Context(), &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.AccessPermitSpec{ResourceId: resourceID, Role: role},
	})
	mbDone(t, pop, err)
}

// aiEndpointCount is how many endpoints the project holds.
func aiEndpointCount(t *testing.T, f aiSecretFixture) int {
	t.Helper()
	resp, err := f.env.svc.List(t.Context(), &aipb.ListEndpointsRequest{ParentId: f.env.inst.ProjectID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(resp.GetItems())
}

// TestAIEndpointSecretPermission: wiring a secret into an endpoint hands its
// payload to the workload, so the caller needs mysterybox.payload.read on it.
func TestAIEndpointSecretPermission(t *testing.T) {
	t.Parallel()
	f := aiStartWithSecret(t, testkit.WithAuthz(authz.ModeEnforce))
	project := f.env.inst.ProjectID

	t.Run("admin may", func(t *testing.T) {
		t.Parallel()
		aiCreateWith(t, f.as(t, config.DefaultDevToken), project, "by-admin", f.authSpec())
	})
	t.Run("editor may not use the secret as auth token", func(t *testing.T) {
		t.Parallel()
		_, err := f.as(t, config.DefaultEditorToken).Create(t.Context(), aiCreateReq(project, "by-editor", f.authSpec()))
		assertCode(t, err, codes.PermissionDenied)
		if _, err := f.env.svc.GetByName(t.Context(), &aipb.GetEndpointByNameRequest{ParentId: project, Name: "by-editor"}); err == nil {
			t.Error("endpoint by-editor exists after a denied create")
		}
	})
	t.Run("editor may not use the secret as env var", func(t *testing.T) {
		t.Parallel()
		_, err := f.as(t, config.DefaultEditorToken).Create(t.Context(), aiCreateReq(project, "env-by-editor", f.envSpec()))
		assertCode(t, err, codes.PermissionDenied)
		if _, err := f.env.svc.GetByName(t.Context(), &aipb.GetEndpointByNameRequest{ParentId: project, Name: "env-by-editor"}); err == nil {
			t.Error("endpoint env-by-editor exists after a denied create")
		}
	})
	t.Run("editor without secrets may create", func(t *testing.T) {
		t.Parallel()
		aiCreateWith(t, f.as(t, config.DefaultEditorToken), project, "plain-editor", validSpec())
	})
}

// TestAIEndpointSecretPermissionGranted: an editor who was also given the
// payload-viewer role through IAM may reference the secret.
func TestAIEndpointSecretPermissionGranted(t *testing.T) {
	t.Parallel()
	f := aiStartWithSecret(t, testkit.WithAuthz(authz.ModeEnforce))
	project := f.env.inst.ProjectID
	grantee := f.as(t, aiGranteeToken)

	aiGrantRole(t, f, aiGranteeID, project, authz.RoleEditor)
	_, err := grantee.Create(t.Context(), aiCreateReq(project, "before-grant", f.authSpec()))
	assertCode(t, err, codes.PermissionDenied)

	aiGrantRole(t, f, aiGranteeID, project, authz.RoleMysteryBoxPayloadViewer)
	id := aiCreateWith(t, grantee, project, "after-grant", f.authSpec())
	ref := aiGet(t, f.env.svc, id).GetSpec().GetAuthTokenMysteryboxSecret()
	if ref.GetSecretId() != f.secretID || ref.GetVersionId() == "" {
		t.Errorf("secret ref = %v, want %s pinned to a version", ref, f.secretID)
	}
	if n := aiEndpointCount(t, f); n != 1 {
		t.Errorf("endpoints = %d, want 1 (only the granted create)", n)
	}
}

// TestAIEndpointSecretPermissionAudit: the default audit mode logs the missing
// permission but lets the editor through.
func TestAIEndpointSecretPermissionAudit(t *testing.T) {
	t.Parallel()
	f := aiStartWithSecret(t)
	aiCreateWith(t, f.as(t, config.DefaultEditorToken), f.env.inst.ProjectID, "audited", f.authSpec())
}

// aiCreateWith creates an endpoint with spec as svc's caller and waits for it.
func aiCreateWith(t *testing.T, svc aisvc.EndpointService, parent, name string, spec *aipb.EndpointSpec) string {
	t.Helper()
	op, err := svc.Create(t.Context(), aiCreateReq(parent, name, spec))
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create(%q) wait: %v", name, err)
	}
	return op.ResourceID()
}
