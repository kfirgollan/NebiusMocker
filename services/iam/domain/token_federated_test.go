package domain_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	fxIssuer = "https://issuer.example/ci"
	fxSub    = "repo:org/app:ref:main"
	fxCred   = "federatedcredentials-e00tx00000001"
	fxCred2  = "federatedcredentials-e00tx00000002"
	fxKid    = "issuer-key-1"
)

var (
	// fxOtherRSA signs tokens no credential trusts.
	fxOtherRSA = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })
	fxEC       = sync.OnceValues(func() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) })
)

type txFedCreds struct {
	mu    sync.Mutex
	items []*iampb.FederatedCredentials
	err   error
}

func (f *txFedCreds) ListBySubject(_ context.Context, saID string) ([]*iampb.FederatedCredentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []*iampb.FederatedCredentials
	for _, c := range f.items {
		if c.GetSpec().GetSubjectId() == saID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *txFedCreds) add(c *iampb.FederatedCredentials) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, c)
}

// txOIDC serves JWKS documents per issuer: stale ones until refreshed.
type txOIDC struct {
	mu    sync.Mutex
	docs  map[string][]byte
	stale map[string][]byte
	err   error
	calls []string
}

func (f *txOIDC) KeySet(_ context.Context, issuerURL string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, issuerURL+" refresh=false")
	if doc, ok := f.stale[issuerURL]; ok && f.err == nil {
		return doc, nil
	}
	return f.currentLocked(issuerURL)
}

func (f *txOIDC) RefreshKeySet(_ context.Context, issuerURL string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, issuerURL+" refresh=true")
	return f.currentLocked(issuerURL)
}

func (f *txOIDC) currentLocked(issuerURL string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	doc, ok := f.docs[issuerURL]
	if !ok {
		return nil, errors.New("GET " + issuerURL + "/.well-known/openid-configuration: HTTP 404")
	}
	return doc, nil
}

func (f *txOIDC) numCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func fxJWKS(t *testing.T, keys ...auth.JWK) []byte {
	t.Helper()
	b, err := auth.JWKSet{Keys: keys}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fxCredentials(id, issuer, jwks string) *iampb.FederatedCredentials {
	return &iampb.FederatedCredentials{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: txProject},
		Spec: &iampb.FederatedCredentialsSpec{
			CredentialsProvider: &iampb.FederatedCredentialsSpec_OidcProvider{OidcProvider: &iampb.OidcCredentialsProvider{
				IssuerUrl: issuer, JwkSetJson: jwks,
			}},
			FederatedSubjectId: fxSub, SubjectId: txSA,
		},
	}
}

// newFederatedTxFixture is a token-exchange fixture with one credential of txSA
// trusting fxIssuer/fxSub through discovery, whose JWKS holds the fixture's
// RSA key as fxKid.
func newFederatedTxFixture(t *testing.T) *txFixture {
	t.Helper()
	f := newTxFixture(t, authz.ModeEnforce)
	f.fed.add(fxCredentials(fxCred, fxIssuer, ""))
	f.oidc.docs[fxIssuer] = fxJWKS(t, auth.JWK{KeyID: fxKid, Key: &f.priv.PublicKey})
	return f
}

func fxSign(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fxClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": fxIssuer, "sub": fxSub, "aud": "anything-at-all",
		"iat": txT0.Unix(), "exp": txT0.Add(10 * time.Minute).Unix(),
	}
}

// fedJWT signs fxClaims (edited by mutate) with the fixture's RSA key as fxKid.
func (f *txFixture) fedJWT(t *testing.T, mutate func(c jwt.MapClaims)) string {
	t.Helper()
	c := fxClaims()
	if mutate != nil {
		mutate(c)
	}
	return fxSign(t, jwt.SigningMethodRS256, f.priv, fxKid, c)
}

func federatedRequest(saID, token string) domain.ExchangeRequest {
	return domain.ExchangeRequest{
		GrantType: domain.GrantTypeTokenExchange, RequestedTokenType: domain.TokenTypeAccessToken,
		SubjectToken: saID, SubjectTokenType: domain.TokenTypeSubjectIdentifier,
		ActorToken: token, ActorTokenType: domain.TokenTypeJWT,
	}
}

func (f *txFixture) wantFederatedGrant(t *testing.T, req domain.ExchangeRequest, credID string) auth.Claims {
	t.Helper()
	res, err := f.svc.Exchange(t.Context(), req)
	if err != nil {
		t.Fatalf("Exchange = %v", err)
	}
	c, err := f.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("minted token does not verify: %v", err)
	}
	if res.Principal != txSA || c.Principal != txSA || c.Tenant != txTenant || c.KeyID != credID || c.Actor != "" {
		t.Errorf("claims = %+v, want principal %s tenant %s kid %s", c, txSA, txTenant, credID)
	}
	return c
}

