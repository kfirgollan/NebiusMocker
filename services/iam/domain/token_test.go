package domain_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	txTenant  = "tenant-e00tx0000000000001"
	txProject = "project-e00tx0000000000001"
	txSA      = "serviceaccount-e00tx0000000001"
	txKey     = "publickey-e00tx0000000000001"
	txActor   = "serviceaccount-e00tx0000000002"
	txMaxTTL  = 10 * time.Minute
)

var txT0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// txRSA is generated once: RSA key generation dominates the test time.
var txRSA = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

type txKeyGetter struct {
	mu   sync.Mutex
	keys map[string]*iampb.AuthPublicKey
}

func (f *txKeyGetter) Get(_ context.Context, id string) (*iampb.AuthPublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return k, nil
}

type txSARepo struct {
	items map[string]*iampb.ServiceAccount
}

func (f *txSARepo) Get(_ context.Context, id string) (*iampb.ServiceAccount, error) {
	sa, ok := f.items[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return sa, nil
}

func (f *txSARepo) GetByName(context.Context, string, string) (*iampb.ServiceAccount, error) {
	return nil, errors.New("txSARepo: GetByName not expected")
}

func (f *txSARepo) List(context.Context, string, int, string) (domain.ServiceAccountPage, error) {
	return domain.ServiceAccountPage{}, errors.New("txSARepo: List not expected")
}

func (f *txSARepo) Create(context.Context, *iampb.ServiceAccount) error {
	return errors.New("txSARepo: Create not expected")
}

func (f *txSARepo) Update(context.Context, *iampb.ServiceAccount) error {
	return errors.New("txSARepo: Update not expected")
}

func (f *txSARepo) Delete(context.Context, string) error {
	return errors.New("txSARepo: Delete not expected")
}

type txProjects struct{}

func (txProjects) Get(_ context.Context, id string) (*iampb.Container, error) {
	if id != txProject {
		return nil, errs.NotFound(id)
	}
	return &iampb.Container{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: txTenant}}, nil
}

func (txProjects) Ensure(context.Context, *iampb.Container) error { return nil }

type txPerms struct {
	mu      sync.Mutex
	allowed bool
	calls   []string
}

func (f *txPerms) Check(_ context.Context, principal string, perm authz.Permission, target string) (authz.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, principal+" "+string(perm)+" "+target)
	return authz.Decision{Allowed: f.allowed, Permission: perm, Target: target, Reason: "fake"}, nil
}

func (f *txPerms) numCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type txFixture struct {
	svc    *domain.TokenExchangeService
	issuer *auth.Issuer
	keys   *txKeyGetter
	sas    *txSARepo
	perms  *txPerms
	clk    *clock.Manual
	priv   *rsa.PrivateKey
	// actorToken is a valid static dev token of principal txActor.
	actorToken string
}

func txPEM(t *testing.T, pub *rsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func newTxFixture(t *testing.T, mode authz.Mode) *txFixture {
	t.Helper()
	priv, err := txRSA()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	clk := clock.NewManual(txT0)
	secret, err := auth.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := auth.NewIssuer(secret, time.Hour, clk)
	if err != nil {
		t.Fatal(err)
	}
	f := &txFixture{
		issuer: issuer, clk: clk, priv: priv, actorToken: "static-actor-token",
		keys: &txKeyGetter{keys: map[string]*iampb.AuthPublicKey{txKey: {
			Metadata: &commonpb.ResourceMetadata{Id: txKey},
			Spec: &iampb.AuthPublicKeySpec{
				Account: &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: txSA}}},
				Data:    txPEM(t, &priv.PublicKey),
			},
			Status: &iampb.AuthPublicKeyStatus{State: iampb.AuthPublicKeyStatus_ACTIVE},
		}}},
		sas: &txSARepo{items: map[string]*iampb.ServiceAccount{txSA: {
			Metadata: &commonpb.ResourceMetadata{Id: txSA, ParentId: txProject},
			Status:   &iampb.ServiceAccountStatus{Active: true},
		}}},
		perms: &txPerms{allowed: true},
	}
	svc, err := domain.NewTokenExchangeService(domain.TokenExchangeDeps{
		Keys: f.keys, ServiceAccounts: f.sas, Projects: txProjects{}, Minter: issuer,
		Actors:      auth.NewAuthenticator(map[string]string{f.actorToken: txActor}, issuer),
		Permissions: f.perms, Mode: mode, Clock: clk, MaxAssertionTTL: txMaxTTL,
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewTokenExchangeService: %v", err)
	}
	f.svc = svc
	return f
}

