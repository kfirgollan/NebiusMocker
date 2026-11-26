package api_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/api"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	apTenant  = "tenant-e00ap0000000000001"
	apProject = "project-e00ap0000000000001"
	apSA      = "serviceaccount-e00ap0000000001"
	apKey     = "publickey-e00ap0000000000001"
	apActor   = "static-actor-token"
)

var apT0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type apKeys struct{ key *iampb.AuthPublicKey }

func (f apKeys) Get(_ context.Context, id string) (*iampb.AuthPublicKey, error) {
	if id != f.key.GetMetadata().GetId() {
		return nil, errs.NotFound(id)
	}
	return f.key, nil
}

// apNoFederated has no federated credentials and reaches no OIDC issuer.
type apNoFederated struct{}

func (apNoFederated) ListBySubject(context.Context, string) ([]*iampb.FederatedCredentials, error) {
	return nil, nil
}

func (apNoFederated) KeySet(_ context.Context, issuerURL string) ([]byte, error) {
	return nil, errors.New("no issuer " + issuerURL)
}

func (apNoFederated) RefreshKeySet(_ context.Context, issuerURL string) ([]byte, error) {
	return nil, errors.New("no issuer " + issuerURL)
}

// apSAs implements domain.ServiceAccountRepository for one account.
type apSAs struct{ sa *iampb.ServiceAccount }

func (f apSAs) Get(_ context.Context, id string) (*iampb.ServiceAccount, error) {
	if id != f.sa.GetMetadata().GetId() {
		return nil, errs.NotFound(id)
	}
	return f.sa, nil
}

func (apSAs) GetByName(context.Context, string, string) (*iampb.ServiceAccount, error) {
	return nil, errors.New("apSAs: GetByName not expected")
}

func (apSAs) List(context.Context, string, int, string) (domain.ServiceAccountPage, error) {
	return domain.ServiceAccountPage{}, errors.New("apSAs: List not expected")
}

func (apSAs) Create(context.Context, *iampb.ServiceAccount) error {
	return errors.New("apSAs: Create not expected")
}

func (apSAs) Update(context.Context, *iampb.ServiceAccount) error {
	return errors.New("apSAs: Update not expected")
}

func (apSAs) Delete(context.Context, string) error { return errors.New("apSAs: Delete not expected") }

type apProjects struct{}

func (apProjects) Get(_ context.Context, id string) (*iampb.Container, error) {
	if id != apProject {
		return nil, errs.NotFound(id)
	}
	return &iampb.Container{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: apTenant}}, nil
}

func (apProjects) Ensure(context.Context, *iampb.Container) error { return nil }

type apPerms struct{ allowed bool }

func (f apPerms) Check(_ context.Context, _ string, perm authz.Permission, target string) (authz.Decision, error) {
	return authz.Decision{Allowed: f.allowed, Permission: perm, Target: target}, nil
}

type apFixture struct {
	handler *api.TokenExchangeHandler
	issuer  *auth.Issuer
	priv    *rsa.PrivateKey
}

func newApFixture(t *testing.T, impersonationAllowed bool) *apFixture {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewManual(apT0)
	secret, err := auth.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := auth.NewIssuer(secret, time.Hour, clk)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	svc, err := domain.NewTokenExchangeService(domain.TokenExchangeDeps{
		Keys: apKeys{key: &iampb.AuthPublicKey{
			Metadata: &commonpb.ResourceMetadata{Id: apKey},
			Spec: &iampb.AuthPublicKeySpec{
				Account: &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: apSA}}},
				Data:    string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
			},
			Status: &iampb.AuthPublicKeyStatus{State: iampb.AuthPublicKeyStatus_ACTIVE},
		}},
		ServiceAccounts: apSAs{sa: &iampb.ServiceAccount{
			Metadata: &commonpb.ResourceMetadata{Id: apSA, ParentId: apProject},
			Status:   &iampb.ServiceAccountStatus{Active: true},
		}},
		Projects: apProjects{}, Minter: issuer,
		Actors:      auth.NewAuthenticator(map[string]string{apActor: "user-actor"}, issuer),
		Permissions: apPerms{allowed: impersonationAllowed}, FederatedCredentials: apNoFederated{}, OIDC: apNoFederated{},
		Mode:  authz.ModeEnforce,
		Clock: clk, MaxAssertionTTL: 10 * time.Minute, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &apFixture{handler: api.NewTokenExchangeHandler(svc, log), issuer: issuer, priv: priv}
}

