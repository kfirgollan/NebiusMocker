package contract

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/auth"
	"github.com/nebius/gosdk/conn"
	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	auditsvc "github.com/nebius/gosdk/services/nebius/audit/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

const (
	saAuthGrantType   = "urn:ietf:params:oauth:grant-type:token-exchange"
	saAuthAccessType  = "urn:ietf:params:oauth:token-type:access_token"
	saAuthJWTType     = "urn:ietf:params:oauth:token-type:jwt"
	saAuthSubjectType = "urn:nebius:params:oauth:token-type:subject_identifier"
	saAuthIDTokenType = "urn:ietf:params:oauth:token-type:id_token"
	saAuthTokenTTL    = 12 * time.Hour
)

// saAuthEnv is a running mocker plus the admin clients the auth tests drive.
type saAuthEnv struct {
	inst  *testkit.Instance
	admin *gosdk.SDK
	sas   iamsvc.ServiceAccountService
	keys  iamsvc.AuthPublicKeyService
}

func saAuthStart(t *testing.T, modules []mocker.Module, opts ...testkit.Option) *saAuthEnv {
	t.Helper()
	inst := testkit.StartT(t, modules, opts...)
	admin := inst.SDK(t)
	return &saAuthEnv{
		inst:  inst,
		admin: admin,
		sas:   iamsvc.NewServiceAccountService(admin),
		keys:  iamsvc.NewAuthPublicKeyService(admin),
	}
}

func saAuthStartIAM(t *testing.T, opts ...testkit.Option) *saAuthEnv {
	t.Helper()
	return saAuthStart(t, []mocker.Module{iam.New()}, opts...)
}

// saAuthKey is an RSA key pair registered as an auth public key of a service account.
type saAuthKey struct {
	priv *rsa.PrivateKey
	id   string
	saID string
}

func saAuthGenerate(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return k
}

func saAuthPublicPEM(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func saAuthPrivatePEM(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func (e *saAuthEnv) newSA(t *testing.T, name string) string {
	t.Helper()
	return createServiceAccount(t, e.sas, e.inst.ProjectID, name).GetMetadata().GetId()
}

// newKey generates an RSA key and uploads its public half for saID through
// AuthPublicKeyService.Create, as a user would.
func (e *saAuthEnv) newKey(t *testing.T, saID string, expiresAt *timestamppb.Timestamp) saAuthKey {
	t.Helper()
	priv := saAuthGenerate(t)
	op, err := e.keys.Create(t.Context(), &iampb.CreateAuthPublicKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID},
		Spec: &iampb.AuthPublicKeySpec{
			Account:     &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: saID}}},
			ExpiresAt:   expiresAt,
			Description: "contract test key",
			Data:        saAuthPublicPEM(t, priv),
		},
	})
	if err != nil {
		t.Fatalf("Create auth public key: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create auth public key wait: %v", err)
	}
	return saAuthKey{priv: priv, id: op.ResourceID(), saID: saID}
}

func (e *saAuthEnv) keyState(t *testing.T, id string) iampb.AuthPublicKeyStatus_State {
	t.Helper()
	k, err := e.keys.Get(t.Context(), &iampb.GetAuthPublicKeyRequest{Id: id})
	if err != nil {
		t.Fatalf("Get auth public key %s: %v", id, err)
	}
	return k.GetStatus().GetState()
}

func (e *saAuthEnv) deactivate(t *testing.T, id string) {
	t.Helper()
	op, err := e.keys.Deactivate(t.Context(), &iampb.DeactivateAuthPublicKeyRequest{Id: id})
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Deactivate wait: %v", err)
	}
}

func (e *saAuthEnv) activate(t *testing.T, id string) {
	t.Helper()
	op, err := e.keys.Activate(t.Context(), &iampb.ActivateAuthPublicKeyRequest{Id: id})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Activate wait: %v", err)
	}
}

func (e *saAuthEnv) deleteKey(t *testing.T, id string) {
	t.Helper()
	op, err := e.keys.Delete(t.Context(), &iampb.DeleteAuthPublicKeyRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete key: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Delete key wait: %v", err)
	}
}

