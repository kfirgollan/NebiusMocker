package contract

import (
	"crypto/rsa"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/auth"
	gosdkconfig "github.com/nebius/gosdk/config"
	"github.com/nebius/gosdk/config/reader"
	"github.com/nebius/gosdk/conn"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	mockauth "github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// fcEnv is a running mocker (IAM, authz enforced) plus the admin's
// FederatedCredentialsService client.
type fcEnv struct {
	*saAuthEnv
	creds iamsvc.FederatedCredentialsService
}

func fcStart(t *testing.T, opts ...testkit.Option) *fcEnv {
	t.Helper()
	env := saAuthStartIAM(t, append([]testkit.Option{testkit.WithAuthz(authz.ModeEnforce)}, opts...)...)
	return &fcEnv{saAuthEnv: env, creds: iamsvc.NewFederatedCredentialsService(env.admin)}
}

// fakeIssuer is the issuer URL of the mocker's fake OIDC issuer name, as a
// client on this machine reaches it.
func (e *fcEnv) fakeIssuer(name string) string {
	return "http://" + e.inst.Addr + "/_mocker/oidc/" + name
}

// mint POSTs to the fake issuer's token endpoint, as a user would with curl.
func (e *fcEnv) mint(t *testing.T, issuer string, form url.Values) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, issuer+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s/token: %v", issuer, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/jwt" {
		t.Fatalf("POST %s/token: %d %s", issuer, resp.StatusCode, body)
	}
	return string(body)
}

func fcSpec(issuer, jwks, sub, saID string) *iampb.FederatedCredentialsSpec {
	return &iampb.FederatedCredentialsSpec{
		CredentialsProvider: &iampb.FederatedCredentialsSpec_OidcProvider{OidcProvider: &iampb.OidcCredentialsProvider{
			IssuerUrl: issuer, JwkSetJson: jwks,
		}},
		FederatedSubjectId: sub, SubjectId: saID,
	}
}

func (e *fcEnv) create(t *testing.T, svc iamsvc.FederatedCredentialsService, parent, name string, spec *iampb.FederatedCredentialsSpec) string {
	t.Helper()
	op, err := svc.Create(t.Context(), &iampb.CreateFederatedCredentialsRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name, Labels: map[string]string{"team": "ci"}}, Spec: spec,
	})
	if err != nil {
		t.Fatalf("Create federated credentials: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create federated credentials wait: %v", err)
	}
	return op.ResourceID()
}

func (e *fcEnv) get(t *testing.T, id string) *iampb.FederatedCredentials {
	t.Helper()
	c, err := e.creds.Get(t.Context(), &iampb.GetFederatedCredentialsRequest{Id: id})
	if err != nil {
		t.Fatalf("Get federated credentials %s: %v", id, err)
	}
	return c
}

func (e *fcEnv) remove(t *testing.T, id string) {
	t.Helper()
	op, err := e.creds.Delete(t.Context(), &iampb.DeleteFederatedCredentialsRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete federated credentials: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Delete federated credentials wait: %v", err)
	}
}

func fcWriteFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token.jwt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fcForm is the federated exchange as gosdk's FederatedCredentialsTokenRequester sends it.
func fcForm(saID, jwtToken string) url.Values {
	return url.Values{
		"grant_type":           {saAuthGrantType},
		"requested_token_type": {saAuthAccessType},
		"subject_token":        {saID},
		"subject_token_type":   {saAuthSubjectType},
		"actor_token":          {jwtToken},
		"actor_token_type":     {saAuthJWTType},
	}
}