func (f *apFixture) assertion(t *testing.T) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": apSA, "sub": apSA, "iat": apT0.Unix(), "exp": apT0.Add(5 * time.Minute).Unix(),
	})
	tok.Header["kid"] = apKey
	s, err := tok.SignedString(f.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *apFixture) jwtForm(t *testing.T) url.Values {
	t.Helper()
	return url.Values{
		"grant_type":           {domain.GrantTypeTokenExchange},
		"requested_token_type": {domain.TokenTypeAccessToken},
		"subject_token":        {f.assertion(t)},
		"subject_token_type":   {domain.TokenTypeJWT},
	}
}

func post(t *testing.T, h http.Handler, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const formType = "application/x-www-form-urlencoded"

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response body %q is not JSON: %v", w.Body.String(), err)
	}
	return m
}

func TestTokenExchangeHTTPSuccess(t *testing.T) {
	t.Parallel()
	f := newApFixture(t, true)
	w := post(t, f.handler, api.TokenExchangePath, formType+"; charset=utf-8", f.jwtForm(t).Encode())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	body := decode(t, w)
	if body["token_type"] != "Bearer" || body["issued_token_type"] != domain.TokenTypeAccessToken {
		t.Errorf("body = %v, want Bearer / access_token", body)
	}
	if body["expires_in"] != float64(3600) {
		t.Errorf("expires_in = %v, want 3600", body["expires_in"])
	}
	access, _ := body["access_token"].(string)
	if c, err := f.issuer.Verify(access); err != nil || c.Principal != apSA {
		t.Errorf("access_token verify = %+v, %v, want principal %s", c, err, apSA)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestTokenExchangeHTTPImpersonation(t *testing.T) {
	t.Parallel()
	f := newApFixture(t, true)
	form := url.Values{
		"grant_type": {domain.GrantTypeTokenExchange}, "subject_token": {apSA},
		"subject_token_type": {domain.TokenTypeSubjectIdentifier},
		"actor_token":        {apActor}, "actor_token_type": {domain.TokenTypeAccessToken},
	}
	w := post(t, f.handler, api.TokenExchangePath, formType, form.Encode())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	access, _ := decode(t, w)["access_token"].(string)
	if c, err := f.issuer.Verify(access); err != nil || c.Principal != apSA || c.Actor != "user-actor" {
		t.Errorf("claims = %+v, %v, want principal %s actor user-actor", c, err, apSA)
	}
}

func TestTokenExchangeHTTPErrors(t *testing.T) {
	t.Parallel()
	f := newApFixture(t, false)
	good := f.jwtForm(t)
	with := func(mutate func(v url.Values)) string {
		v := url.Values{}
		for k, vs := range good {
			v[k] = append([]string(nil), vs...)
		}
		mutate(v)
		return v.Encode()
	}
	tests := []struct {
		name        string
		target      string
		contentType string
		body        string
		wantCode    string
	}{
		{
			name: "wrong content type", target: api.TokenExchangePath, contentType: "application/json",
			body: `{"grant_type":"x"}`, wantCode: "invalid_request",
		},
		{name: "missing content type", target: api.TokenExchangePath, body: good.Encode(), wantCode: "invalid_request"},
		{
			name: "repeated grant_type", target: api.TokenExchangePath, contentType: formType,
			body: good.Encode() + "&grant_type=other", wantCode: "invalid_request",
		},
		{
			name: "repeated subject_token", target: api.TokenExchangePath, contentType: formType,
			body: good.Encode() + "&subject_token=again", wantCode: "invalid_request",
		},
		{
			name: "params in URL query", target: api.TokenExchangePath + "?" + good.Encode(), contentType: formType,
			body: "", wantCode: "invalid_request",
		},
		{
			name: "bad grant_type", target: api.TokenExchangePath, contentType: formType,
			body: with(func(v url.Values) { v.Set("grant_type", "password") }), wantCode: "unsupported_grant_type",
		},
		{
			name: "bad JWT", target: api.TokenExchangePath, contentType: formType,
			body: with(func(v url.Values) { v.Set("subject_token", "not.a.jwt") }), wantCode: "invalid_grant",
		},
		{
			name: "scope set", target: api.TokenExchangePath, contentType: formType,
			body: with(func(v url.Values) { v.Set("scope", "compute") }), wantCode: "invalid_scope",
		},
		{
			name: "audience set", target: api.TokenExchangePath, contentType: formType,
			body: with(func(v url.Values) { v.Set("audience", "x") }), wantCode: "invalid_target",
		},
		{
			name: "impersonation denied", target: api.TokenExchangePath, contentType: formType,
			body: url.Values{
				"grant_type": {domain.GrantTypeTokenExchange}, "subject_token": {apSA},
				"subject_token_type": {domain.TokenTypeSubjectIdentifier},
				"actor_token":        {apActor}, "actor_token_type": {domain.TokenTypeAccessToken},
			}.Encode(), wantCode: "unauthorized_client",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := post(t, f.handler, tt.target, tt.contentType, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body)
			}
			body := decode(t, w)
			if body["error"] != tt.wantCode {
				t.Errorf("error = %v, want %s (body %v)", body["error"], tt.wantCode, body)
			}
			if desc, _ := body["error_description"].(string); desc == "" {
				t.Errorf("error_description is empty in %v", body)
			}
			for k := range body {
				if k != "error" && k != "error_description" {
					t.Errorf("error body has unexpected field %q: %v", k, body)
				}
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if _, ok := body["access_token"]; ok {
				t.Errorf("error response carries an access_token")
			}
		})
	}
}

func TestTokenExchangeGRPC(t *testing.T) {
	t.Parallel()
	f := newApFixture(t, false)
	jwtReq := func() *iampb.ExchangeTokenRequest {
		return &iampb.ExchangeTokenRequest{
			GrantType: domain.GrantTypeTokenExchange, RequestedTokenType: domain.TokenTypeAccessToken,
			SubjectToken: f.assertion(t), SubjectTokenType: domain.TokenTypeJWT,
		}
	}
	tests := []struct {
		name   string
		mutate func(r *iampb.ExchangeTokenRequest)
		want   codes.Code
	}{
		{name: "ok", mutate: func(*iampb.ExchangeTokenRequest) {}, want: codes.OK},
		{name: "bad assertion", mutate: func(r *iampb.ExchangeTokenRequest) { r.SubjectToken = "junk" }, want: codes.Unauthenticated},
		{name: "unsupported grant type", mutate: func(r *iampb.ExchangeTokenRequest) { r.GrantType = "password" }, want: codes.InvalidArgument},
		{name: "scopes", mutate: func(r *iampb.ExchangeTokenRequest) { r.Scopes = []string{"compute"} }, want: codes.InvalidArgument},
		{name: "impersonation denied", mutate: func(r *iampb.ExchangeTokenRequest) {
			r.SubjectToken = apSA
			r.SubjectTokenType = domain.TokenTypeSubjectIdentifier
			r.ActorToken = apActor
			r.ActorTokenType = domain.TokenTypeAccessToken
		}, want: codes.PermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := jwtReq()
			tt.mutate(req)
			resp, err := f.handler.Exchange(t.Context(), req)
			if got := status.Code(err); got != tt.want {
				t.Fatalf("Exchange code = %v, want %v (err %v)", got, tt.want, err)
			}
			if tt.want != codes.OK {
				return
			}
			if resp.GetTokenType() != "Bearer" || resp.GetIssuedTokenType() != domain.TokenTypeAccessToken || resp.GetExpiresIn() != 3600 {
				t.Errorf("response = %v, want Bearer access_token expires_in 3600", resp)
			}
			if c, err := f.issuer.Verify(resp.GetAccessToken()); err != nil || c.Principal != apSA {
				t.Errorf("access_token verify = %+v, %v", c, err)
			}
		})
	}
}