// addToGroup makes principalID a member of the seeded tenant group name
// ("viewers", "editors" or "admins"), as the admin.
func (e *saAuthEnv) addToGroup(t *testing.T, principalID, name string) {
	t.Helper()
	g, err := iamsvc.NewGroupService(e.admin).GetByName(t.Context(), &iampb.GetGroupByNameRequest{ParentId: e.inst.TenantID, Name: name})
	if err != nil {
		t.Fatalf("GetByName group %q: %v", name, err)
	}
	op, err := iamsvc.NewGroupMembershipService(e.admin).Create(t.Context(), &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: g.GetMetadata().GetId()},
		Spec:     &iampb.GroupMembershipSpec{MemberId: principalID},
	})
	if err != nil {
		t.Fatalf("add %s to %s: %v", principalID, name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("add %s to %s wait: %v", principalID, name, err)
	}
}

// assertion signs a JWT assertion (RS256, kid = the key ID, iss = sub = the
// service account) expiring at exp.
func (k saAuthKey) assertion(t *testing.T, exp time.Time) string {
	t.Helper()
	return saAuthSign(t, jwt.SigningMethodRS256, k.priv, k.id, saAuthClaims(k.saID, exp))
}

func saAuthClaims(saID string, exp time.Time) jwt.MapClaims {
	return jwt.MapClaims{"iss": saID, "sub": saID, "exp": exp.Unix()}
}

func saAuthSign(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return s
}

// credentialsFile writes the key as a gosdk service account credentials file.
func (k saAuthKey) credentialsFile(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"subject-credentials": map[string]string{
		"type": "JWT", "alg": "RS256", "private-key": string(saAuthPrivatePEM(t, k.priv)),
		"kid": k.id, "iss": k.saID, "sub": k.saID,
	}})
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

func (k saAuthKey) privateKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private_key.pem")
	if err := os.WriteFile(path, saAuthPrivatePEM(t, k.priv), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	return path
}

// saAuthSDK is a new gosdk client that authenticates with reader: the SDK signs
// its own JWT and exchanges it, nothing is pre-minted.
func saAuthSDK(t *testing.T, inst *testkit.Instance, reader auth.ServiceAccountReader) *gosdk.SDK {
	t.Helper()
	sdk, err := gosdk.New(t.Context(),
		gosdk.WithResolvers(conn.NewBasicResolver("*", inst.Addr)),
		gosdk.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		gosdk.WithCredentials(gosdk.ServiceAccountReader(reader)),
	)
	if err != nil {
		t.Fatalf("gosdk.New with service account credentials: %v", err)
	}
	t.Cleanup(func() { _ = sdk.Close() }) // best-effort: connections are local
	return sdk
}

// saAuthProfileID returns the service account ID the token authenticates as,
// or the error ProfileService.Get failed with.
func saAuthProfileID(t *testing.T, sdk *gosdk.SDK) (string, error) {
	t.Helper()
	resp, err := iamsvc.NewProfileService(sdk).Get(t.Context(), &iampb.GetProfileRequest{})
	if err != nil {
		return "", err
	}
	return resp.GetServiceAccountProfile().GetInfo().GetMetadata().GetId(), nil
}

func saAuthWantProfile(t *testing.T, sdk *gosdk.SDK, saID string) {
	t.Helper()
	got, err := saAuthProfileID(t, sdk)
	if err != nil {
		t.Fatalf("ProfileService.Get: %v", err)
	}
	if got != saID {
		t.Fatalf("service account profile id = %q, want %q", got, saID)
	}
}

// saAuthForm is the form of the nebius/api README curl.
func saAuthForm(subjectToken string) url.Values {
	return url.Values{
		"grant_type":           {saAuthGrantType},
		"requested_token_type": {saAuthAccessType},
		"subject_token":        {subjectToken},
		"subject_token_type":   {saAuthJWTType},
	}
}

type saAuthHTTPResponse struct {
	status int
	body   map[string]any
}

func (r saAuthHTTPResponse) str(key string) string {
	s, _ := r.body[key].(string)
	return s
}

// saAuthPostRaw sends the HTTP exchange request as net/http would.
func saAuthPostRaw(t *testing.T, addr, method, contentType string, body []byte) saAuthHTTPResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+"/oauth2/token/exchange", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s token exchange: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := saAuthHTTPResponse{status: resp.StatusCode, body: map[string]any{}}
	_ = json.NewDecoder(resp.Body).Decode(&out.body) // an empty body (405) leaves body empty
	return out
}

func saAuthPostForm(t *testing.T, addr string, form url.Values) saAuthHTTPResponse {
	t.Helper()
	return saAuthPostRaw(t, addr, http.MethodPost, "application/x-www-form-urlencoded", []byte(form.Encode()))
}