func TestFederatedCredentialsCRUD(t *testing.T) {
	t.Parallel()
	env := fcStart(t)
	ctx := t.Context()
	project := env.inst.ProjectID
	saID := env.newSA(t, "ci")
	issuer := env.fakeIssuer("ci")

	id := env.create(t, env.creds, project, "github", fcSpec(issuer, "", "repo:org/app", saID))
	c := env.get(t, id)
	if !strings.HasPrefix(id, "federatedcredentials-") || c.GetMetadata().GetParentId() != project ||
		c.GetSpec().GetOidcProvider().GetIssuerUrl() != issuer || c.GetSpec().GetSubjectId() != saID ||
		c.GetMetadata().GetLabels()["team"] != "ci" || c.GetStatus() == nil {
		t.Fatalf("created = %v", c)
	}
	byName, err := env.creds.GetByName(ctx, &iampb.GetByNameFederatedCredentialsRequest{ParentId: project, Name: "github"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	list, err := env.creds.List(ctx, &iampb.ListFederatedCredentialsRequest{ParentId: project})
	if err != nil || len(list.GetItems()) != 1 {
		t.Errorf("List = %v, %v", list, err)
	}
	_, err = env.creds.List(ctx, &iampb.ListFederatedCredentialsRequest{ParentId: project, Filter: "name = 'github'"})
	authKeyWantInvalid(t, "List with filter", err, "filter")

	op, err := env.creds.Update(ctx, &iampb.UpdateFederatedCredentialsRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: project, Name: "github", Labels: map[string]string{"team": "infra"}},
		Spec:     fcSpec(issuer, "", "repo:org/app:ref:main", saID),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	authKeyWaitOp(t, "Update", func() error { _, err := op.Wait(ctx); return err })
	c = env.get(t, id)
	if c.GetSpec().GetFederatedSubjectId() != "repo:org/app:ref:main" || c.GetMetadata().GetLabels()["team"] != "infra" ||
		c.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("after update = %v", c)
	}

	env.remove(t, id)
	if _, err := env.creds.Get(ctx, &iampb.GetFederatedCredentialsRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after delete: %v, want NotFound", err)
	}
}

func TestFederatedCredentialsCreateErrors(t *testing.T) {
	t.Parallel()
	env := fcStart(t, testkit.WithProjects(config.ProjectSeed{Name: "second"}))
	project := env.inst.ProjectID
	saID := env.newSA(t, "ci")
	other := createServiceAccount(t, env.sas, env.inst.Projects[1].ID, "elsewhere").GetMetadata().GetId()
	issuer := env.fakeIssuer("ci")
	for name, tt := range map[string]struct {
		parent string
		spec   *iampb.FederatedCredentialsSpec
		field  string
	}{
		"SA of another project":   {project, fcSpec(issuer, "", "s", other), "spec.subject_id"},
		"unknown SA":              {project, fcSpec(issuer, "", "s", "serviceaccount-e00nope000000000"), "spec.subject_id"},
		"no federated subject":    {project, fcSpec(issuer, "", "", saID), "spec.federated_subject_id"},
		"relative issuer":         {project, fcSpec("issuer.example", "", "s", saID), "spec.oidc_provider.issuer_url"},
		"jwk_set_json not a JWKS": {project, fcSpec(issuer, `{"keys":[]}`, "s", saID), "spec.oidc_provider.jwk_set_json"},
	} {
		_, err := env.creds.Create(t.Context(), &iampb.CreateFederatedCredentialsRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: tt.parent}, Spec: tt.spec,
		})
		authKeyWantInvalid(t, name, err, tt.field)
	}
	_, err := env.creds.Create(t.Context(), &iampb.CreateFederatedCredentialsRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project}, Spec: fcSpec(issuer, "", "", saID),
	})
	if err == nil || !strings.Contains(err.Error(), "does not trust every subject") {
		t.Errorf("missing federated_subject_id: %v", err)
	}
	env.create(t, env.creds, project, "dup", fcSpec(issuer, "", "s", saID))
	_, err = env.creds.Create(t.Context(), &iampb.CreateFederatedCredentialsRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: "dup"}, Spec: fcSpec(issuer, "", "s", saID),
	})
	assertCode(t, err, codes.AlreadyExists)
}