func (f *txFixture) setKeyState(state iampb.AuthPublicKeyStatus_State) {
	f.keys.mu.Lock()
	defer f.keys.mu.Unlock()
	f.keys.keys[txKey].Status.State = state
}

// jwtWith signs an assertion for the fixture's key; mutate edits the claims
// and header (nil keeps a valid assertion).
func (f *txFixture) jwtWith(t *testing.T, mutate func(claims jwt.MapClaims, header map[string]any)) string {
	t.Helper()
	claims := jwt.MapClaims{"iss": txSA, "sub": txSA, "iat": txT0.Unix(), "exp": txT0.Add(5 * time.Minute).Unix()}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = txKey
	if mutate != nil {
		mutate(claims, tok.Header)
	}
	s, err := tok.SignedString(f.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *txFixture) jwtRequest(t *testing.T, mutate func(claims jwt.MapClaims, header map[string]any)) domain.ExchangeRequest {
	t.Helper()
	return domain.ExchangeRequest{
		GrantType: domain.GrantTypeTokenExchange, RequestedTokenType: domain.TokenTypeAccessToken,
		SubjectToken: f.jwtWith(t, mutate), SubjectTokenType: domain.TokenTypeJWT,
	}
}

func impersonationRequest(f *txFixture, saID string) domain.ExchangeRequest {
	return domain.ExchangeRequest{
		GrantType: domain.GrantTypeTokenExchange, RequestedTokenType: domain.TokenTypeAccessToken,
		SubjectToken: saID, SubjectTokenType: domain.TokenTypeSubjectIdentifier,
		ActorToken: f.actorToken, ActorTokenType: domain.TokenTypeAccessToken,
	}
}

// wantExchangeError asserts err is a refused exchange with the code and
// domain error kind, and that its text mentions every substring.
func wantExchangeError(t *testing.T, err error, code domain.OAuthErrorCode, kind errs.Kind, mentions ...string) {
	t.Helper()
	xe, ok := errors.AsType[*domain.ExchangeError](err)
	if !ok {
		t.Fatalf("error = %v (%T), want *domain.ExchangeError", err, err)
	}
	if xe.Code != code {
		t.Errorf("code = %s, want %s (error: %v)", xe.Code, code, err)
	}
	if got := errs.KindOf(err); got != kind {
		t.Errorf("kind = %v, want %v (error: %v)", got, kind, err)
	}
	for _, m := range mentions {
		if !strings.Contains(err.Error(), m) && !strings.Contains(xe.Description(), m) {
			t.Errorf("error %q does not mention %q", err, m)
		}
	}
}

func TestExchangeAssertion(t *testing.T) {
	t.Parallel()
	f := newTxFixture(t, authz.ModeEnforce)
	res, err := f.svc.Exchange(t.Context(), f.jwtRequest(t, nil))
	if err != nil {
		t.Fatalf("Exchange = %v", err)
	}
	if res.IssuedTokenType != domain.TokenTypeAccessToken || res.TokenType != "Bearer" || res.Principal != txSA {
		t.Errorf("result = %+v, want access_token/Bearer for %s", res, txSA)
	}
	if res.ExpiresIn != time.Hour {
		t.Errorf("ExpiresIn = %v, want 1h", res.ExpiresIn)
	}
	c, err := f.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("minted token does not verify: %v", err)
	}
	if c.Principal != txSA || c.Tenant != txTenant || c.KeyID != txKey || c.Actor != "" {
		t.Errorf("claims = %+v, want principal %s tenant %s kid %s no actor", c, txSA, txTenant, txKey)
	}
	if f.perms.numCalls() != 0 {
		t.Errorf("assertion exchange checked permissions %d times, want 0", f.perms.numCalls())
	}
	// Exchanged tokens authenticate like any other.
	if p, err := auth.NewAuthenticator(nil, f.issuer).Authenticate(res.AccessToken); err != nil || p != txSA {
		t.Errorf("Authenticate(exchanged) = %q, %v, want %s", p, err, txSA)
	}
}