// saAuthRequest turns the HTTP form into the gRPC request with the same fields.
func saAuthRequest(form url.Values) *iampb.ExchangeTokenRequest {
	return &iampb.ExchangeTokenRequest{
		GrantType: form.Get("grant_type"), RequestedTokenType: form.Get("requested_token_type"),
		SubjectToken: form.Get("subject_token"), SubjectTokenType: form.Get("subject_token_type"),
		Scopes: strings.Fields(form.Get("scope")), ActorToken: form.Get("actor_token"),
		ActorTokenType: form.Get("actor_token_type"),
	}
}

// saAuthExchange exchanges over gRPC with an anonymous client.
func saAuthExchange(t *testing.T, inst *testkit.Instance, form url.Values) (*iampb.CreateTokenResponse, error) {
	t.Helper()
	return iamsvc.NewTokenExchangeService(inst.SDKAs(t, "")).Exchange(t.Context(), saAuthRequest(form))
}

func saAuthMustExchange(t *testing.T, inst *testkit.Instance, form url.Values) string {
	t.Helper()
	resp, err := saAuthExchange(t, inst, form)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if resp.GetAccessToken() == "" {
		t.Fatal("Exchange returned an empty access_token")
	}
	return resp.GetAccessToken()
}

func saAuthWantTokenRejected(t *testing.T, inst *testkit.Instance, token string) {
	t.Helper()
	_, err := saAuthProfileID(t, inst.SDKAs(t, token))
	assertCode(t, err, codes.Unauthenticated)
}

func TestServiceAccountAuthCredentialsFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reader func(t *testing.T, k saAuthKey) auth.ServiceAccountReader
	}{
		{"credentials file", func(t *testing.T, k saAuthKey) auth.ServiceAccountReader {
			t.Helper()
			return auth.NewServiceAccountCredentialsFileParser(nil, k.credentialsFile(t))
		}},
		{"private key file", func(t *testing.T, k saAuthKey) auth.ServiceAccountReader {
			t.Helper()
			return auth.NewPrivateKeyFileParser(nil, k.privateKeyFile(t), k.id, k.saID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := saAuthStart(t, []mocker.Module{iam.New(), audit.New()}, testkit.WithAuthz(authz.ModeEnforce))
			ctx := t.Context()
			saID := env.newSA(t, "deployer")
			key := env.newKey(t, saID, nil)
			sdk := saAuthSDK(t, env.inst, tc.reader(t, key))

			saAuthWantProfile(t, sdk, saID)
			profile, err := iamsvc.NewProfileService(sdk).Get(ctx, &iampb.GetProfileRequest{})
			if err != nil {
				t.Fatalf("ProfileService.Get: %v", err)
			}
			info := profile.GetServiceAccountProfile().GetInfo()
			if info.GetMetadata().GetName() != "deployer" || !info.GetStatus().GetActive() {
				t.Errorf("profile info = %v, want the active service account %q", info, "deployer")
			}

			// No roles yet: a write is denied. With editor membership it succeeds.
			groups := iamsvc.NewGroupService(sdk)
			_, err = groups.Create(ctx, createReq(env.inst.TenantID, "by-sa"))
			assertCode(t, err, codes.PermissionDenied)
			env.addToGroup(t, saID, "editors")
			createGroup(t, groups, env.inst.TenantID, "by-sa", nil)

			// The audit event of the mutating call names the service account.
			events := auditsvc.NewAuditEventService(env.admin)
			var found bool
			for _, ev := range listEvents(t, events, env.inst.TenantID, "") {
				if ev.GetSource() != "nebius.iam.v1.GroupService/Create" || ev.GetStatus() != auditv2pb.Status_DONE {
					continue
				}
				subj := ev.GetAuthentication().GetSubject()
				if subj.GetServiceAccountId() != saID || subj.GetTenantUserId() != "" {
					continue
				}
				found = true
			}
			if !found {
				t.Errorf("no DONE GroupService/Create audit event with service_account_id %s", saID)
			}
		})
	}
}