func TestFederatedCredentialsAuthz(t *testing.T) {
	t.Parallel()
	env := fcStart(t)
	ctx := t.Context()
	project := env.inst.ProjectID
	saID := env.newSA(t, "ci")
	spec := fcSpec(env.fakeIssuer("ci"), "", "s", saID)
	id := env.create(t, env.creds, project, "seed", spec)

	viewer := iamsvc.NewFederatedCredentialsService(env.inst.SDKAs(t, config.DefaultViewerToken))
	if _, err := viewer.Get(ctx, &iampb.GetFederatedCredentialsRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := viewer.List(ctx, &iampb.ListFederatedCredentialsRequest{ParentId: project}); err != nil {
		t.Errorf("viewer List: %v", err)
	}
	_, err := viewer.Create(ctx, &iampb.CreateFederatedCredentialsRequest{Metadata: &commonpb.ResourceMetadata{ParentId: project}, Spec: spec})
	assertCode(t, err, codes.PermissionDenied)
	_, err = viewer.Update(ctx, &iampb.UpdateFederatedCredentialsRequest{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: spec})
	assertCode(t, err, codes.PermissionDenied)
	_, err = viewer.Delete(ctx, &iampb.DeleteFederatedCredentialsRequest{Id: id})
	assertCode(t, err, codes.PermissionDenied)

	editor := iamsvc.NewFederatedCredentialsService(env.inst.SDKAs(t, config.DefaultEditorToken))
	byEditor := env.create(t, editor, project, "by-editor", spec)
	del, err := editor.Delete(ctx, &iampb.DeleteFederatedCredentialsRequest{Id: byEditor})
	if err != nil {
		t.Fatalf("editor Delete: %v", err)
	}
	authKeyWaitOp(t, "editor Delete", func() error { _, err := del.Wait(ctx); return err })

	anon := iamsvc.NewFederatedCredentialsService(env.inst.SDKAs(t, ""))
	_, err = anon.Get(ctx, &iampb.GetFederatedCredentialsRequest{Id: id})
	assertCode(t, err, codes.Unauthenticated)
}

// fcSDK is a gosdk client authenticating as saID with federated credentials:
// gosdk's own NewFederatedCredentialsTokener reads file and exchanges it.
func fcSDK(t *testing.T, inst *testkit.Instance, saID, file string) *gosdk.SDK {
	t.Helper()
	cc, err := grpc.NewClient(inst.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() }) // best-effort: local connection
	tokener := auth.NewFederatedCredentialsTokener(saID, file, func() (iampb.TokenExchangeServiceClient, error) {
		return iampb.NewTokenExchangeServiceClient(cc), nil
	})
	sdk, err := gosdk.New(t.Context(),
		gosdk.WithResolvers(conn.NewBasicResolver("*", inst.Addr)),
		gosdk.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		gosdk.WithCredentials(gosdk.CustomTokener(tokener)),
	)
	if err != nil {
		t.Fatalf("gosdk.New with federated credentials: %v", err)
	}
	t.Cleanup(func() { _ = sdk.Close() }) // best-effort: connections are local
	return sdk
}

// fcProfileSDK is a gosdk client configured like a CLI profile with
// service-account-id and federated-subject-credentials-file-path.
func fcProfileSDK(t *testing.T, inst *testkit.Instance, saID, file string) *gosdk.SDK {
	t.Helper()
	cfg := &gosdkconfig.Config{Default: "fed", Profiles: gosdkconfig.ProfilesConfig{"fed": {
		Endpoint: inst.Addr, AuthType: gosdkconfig.AuthTypeServiceAccount, ServiceAccountID: saID,
		FederatedSubjectCredentialsFilePath: file, ParentID: inst.ProjectID,
	}}}
	sdk, err := gosdk.New(t.Context(),
		gosdk.WithResolvers(conn.NewBasicResolver("*", inst.Addr)),
		gosdk.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		gosdk.WithConfigReader(reader.NewConfigReader(
			reader.WithPreloadedConfig(cfg), reader.WithoutEnvVars(), reader.WithoutFileCache(), reader.WithoutBrowserOpen(),
		)),
	)
	if err != nil {
		t.Fatalf("gosdk.New with a federated profile: %v", err)
	}
	t.Cleanup(func() { _ = sdk.Close() }) // best-effort: connections are local
	return sdk
}