func TestExchangeRequestValidation(t *testing.T) {
	t.Parallel()
	f := newTxFixture(t, authz.ModeEnforce)
	tests := []struct {
		name    string
		mutate  func(r *domain.ExchangeRequest)
		code    domain.OAuthErrorCode
		kind    errs.Kind
		mention string
	}{
		{
			name: "unsupported grant type", mutate: func(r *domain.ExchangeRequest) { r.GrantType = "client_credentials" },
			code: domain.OAuthUnsupportedGrantType, kind: errs.KindBadRequest, mention: "grant_type",
		},
		{
			name: "empty grant type", mutate: func(r *domain.ExchangeRequest) { r.GrantType = "" },
			code: domain.OAuthUnsupportedGrantType, kind: errs.KindBadRequest, mention: "grant_type",
		},
		{
			name: "requested token type jwt", mutate: func(r *domain.ExchangeRequest) { r.RequestedTokenType = domain.TokenTypeJWT },
			code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "requested_token_type",
		},
		{
			name: "missing subject token", mutate: func(r *domain.ExchangeRequest) { r.SubjectToken = "" },
			code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "subject_token",
		},
		{
			name: "missing subject token type", mutate: func(r *domain.ExchangeRequest) { r.SubjectTokenType = "" },
			code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "subject_token_type",
		},
		{
			name: "scopes", mutate: func(r *domain.ExchangeRequest) { r.Scopes = []string{"compute"} },
			code: domain.OAuthInvalidScope, kind: errs.KindBadRequest, mention: "scopes",
		},
		{
			name: "audience", mutate: func(r *domain.ExchangeRequest) { r.Audience = "https://example.com" },
			code: domain.OAuthInvalidTarget, kind: errs.KindBadRequest, mention: "audience",
		},
		{
			name: "resource", mutate: func(r *domain.ExchangeRequest) { r.Resource = []string{"https://example.com"} },
			code: domain.OAuthInvalidTarget, kind: errs.KindBadRequest, mention: "resource",
		},
		{
			name: "id_token subject type", mutate: func(r *domain.ExchangeRequest) { r.SubjectTokenType = "urn:ietf:params:oauth:token-type:id_token" },
			code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "federated",
		},
		{name: "actor token with jwt subject", mutate: func(r *domain.ExchangeRequest) {
			r.ActorToken = f.actorToken
			r.ActorTokenType = domain.TokenTypeAccessToken
		}, code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "actor_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := f.jwtRequest(t, nil)
			tt.mutate(&req)
			_, err := f.svc.Exchange(t.Context(), req)
			wantExchangeError(t, err, tt.code, tt.kind, tt.mention)
		})
	}
}

func TestExchangeAssertionRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(claims jwt.MapClaims, header map[string]any)
		setup   func(f *txFixture)
		mention string
	}{
		{
			name: "kid not an auth public key (federated)", mention: "federated",
			mutate: func(_ jwt.MapClaims, h map[string]any) { h["kid"] = "external-issuer-key-1" },
		},
		{
			name: "unknown key", mention: "does not exist",
			mutate: func(_ jwt.MapClaims, h map[string]any) { h["kid"] = "publickey-e00unknown" },
		},
		{name: "missing kid", mutate: func(_ jwt.MapClaims, h map[string]any) { delete(h, "kid") }},
		{name: "inactive key", mention: "INACTIVE", setup: func(f *txFixture) { f.setKeyState(iampb.AuthPublicKeyStatus_INACTIVE) }},
		{name: "expired key", mention: "EXPIRED", setup: func(f *txFixture) { f.setKeyState(iampb.AuthPublicKeyStatus_EXPIRED) }},
		{name: "iss and sub of another service account", mention: txSA, mutate: func(c jwt.MapClaims, _ map[string]any) {
			c["iss"] = "serviceaccount-e00other"
			c["sub"] = "serviceaccount-e00other"
		}},
		{name: "iss differs from sub", mutate: func(c jwt.MapClaims, _ map[string]any) { c["iss"] = "serviceaccount-e00other" }},
		{name: "exp too far", mutate: func(c jwt.MapClaims, _ map[string]any) {
			delete(c, "iat")
			c["exp"] = txT0.Add(txMaxTTL + time.Hour).Unix()
		}},
		{name: "exp passed", mutate: func(c jwt.MapClaims, _ map[string]any) { c["exp"] = txT0.Add(-time.Minute).Unix() }},
		{name: "service account deleted", mention: "does not exist", setup: func(f *txFixture) { delete(f.sas.items, txSA) }},
		{name: "service account inactive", mention: "not active", setup: func(f *txFixture) {
			f.sas.items[txSA].Status.Active = false
		}},
		{name: "not a JWT", mutate: nil, mention: "subject_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newTxFixture(t, authz.ModeEnforce)
			if tt.setup != nil {
				tt.setup(f)
			}
			req := f.jwtRequest(t, tt.mutate)
			if tt.name == "not a JWT" {
				req.SubjectToken = "nmock-dev-admin"
			}
			_, err := f.svc.Exchange(t.Context(), req)
			mentions := []string{}
			if tt.mention != "" {
				mentions = append(mentions, tt.mention)
			}
			wantExchangeError(t, err, domain.OAuthInvalidGrant, errs.KindUnauthenticated, mentions...)
		})
	}
}

func TestExchangeImpersonation(t *testing.T) {
	t.Parallel()
	f := newTxFixture(t, authz.ModeEnforce)
	res, err := f.svc.Exchange(t.Context(), impersonationRequest(f, txSA))
	if err != nil {
		t.Fatalf("Exchange = %v", err)
	}
	c, err := f.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if c.Principal != txSA || c.Actor != txActor || c.Tenant != txTenant || c.KeyID != "" {
		t.Errorf("claims = %+v, want principal %s actor %s tenant %s", c, txSA, txActor, txTenant)
	}
	wantCall := txActor + " " + string(domain.PermImpersonate) + " " + txSA
	if f.perms.numCalls() != 1 || f.perms.calls[0] != wantCall {
		t.Errorf("permission checks = %v, want [%q]", f.perms.calls, wantCall)
	}
	// Revoking the actor revokes the impersonation token.
	f.issuer.RevokePrincipal(txActor)
	if _, err := f.issuer.Verify(res.AccessToken); !errors.Is(err, auth.ErrRevokedToken) {
		t.Errorf("Verify after revoking actor = %v, want ErrRevokedToken", err)
	}
}