func TestServiceAccountAuthHTTPExchange(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t)
	saID := env.newSA(t, "http-sa")
	key := env.newKey(t, saID, nil)

	t.Run("http", func(t *testing.T) {
		t.Parallel()
		jwtStr := key.assertion(t, clock.Real{}.Now().Add(5*time.Minute))
		res := saAuthPostForm(t, env.inst.Addr, saAuthForm(jwtStr))
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", res.status, res.body)
		}
		if got := res.str("issued_token_type"); got != saAuthAccessType {
			t.Errorf("issued_token_type = %q, want %q", got, saAuthAccessType)
		}
		if got := res.str("token_type"); got != "Bearer" {
			t.Errorf("token_type = %q, want Bearer", got)
		}
		if got, _ := res.body["expires_in"].(float64); time.Duration(got)*time.Second != saAuthTokenTTL {
			t.Errorf("expires_in = %v, want %d", res.body["expires_in"], int(saAuthTokenTTL.Seconds()))
		}
		token := res.str("access_token")
		if !strings.HasPrefix(token, "nmock.v1.") {
			t.Fatalf("access_token = %q, want an nmock.v1. token", token)
		}
		saAuthWantProfile(t, env.inst.SDKAs(t, token), saID)
	})

	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		form := saAuthForm(key.assertion(t, clock.Real{}.Now().Add(5*time.Minute)))
		resp, err := saAuthExchange(t, env.inst, form)
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if resp.GetIssuedTokenType() != saAuthAccessType || resp.GetTokenType() != "Bearer" ||
			resp.GetExpiresIn() != int64(saAuthTokenTTL.Seconds()) {
			t.Errorf("response = %v, want access_token type, Bearer, expires_in %d", resp, int64(saAuthTokenTTL.Seconds()))
		}
		saAuthWantProfile(t, env.inst.SDKAs(t, resp.GetAccessToken()), saID)
	})
}

func TestServiceAccountAuthExchangeErrors(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t)
	saID := env.newSA(t, "err-sa")
	other := env.newSA(t, "other-sa")
	key := env.newKey(t, saID, nil)
	now := clock.Real{}.Now()
	sign := func(t *testing.T, method jwt.SigningMethod, signer any, kid string, claims jwt.MapClaims) string {
		t.Helper()
		return saAuthSign(t, method, signer, kid, claims)
	}

	tests := []struct {
		name     string
		form     func(t *testing.T) url.Values
		oauth    string
		grpcCode codes.Code
	}{
		{
			name: "unsupported grant_type", oauth: "unsupported_grant_type", grpcCode: codes.InvalidArgument,
			form: func(t *testing.T) url.Values {
				t.Helper()
				f := saAuthForm(key.assertion(t, now.Add(5*time.Minute)))
				f.Set("grant_type", "password")
				return f
			},
		},
		{
			name: "missing subject_token", oauth: "invalid_request", grpcCode: codes.InvalidArgument,
			form: func(*testing.T) url.Values {
				f := saAuthForm("")
				f.Del("subject_token")
				return f
			},
		},
		{
			name: "bad signature", oauth: "invalid_grant", grpcCode: codes.Unauthenticated,
			form: func(t *testing.T) url.Values {
				t.Helper()
				return saAuthForm(sign(t, jwt.SigningMethodRS256, saAuthGenerate(t), key.id, saAuthClaims(saID, now.Add(5*time.Minute))))
			},
		},
		{
			name: "unknown kid", oauth: "invalid_grant", grpcCode: codes.Unauthenticated,
			form: func(t *testing.T) url.Values {
				t.Helper()
				return saAuthForm(sign(t, jwt.SigningMethodRS256, key.priv, "publickey-e00doesnotexist", saAuthClaims(saID, now.Add(5*time.Minute))))
			},
		},
		{
			name: "iss differs from sub", oauth: "invalid_grant", grpcCode: codes.Unauthenticated,
			form: func(t *testing.T) url.Values {
				t.Helper()
				c := saAuthClaims(saID, now.Add(5*time.Minute))
				c["iss"] = other
				return saAuthForm(sign(t, jwt.SigningMethodRS256, key.priv, key.id, c))
			},
		},
		{
			name: "expired exp", oauth: "invalid_grant", grpcCode: codes.Unauthenticated,
			form: func(t *testing.T) url.Values {
				t.Helper()
				return saAuthForm(key.assertion(t, now.Add(-time.Hour)))
			},
		},
		{
			name: "exp too far", oauth: "invalid_grant", grpcCode: codes.Unauthenticated,
			form: func(t *testing.T) url.Values {
				t.Helper()
				return saAuthForm(key.assertion(t, now.Add(2*time.Hour)))
			},
		},
		{
			name: "missing exp", oauth: "invalid_grant", grpcCode: codes.Unauthenticated,
			form: func(t *testing.T) url.Values {
				t.Helper()
				return saAuthForm(sign(t, jwt.SigningMethodRS256, key.priv, key.id, jwt.MapClaims{"iss": saID, "sub": saID}))
			},
		},
		{
			name: "alg none", oauth: "invalid_grant", grpcCode: codes.Unauthenticated,
			form: func(t *testing.T) url.Values {
				t.Helper()
				return saAuthForm(sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, key.id, saAuthClaims(saID, now.Add(5*time.Minute))))
			},
		},
		{
			name: "alg HS256", oauth: "invalid_grant", grpcCode: codes.Unauthenticated,
			form: func(t *testing.T) url.Values {
				t.Helper()
				return saAuthForm(sign(t, jwt.SigningMethodHS256, []byte("shared-secret"), key.id, saAuthClaims(saID, now.Add(5*time.Minute))))
			},
		},
		{
			name: "scope set", oauth: "invalid_scope", grpcCode: codes.InvalidArgument,
			form: func(t *testing.T) url.Values {
				t.Helper()
				f := saAuthForm(key.assertion(t, now.Add(5*time.Minute)))
				f.Set("scope", "read")
				return f
			},
		},
		{
			name: "federated subject_token_type", oauth: "invalid_request", grpcCode: codes.InvalidArgument,
			form: func(t *testing.T) url.Values {
				t.Helper()
				f := saAuthForm(key.assertion(t, now.Add(5*time.Minute)))
				f.Set("subject_token_type", saAuthIDTokenType)
				return f
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			form := tt.form(t)

			res := saAuthPostForm(t, env.inst.Addr, form)
			if res.status != http.StatusBadRequest {
				t.Errorf("HTTP status = %d, want 400 (body %v)", res.status, res.body)
			}
			if got := res.str("error"); got != tt.oauth {
				t.Errorf("HTTP error = %q, want %q (body %v)", got, tt.oauth, res.body)
			}
			if res.str("error_description") == "" {
				t.Errorf("HTTP error_description is empty (body %v)", res.body)
			}

			_, err := saAuthExchange(t, env.inst, form)
			if got := codeOf(err); got != tt.grpcCode {
				t.Errorf("gRPC code = %v, want %v (err: %v)", got, tt.grpcCode, err)
			}
		})
	}

	t.Run("wrong content type", func(t *testing.T) {
		t.Parallel()
		body, err := json.Marshal(map[string]string{"grant_type": saAuthGrantType, "subject_token": key.assertion(t, now.Add(5*time.Minute))})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		res := saAuthPostRaw(t, env.inst.Addr, http.MethodPost, "application/json", body)
		if res.status != http.StatusBadRequest || res.str("error") != "invalid_request" {
			t.Errorf("response = %d %v, want 400 invalid_request", res.status, res.body)
		}
	})

	t.Run("GET", func(t *testing.T) {
		t.Parallel()
		res := saAuthPostRaw(t, env.inst.Addr, http.MethodGet, "", nil)
		if res.status != http.StatusMethodNotAllowed {
			t.Errorf("GET status = %d, want 405", res.status)
		}
	})
}