func TestExchangeFederatedDiscovery(t *testing.T) {
	t.Parallel()
	f := newFederatedTxFixture(t)
	// aud is not checked, and a trailing newline from a token file is tolerated.
	c := f.wantFederatedGrant(t, federatedRequest(txSA, f.fedJWT(t, nil)+"\n"), fxCred)
	if f.oidc.numCalls() != 1 || f.oidc.calls[0] != fxIssuer+" refresh=false" {
		t.Errorf("OIDC calls = %v, want one cached fetch of %s", f.oidc.calls, fxIssuer)
	}
	if f.perms.numCalls() != 0 {
		t.Errorf("federated exchange checked permissions %d times, want 0", f.perms.numCalls())
	}
	// Deleting or changing the credentials revokes its tokens (RevokeKey on its ID).
	tok, err := f.svc.Exchange(t.Context(), federatedRequest(txSA, f.fedJWT(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.issuer.RevokeKey(c.KeyID)
	if _, err := f.issuer.Verify(tok.AccessToken); !errors.Is(err, auth.ErrRevokedToken) {
		t.Errorf("Verify after RevokeKey(credential) = %v, want ErrRevokedToken", err)
	}
}

func TestExchangeFederatedInlineJWKS(t *testing.T) {
	t.Parallel()
	ec, err := fxEC()
	if err != nil {
		t.Fatal(err)
	}
	f := newTxFixture(t, authz.ModeEnforce)
	// A one-key set: the token needs no kid. The issuer is never contacted.
	f.fed.add(fxCredentials(fxCred, fxIssuer, string(fxJWKS(t, auth.JWK{Key: &ec.PublicKey}))))
	f.wantFederatedGrant(t, federatedRequest(txSA, fxSign(t, jwt.SigningMethodES256, ec, "", fxClaims())), fxCred)
	if f.oidc.numCalls() != 0 {
		t.Errorf("OIDC calls = %v, want none with jwk_set_json", f.oidc.calls)
	}
}

func TestExchangeFederatedIssuerTrailingSlash(t *testing.T) {
	t.Parallel()
	f := newTxFixture(t, authz.ModeEnforce)
	f.fed.add(fxCredentials(fxCred, fxIssuer+"/", string(fxJWKS(t, auth.JWK{KeyID: fxKid, Key: &f.priv.PublicKey}))))
	f.wantFederatedGrant(t, federatedRequest(txSA, f.fedJWT(t, nil)), fxCred)
}

func TestExchangeFederatedRefetchOnUnknownKid(t *testing.T) {
	t.Parallel()
	other, err := fxOtherRSA()
	if err != nil {
		t.Fatal(err)
	}
	f := newFederatedTxFixture(t)
	// The cached set predates the issuer's key rotation.
	f.oidc.stale = map[string][]byte{fxIssuer: fxJWKS(t, auth.JWK{KeyID: "old-key", Key: &other.PublicKey})}
	f.wantFederatedGrant(t, federatedRequest(txSA, f.fedJWT(t, nil)), fxCred)
	want := []string{fxIssuer + " refresh=false", fxIssuer + " refresh=true"}
	if fmt.Sprint(f.oidc.calls) != fmt.Sprint(want) {
		t.Errorf("OIDC calls = %v, want %v", f.oidc.calls, want)
	}
}

func TestExchangeFederatedTriesEveryMatchingCredential(t *testing.T) {
	t.Parallel()
	other, err := fxOtherRSA()
	if err != nil {
		t.Fatal(err)
	}
	f := newTxFixture(t, authz.ModeEnforce)
	f.fed.add(fxCredentials(fxCred, fxIssuer, string(fxJWKS(t, auth.JWK{KeyID: fxKid, Key: &other.PublicKey}))))
	f.fed.add(fxCredentials(fxCred2, fxIssuer, string(fxJWKS(t, auth.JWK{KeyID: fxKid, Key: &f.priv.PublicKey}))))
	f.wantFederatedGrant(t, federatedRequest(txSA, f.fedJWT(t, nil)), fxCred2)
}

func TestExchangeFederatedRefusals(t *testing.T) {
	t.Parallel()
	other, err := fxOtherRSA()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		token   func(t *testing.T, f *txFixture) string
		setup   func(t *testing.T, f *txFixture)
		mention string
	}{
		{name: "other issuer", mention: "trust issuer", token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return f.fedJWT(t, func(c jwt.MapClaims) { c["iss"] = "https://evil.example" })
		}},
		{name: "issuer differs by more than a slash", mention: "trust issuer", token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return f.fedJWT(t, func(c jwt.MapClaims) { c["iss"] = fxIssuer + "//" })
		}},
		{name: "other subject", mention: "do not trust subject", token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return f.fedJWT(t, func(c jwt.MapClaims) { c["sub"] = "repo:org/app:ref:feature" })
		}},
		{name: "no credentials for the service account", mention: "trust issuer", setup: func(_ *testing.T, f *txFixture) {
			f.fed.items[0].Spec.SubjectId = txActor
		}},
		{name: "expired", mention: "expired", token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return f.fedJWT(t, func(c jwt.MapClaims) { c["exp"] = txT0.Add(-time.Second).Unix() })
		}},
		{name: "no exp", mention: "exp is required", token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return f.fedJWT(t, func(c jwt.MapClaims) { delete(c, "exp") })
		}},
		{name: "iat in the future", mention: "iat", token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return f.fedJWT(t, func(c jwt.MapClaims) { c["iat"] = txT0.Add(time.Minute).Unix() })
		}},
		{name: "nbf in the future", mention: "nbf", token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return f.fedJWT(t, func(c jwt.MapClaims) { c["nbf"] = txT0.Add(time.Minute).Unix() })
		}},
		{name: "signed by another key", mention: "verification", token: func(t *testing.T, _ *txFixture) string {
			t.Helper()
			return fxSign(t, jwt.SigningMethodRS256, other, fxKid, fxClaims())
		}},
		{name: "unknown kid after refetch", mention: "kid", token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return fxSign(t, jwt.SigningMethodRS256, f.priv, "rotated-away", fxClaims())
		}},
		{name: "no kid with a two-key set", mention: "kid is required", setup: func(t *testing.T, f *txFixture) {
			t.Helper()
			f.oidc.docs[fxIssuer] = fxJWKS(t, auth.JWK{KeyID: fxKid, Key: &f.priv.PublicKey}, auth.JWK{KeyID: "b", Key: &other.PublicKey})
		}, token: func(t *testing.T, f *txFixture) string {
			t.Helper()
			return fxSign(t, jwt.SigningMethodRS256, f.priv, "", fxClaims())
		}},
		{name: "ES256 token against an RSA key", mention: "cannot verify", token: func(t *testing.T, _ *txFixture) string {
			t.Helper()
			ec, err := fxEC()
			if err != nil {
				t.Fatal(err)
			}
			return fxSign(t, jwt.SigningMethodES256, ec, fxKid, fxClaims())
		}},
		{name: "issuer unreachable", mention: "cannot fetch", setup: func(_ *testing.T, f *txFixture) {
			f.oidc.err = errors.New("connection refused")
		}},
		{name: "issuer JWKS unusable", mention: "unusable", setup: func(_ *testing.T, f *txFixture) {
			f.oidc.docs[fxIssuer] = []byte(`{"keys":[]}`)
		}},
		{name: "service account deleted", mention: "does not exist", setup: func(_ *testing.T, f *txFixture) {
			delete(f.sas.items, txSA)
		}},
		{name: "service account inactive", mention: "not active", setup: func(_ *testing.T, f *txFixture) {
			f.sas.items[txSA].Status.Active = false
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFederatedTxFixture(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			token := f.fedJWT(t, nil)
			if tt.token != nil {
				token = tt.token(t, f)
			}
			_, err := f.svc.Exchange(t.Context(), federatedRequest(txSA, token))
			wantExchangeError(t, err, domain.OAuthInvalidGrant, errs.KindUnauthenticated, tt.mention)
		})
	}
}