func TestExchangeImpersonationWithIssuedActorToken(t *testing.T) {
	t.Parallel()
	f := newTxFixture(t, authz.ModeEnforce)
	first, err := f.svc.Exchange(t.Context(), f.jwtRequest(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	// The service account's own issued token impersonates another service account.
	f.sas.items[txActor] = &iampb.ServiceAccount{
		Metadata: &commonpb.ResourceMetadata{Id: txActor, ParentId: txProject},
		Status:   &iampb.ServiceAccountStatus{Active: true},
	}
	req := impersonationRequest(f, txActor)
	req.ActorToken = first.AccessToken
	res, err := f.svc.Exchange(t.Context(), req)
	if err != nil {
		t.Fatalf("Exchange = %v", err)
	}
	c, err := f.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if c.Principal != txActor || c.Actor != txSA {
		t.Errorf("claims = %+v, want principal %s actor %s", c, txActor, txSA)
	}
}

func TestExchangeImpersonationRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(r *domain.ExchangeRequest)
		setup   func(f *txFixture)
		code    domain.OAuthErrorCode
		kind    errs.Kind
		mention string
	}{
		{
			name: "missing actor token", mutate: func(r *domain.ExchangeRequest) { r.ActorToken = "" },
			code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "actor_token",
		},
		{
			name: "wrong actor token type", mutate: func(r *domain.ExchangeRequest) { r.ActorTokenType = domain.TokenTypeJWT },
			code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "actor_token_type",
		},
		{
			name: "missing actor token type", mutate: func(r *domain.ExchangeRequest) { r.ActorTokenType = "" },
			code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "actor_token_type",
		},
		{
			name: "invalid actor token", mutate: func(r *domain.ExchangeRequest) { r.ActorToken = "no-such-token" },
			code: domain.OAuthInvalidGrant, kind: errs.KindUnauthenticated, mention: "actor_token",
		},
		{
			name: "subject not a service account id", mutate: func(r *domain.ExchangeRequest) { r.SubjectToken = "user-e00x" },
			code: domain.OAuthInvalidRequest, kind: errs.KindBadRequest, mention: "subject_token",
		},
		{
			name: "unknown service account", mutate: func(r *domain.ExchangeRequest) { r.SubjectToken = "serviceaccount-e00missing" },
			code: domain.OAuthInvalidGrant, kind: errs.KindUnauthenticated, mention: "does not exist",
		},
		{
			name: "inactive service account", setup: func(f *txFixture) { f.sas.items[txSA].Status.Active = false },
			code: domain.OAuthInvalidGrant, kind: errs.KindUnauthenticated, mention: "not active",
		},
		{
			name: "permission denied", setup: func(f *txFixture) { f.perms.allowed = false },
			code: domain.OAuthUnauthorizedClient, kind: errs.KindPermissionDenied, mention: txActor,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newTxFixture(t, authz.ModeEnforce)
			if tt.setup != nil {
				tt.setup(f)
			}
			req := impersonationRequest(f, txSA)
			if tt.mutate != nil {
				tt.mutate(&req)
			}
			_, err := f.svc.Exchange(t.Context(), req)
			wantExchangeError(t, err, tt.code, tt.kind, tt.mention)
		})
	}
}

func TestExchangeImpersonationAuthzModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		mode      authz.Mode
		allowed   bool
		wantErr   bool
		wantCalls int
	}{
		{name: "enforce allowed", mode: authz.ModeEnforce, allowed: true, wantCalls: 1},
		{name: "enforce denied", mode: authz.ModeEnforce, allowed: false, wantErr: true, wantCalls: 1},
		{name: "audit denied still allowed", mode: authz.ModeAudit, allowed: false, wantCalls: 1},
		{name: "audit allowed", mode: authz.ModeAudit, allowed: true, wantCalls: 1},
		{name: "off skips the check", mode: authz.ModeOff, allowed: false, wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newTxFixture(t, tt.mode)
			f.perms.allowed = tt.allowed
			res, err := f.svc.Exchange(t.Context(), impersonationRequest(f, txSA))
			if tt.wantErr {
				wantExchangeError(t, err, domain.OAuthUnauthorizedClient, errs.KindPermissionDenied)
			} else if err != nil || res.Principal != txSA {
				t.Errorf("Exchange = %+v, %v, want a token for %s", res, err, txSA)
			}
			if got := f.perms.numCalls(); got != tt.wantCalls {
				t.Errorf("permission checks = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestNewTokenExchangeServiceRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewTokenExchangeService(domain.TokenExchangeDeps{}); err == nil {
		t.Errorf("NewTokenExchangeService with no deps succeeded")
	}
	f := newTxFixture(t, authz.ModeOff)
	_, err := domain.NewTokenExchangeService(domain.TokenExchangeDeps{
		Keys: f.keys, ServiceAccounts: f.sas, Projects: txProjects{}, Minter: f.issuer,
		Actors: auth.NewAuthenticator(nil, f.issuer), Permissions: f.perms, Clock: f.clk,
		Log: slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Errorf("NewTokenExchangeService without MaxAssertionTTL succeeded")
	}
}