func TestServiceAccountAuthKeyLifecycle(t *testing.T) {
	// Sequential: the subtests share one manual clock.
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewManual(t0)
	env := saAuthStartIAM(t, testkit.WithClock(clk))
	saID := env.newSA(t, "lifecycle")

	exchange := func(t *testing.T, k saAuthKey) (*iampb.CreateTokenResponse, error) {
		t.Helper()
		return saAuthExchange(t, env.inst, saAuthForm(k.assertion(t, clk.Now().Add(5*time.Minute))))
	}
	mustExchange := func(t *testing.T, k saAuthKey) string {
		t.Helper()
		resp, err := exchange(t, k)
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		return resp.GetAccessToken()
	}
	wantRefused := func(t *testing.T, k saAuthKey) {
		t.Helper()
		_, err := exchange(t, k)
		assertCode(t, err, codes.Unauthenticated)
	}

	t.Run("deactivate and activate", func(t *testing.T) {
		key := env.newKey(t, saID, nil)
		token := mustExchange(t, key)
		saAuthWantProfile(t, env.inst.SDKAs(t, token), saID)

		env.deactivate(t, key.id)
		wantRefused(t, key)
		saAuthWantTokenRejected(t, env.inst, token)

		env.activate(t, key.id)
		again := mustExchange(t, key)
		saAuthWantProfile(t, env.inst.SDKAs(t, again), saID)
		saAuthWantTokenRejected(t, env.inst, token) // the revocation sticks

		env.deleteKey(t, key.id)
		wantRefused(t, key)
		saAuthWantTokenRejected(t, env.inst, again)
	})

	t.Run("key expiry", func(t *testing.T) {
		key := env.newKey(t, saID, timestamppb.New(clk.Now().Add(time.Hour)))
		if got := env.keyState(t, key.id); got != iampb.AuthPublicKeyStatus_ACTIVE {
			t.Fatalf("new key state = %v, want ACTIVE", got)
		}
		mustExchange(t, key)

		clk.Advance(2 * time.Hour)
		if got := env.keyState(t, key.id); got != iampb.AuthPublicKeyStatus_EXPIRED {
			t.Errorf("state after expires_at = %v, want EXPIRED", got)
		}
		wantRefused(t, key)
	})

	t.Run("token expiry", func(t *testing.T) {
		key := env.newKey(t, saID, nil)
		resp, err := exchange(t, key)
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if resp.GetExpiresIn() != int64(saAuthTokenTTL.Seconds()) {
			t.Errorf("expires_in = %d, want %d", resp.GetExpiresIn(), int64(saAuthTokenTTL.Seconds()))
		}
		sdk := env.inst.SDKAs(t, resp.GetAccessToken())
		saAuthWantProfile(t, sdk, saID)
		clk.Advance(saAuthTokenTTL - time.Minute)
		saAuthWantProfile(t, sdk, saID)
		clk.Advance(2 * time.Minute)
		saAuthWantTokenRejected(t, env.inst, resp.GetAccessToken())
	})

	t.Run("service account delete", func(t *testing.T) {
		doomed := env.newSA(t, "doomed")
		key := env.newKey(t, doomed, nil)
		token := mustExchange(t, key)
		saAuthWantProfile(t, env.inst.SDKAs(t, token), doomed)

		op, err := env.sas.Delete(t.Context(), &iampb.DeleteServiceAccountRequest{Id: doomed})
		if err != nil {
			t.Fatalf("Delete service account: %v", err)
		}
		if _, err := op.Wait(t.Context()); err != nil {
			t.Fatalf("Delete service account wait: %v", err)
		}
		saAuthWantTokenRejected(t, env.inst, token)
		wantRefused(t, key)
	})
}