func TestFederatedExchangeFakeIssuerGoSDK(t *testing.T) {
	t.Parallel()
	env := fcStart(t)
	ctx := t.Context()
	saID := env.newSA(t, "ci")
	env.addToGroup(t, saID, "viewers")
	issuer := env.fakeIssuer("ci")
	credID := env.create(t, env.creds, env.inst.ProjectID, "ci", fcSpec(issuer, "", "ci-runner", saID))
	file := fcWriteFile(t, env.mint(t, issuer, url.Values{"sub": {"ci-runner"}, "aud": {"nebius"}}))

	for name, sdk := range map[string]*gosdk.SDK{
		"tokener": fcSDK(t, env.inst, saID, file),
		"profile": fcProfileSDK(t, env.inst, saID, file),
	} {
		saAuthWantProfile(t, sdk, saID)
		// The viewers membership applies: reads succeed, writes are denied.
		if _, err := iamsvc.NewServiceAccountService(sdk).List(ctx, &iampb.ListServiceAccountRequest{ParentId: env.inst.ProjectID}); err != nil {
			t.Errorf("%s: List service accounts as the federated SA: %v", name, err)
		}
		_, err := iamsvc.NewGroupService(sdk).Create(ctx, createReq(env.inst.TenantID, "by-federated-"+name))
		assertCode(t, err, codes.PermissionDenied)
	}

	// The same exchange over HTTP.
	res := saAuthPostForm(t, env.inst.Addr, fcForm(saID, env.mint(t, issuer, url.Values{"sub": {"ci-runner"}})))
	if res.status != http.StatusOK || res.str("access_token") == "" || res.str("issued_token_type") != saAuthAccessType {
		t.Fatalf("HTTP exchange = %d %v", res.status, res.body)
	}
	token := res.str("access_token")
	if got, err := saAuthProfileID(t, env.inst.SDKAs(t, token)); err != nil || got != saID {
		t.Errorf("HTTP-exchanged token is %q (%v), want %s", got, err, saID)
	}

	// Deleting the credentials revokes its tokens and stops new exchanges.
	env.remove(t, credID)
	saAuthWantTokenRejected(t, env.inst, token)
	res = saAuthPostForm(t, env.inst.Addr, fcForm(saID, env.mint(t, issuer, url.Values{"sub": {"ci-runner"}})))
	if res.status != http.StatusBadRequest || res.str("error") != "invalid_grant" {
		t.Errorf("exchange after delete = %d %v, want 400 invalid_grant", res.status, res.body)
	}
}