func TestExchangeFederatedMalformed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		req     func(t *testing.T, f *txFixture) domain.ExchangeRequest
		mention string
	}{
		{name: "not a JWT", mention: "actor_token", req: func(_ *testing.T, _ *txFixture) domain.ExchangeRequest {
			return federatedRequest(txSA, "not-a-jwt")
		}},
		{name: "empty actor token", mention: "actor_token", req: func(_ *testing.T, _ *txFixture) domain.ExchangeRequest {
			return federatedRequest(txSA, "")
		}},
		{name: "HS256 refused by name", mention: `"HS256" is not supported`, req: func(t *testing.T, _ *txFixture) domain.ExchangeRequest {
			t.Helper()
			return federatedRequest(txSA, fxSign(t, jwt.SigningMethodHS256, []byte("shared-secret-shared-secret-1234"), fxKid, fxClaims()))
		}},
		{name: "alg none refused by name", mention: `"none" is not supported`, req: func(t *testing.T, _ *txFixture) domain.ExchangeRequest {
			t.Helper()
			return federatedRequest(txSA, fxSign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, fxKid, fxClaims()))
		}},
		{name: "no iss", mention: "iss and sub", req: func(t *testing.T, f *txFixture) domain.ExchangeRequest {
			t.Helper()
			return federatedRequest(txSA, f.fedJWT(t, func(c jwt.MapClaims) { delete(c, "iss") }))
		}},
		{name: "subject token not a service account", mention: "subject_token", req: func(t *testing.T, f *txFixture) domain.ExchangeRequest {
			t.Helper()
			return federatedRequest("user-e00x", f.fedJWT(t, nil))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFederatedTxFixture(t)
			_, err := f.svc.Exchange(t.Context(), tt.req(t, f))
			wantExchangeError(t, err, domain.OAuthInvalidRequest, errs.KindBadRequest, tt.mention)
		})
	}
}

func TestExchangeFederatedLookupFailure(t *testing.T) {
	t.Parallel()
	f := newFederatedTxFixture(t)
	f.fed.err = errors.New("store down")
	_, err := f.svc.Exchange(t.Context(), federatedRequest(txSA, f.fedJWT(t, nil)))
	if _, refused := errors.AsType[*domain.ExchangeError](err); err == nil || refused {
		t.Errorf("Exchange with a failing store = %v, want an internal (non-refusal) error", err)
	}
}