func TestServiceAccountAuthImpersonation(t *testing.T) {
	t.Parallel()
	impersonate := func(actorToken, actorType, subjectType, subject string) url.Values {
		return url.Values{
			"grant_type": {saAuthGrantType}, "requested_token_type": {saAuthAccessType},
			"subject_token": {subject}, "subject_token_type": {subjectType},
			"actor_token": {actorToken}, "actor_token_type": {actorType},
		}
	}

	t.Run("admin actor", func(t *testing.T) {
		t.Parallel()
		env := saAuthStartIAM(t)
		saID := env.newSA(t, "impersonated")
		token := saAuthMustExchange(t, env.inst, impersonate(config.DefaultDevToken, saAuthAccessType, saAuthSubjectType, saID))
		saAuthWantProfile(t, env.inst.SDKAs(t, token), saID)
	})

	t.Run("enforce", func(t *testing.T) {
		t.Parallel()
		env := saAuthStartIAM(t, testkit.WithAuthz(authz.ModeEnforce))
		saID := env.newSA(t, "impersonated")

		token := saAuthMustExchange(t, env.inst, impersonate(config.DefaultDevToken, saAuthAccessType, saAuthSubjectType, saID))
		saAuthWantProfile(t, env.inst.SDKAs(t, token), saID)

		for _, actor := range []string{config.DefaultViewerToken, config.DefaultEditorToken} {
			_, err := saAuthExchange(t, env.inst, impersonate(actor, saAuthAccessType, saAuthSubjectType, saID))
			if got := codeOf(err); got != codes.PermissionDenied {
				t.Errorf("impersonation by %s: code %v, want PermissionDenied (err: %v)", actor, got, err)
			}
		}
	})

	t.Run("actor with jwt subject type", func(t *testing.T) {
		t.Parallel()
		env := saAuthStartIAM(t)
		saID := env.newSA(t, "impersonated")
		key := env.newKey(t, saID, nil)
		form := impersonate(config.DefaultDevToken, saAuthAccessType, saAuthJWTType, key.assertion(t, clock.Real{}.Now().Add(5*time.Minute)))
		_, err := saAuthExchange(t, env.inst, form)
		assertCode(t, err, codes.InvalidArgument)
	})
}