func TestFederatedExchangeInlineJWKS(t *testing.T) {
	t.Parallel()
	env := fcStart(t)
	saID := env.newSA(t, "ci")
	priv := saAuthGenerate(t)
	jwks, err := mockauth.JWKSet{Keys: []mockauth.JWK{{KeyID: "ci-key", Key: &priv.PublicKey}}}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	// The issuer does not resolve: the keys come from jwk_set_json only.
	const issuer = "https://oidc.invalid/ci"
	env.create(t, env.creds, env.inst.ProjectID, "inline", fcSpec(issuer, string(jwks), "deploy", saID))
	sign := func(edit func(jwt.MapClaims)) string { return fcSign(t, priv, "ci-key", issuer, "deploy", edit) }

	resp, err := saAuthExchange(t, env.inst, fcForm(saID, sign(nil)))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got, err := saAuthProfileID(t, env.inst.SDKAs(t, resp.GetAccessToken())); err != nil || got != saID {
		t.Errorf("exchanged token is %q (%v), want %s", got, err, saID)
	}

	for name, tok := range map[string]string{
		"wrong sub": sign(func(c jwt.MapClaims) { c["sub"] = "other" }),
		"wrong iss": sign(func(c jwt.MapClaims) { c["iss"] = "https://evil.invalid/ci" }),
		"expired":   sign(func(c jwt.MapClaims) { c["exp"] = clock.Real{}.Now().Add(-time.Minute).Unix() }),
		"other key": fcSign(t, saAuthGenerate(t), "ci-key", issuer, "deploy", nil),
	} {
		_, err := saAuthExchange(t, env.inst, fcForm(saID, tok))
		assertCode(t, err, codes.Unauthenticated)
		if res := saAuthPostForm(t, env.inst.Addr, fcForm(saID, tok)); res.status != http.StatusBadRequest || res.str("error") != "invalid_grant" {
			t.Errorf("%s over HTTP = %d %v, want 400 invalid_grant", name, res.status, res.body)
		}
	}
	// Malformed actor tokens are invalid_request.
	if res := saAuthPostForm(t, env.inst.Addr, fcForm(saID, "not-a-jwt")); res.status != http.StatusBadRequest || res.str("error") != "invalid_request" {
		t.Errorf("malformed JWT over HTTP = %d %v, want 400 invalid_request", res.status, res.body)
	}
}

func fcSign(t *testing.T, priv *rsa.PrivateKey, kid, iss, sub string, edit func(jwt.MapClaims)) string {
	t.Helper()
	now := clock.Real{}.Now()
	c := jwt.MapClaims{"iss": iss, "sub": sub, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix()}
	if edit != nil {
		edit(c)
	}
	return saAuthSign(t, jwt.SigningMethodRS256, priv, kid, c)
}

func TestFederatedExchangeRefusals(t *testing.T) {
	t.Parallel()
	env := fcStart(t)
	saID := env.newSA(t, "ci")
	issuer := env.fakeIssuer("ci")
	env.create(t, env.creds, env.inst.ProjectID, "ci", fcSpec(issuer, "", "ci-runner", saID))

	for name, tt := range map[string]struct{ token, mention string }{
		"wrong sub":    {env.mint(t, issuer, url.Values{"sub": {"someone-else"}}), "do not trust subject"},
		"other issuer": {env.mint(t, env.fakeIssuer("other"), url.Values{"sub": {"ci-runner"}}), "trust issuer"},
	} {
		_, err := saAuthExchange(t, env.inst, fcForm(saID, tt.token))
		assertCode(t, err, codes.Unauthenticated)
		if err != nil && !strings.Contains(err.Error(), tt.mention) {
			t.Errorf("%s: %v, want it to mention %q", name, err, tt.mention)
		}
	}

	// Deleting the service account stops its federated credentials.
	op, err := env.sas.Delete(t.Context(), &iampb.DeleteServiceAccountRequest{Id: saID})
	if err != nil {
		t.Fatalf("Delete service account: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Delete service account wait: %v", err)
	}
	_, err = saAuthExchange(t, env.inst, fcForm(saID, env.mint(t, issuer, url.Values{"sub": {"ci-runner"}})))
	assertCode(t, err, codes.Unauthenticated)
	if err != nil && !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("exchange for a deleted SA: %v, want it to say the SA does not exist", err)
	}
}

// TestFakeOIDCDisabled: NEBIUS_MOCKER_IAM_FAKE_OIDC=false removes the fake issuer routes.
func TestFakeOIDCDisabled(t *testing.T) {
	t.Parallel()
	env := fcStart(t, testkit.WithConfig(func(c *config.Config) { c.Auth.FakeOIDC = false }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.fakeIssuer("ci")+"/.well-known/openid-configuration", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // nothing to read
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("discovery with the fake issuer disabled = %d, want 404", resp.StatusCode)
	}
}
