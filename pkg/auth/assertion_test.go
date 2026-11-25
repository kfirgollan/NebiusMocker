package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
)

const (
	testSA     = "serviceaccount-e00test"
	testKID    = "publickey-e00test"
	assertTTL  = 10 * time.Minute
	assertSkew = 30 * time.Second
)

// claimsAt returns valid claims for the assertion tests: issued at t0,
// expiring after 5 minutes.
func claimsAt() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": testSA, "sub": testSA,
		"iat": t0.Unix(), "exp": t0.Add(5 * time.Minute).Unix(),
	}
}

// signJWT signs claims with method and key and the given kid header ("" omits it).
func signJWT(t *testing.T, method jwt.SigningMethod, kid string, claims jwt.MapClaims, key any) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign %s: %v", method.Alg(), err)
	}
	return s
}

func with(c jwt.MapClaims, mutate func(jwt.MapClaims)) jwt.MapClaims {
	out := jwt.MapClaims{}
	for k, v := range c {
		out[k] = v
	}
	mutate(out)
	return out
}

// verify parses and verifies token against key at t0; the error is nil only
// if both steps pass.
func verify(token string, key *rsa.PublicKey) (auth.Assertion, error) {
	a, err := auth.ParseAssertion(token)
	if err != nil {
		return auth.Assertion{}, err
	}
	return a, auth.VerifyAssertion(a, key, auth.AssertionPolicy{Now: t0, MaxTTL: assertTTL})
}

func TestAssertionValid(t *testing.T) {
	t.Parallel()
	key := genRSA(t, 2048)
	tests := []struct {
		name   string
		claims jwt.MapClaims
	}{
		{name: "baseline", claims: claimsAt()},
		{name: "no iat", claims: with(claimsAt(), func(c jwt.MapClaims) { delete(c, "iat") })},
		{name: "exp at max ttl", claims: with(claimsAt(), func(c jwt.MapClaims) { c["exp"] = t0.Add(assertTTL).Unix() })},
		{name: "exp within leeway of max ttl", claims: with(claimsAt(), func(c jwt.MapClaims) {
			c["exp"] = t0.Add(assertTTL + assertSkew).Unix()
		})},
		{name: "iat within leeway", claims: with(claimsAt(), func(c jwt.MapClaims) { c["iat"] = t0.Add(assertSkew).Unix() })},
		{name: "nbf within leeway", claims: with(claimsAt(), func(c jwt.MapClaims) { c["nbf"] = t0.Add(assertSkew).Unix() })},
		{name: "exp one second ahead", claims: with(claimsAt(), func(c jwt.MapClaims) { c["exp"] = t0.Add(time.Second).Unix() })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tok := signJWT(t, jwt.SigningMethodRS256, testKID, tt.claims, key)
			a, err := verify(tok, &key.PublicKey)
			if err != nil {
				t.Fatalf("verify(%s) = %v, want nil", tt.name, err)
			}
			if a.KeyID != testKID || a.Issuer != testSA || a.Subject != testSA {
				t.Errorf("Assertion = %+v, want kid %s iss=sub=%s", a, testKID, testSA)
			}
		})
	}
}

func TestAssertionRejectsClaims(t *testing.T) {
	t.Parallel()
	key := genRSA(t, 2048)
	tests := []struct {
		name   string
		claims jwt.MapClaims
	}{
		{name: "missing exp", claims: with(claimsAt(), func(c jwt.MapClaims) { delete(c, "exp") })},
		{name: "exp in the past", claims: with(claimsAt(), func(c jwt.MapClaims) { c["exp"] = t0.Add(-time.Minute).Unix() })},
		{name: "exp equals now", claims: with(claimsAt(), func(c jwt.MapClaims) { c["exp"] = t0.Unix() })},
		{name: "exp beyond max ttl", claims: with(claimsAt(), func(c jwt.MapClaims) {
			c["exp"] = t0.Add(assertTTL + assertSkew + time.Second).Unix()
			delete(c, "iat")
		})},
		{name: "iat in the future", claims: with(claimsAt(), func(c jwt.MapClaims) {
			c["iat"] = t0.Add(assertSkew + time.Second).Unix()
		})},
		{name: "nbf in the future", claims: with(claimsAt(), func(c jwt.MapClaims) {
			c["nbf"] = t0.Add(assertSkew + time.Second).Unix()
		})},
		{name: "exp minus iat beyond max ttl", claims: with(claimsAt(), func(c jwt.MapClaims) {
			c["iat"] = t0.Add(-time.Hour).Unix()
			c["exp"] = t0.Add(5 * time.Minute).Unix()
		})},
		{name: "iss differs from sub", claims: with(claimsAt(), func(c jwt.MapClaims) { c["iss"] = "serviceaccount-other" })},
		{name: "missing iss", claims: with(claimsAt(), func(c jwt.MapClaims) { delete(c, "iss") })},
		{name: "missing sub", claims: with(claimsAt(), func(c jwt.MapClaims) { delete(c, "sub") })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tok := signJWT(t, jwt.SigningMethodRS256, testKID, tt.claims, key)
			if _, err := verify(tok, &key.PublicKey); !errors.Is(err, auth.ErrInvalidAssertion) {
				t.Fatalf("verify(%s) = %v, want ErrInvalidAssertion", tt.name, err)
			}
		})
	}
}

func TestAssertionRejectsAlgorithmsAndForgeries(t *testing.T) {
	t.Parallel()
	key := genRSA(t, 2048)
	other := genRSA(t, 2048)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := []byte(pkixPEM(t, &key.PublicKey))
	valid := signJWT(t, jwt.SigningMethodRS256, testKID, claimsAt(), key)
	parts := strings.Split(valid, ".")
	// Valid header and signature with different claims.
	forged := signJWT(t, jwt.SigningMethodRS256, testKID, with(claimsAt(), func(c jwt.MapClaims) { c["sub"] = "serviceaccount-evil" }), other)
	forgedParts := strings.Split(forged, ".")

	tests := []struct {
		name  string
		token string
	}{
		{name: "alg none", token: signJWT(t, jwt.SigningMethodNone, testKID, claimsAt(), jwt.UnsafeAllowNoneSignatureType)},
		{name: "HS256 with PEM public key as secret", token: signJWT(t, jwt.SigningMethodHS256, testKID, claimsAt(), pubPEM)},
		{name: "RS512", token: signJWT(t, jwt.SigningMethodRS512, testKID, claimsAt(), key)},
		{name: "PS256", token: signJWT(t, jwt.SigningMethodPS256, testKID, claimsAt(), key)},
		{name: "ES256", token: signJWT(t, jwt.SigningMethodES256, testKID, claimsAt(), ec)},
		{name: "missing kid", token: signJWT(t, jwt.SigningMethodRS256, "", claimsAt(), key)},
		{name: "signed by another key", token: signJWT(t, jwt.SigningMethodRS256, testKID, claimsAt(), other)},
		{name: "tampered claims", token: parts[0] + "." + forgedParts[1] + "." + parts[2]},
		{name: "tampered signature", token: parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-4] + "AAAA"},
		{name: "not a JWT", token: "nmock-dev-admin"},
		{name: "empty", token: ""},
		{name: "three garbage segments", token: "a.b.c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := verify(tt.token, &key.PublicKey); !errors.Is(err, auth.ErrInvalidAssertion) {
				t.Fatalf("verify(%s) = %v, want ErrInvalidAssertion", tt.name, err)
			}
		})
	}
}

func TestParseAssertionReadsHeaderWithoutVerifying(t *testing.T) {
	t.Parallel()
	signer := genRSA(t, 2048)
	tok := signJWT(t, jwt.SigningMethodRS256, testKID, claimsAt(), signer)
	a, err := auth.ParseAssertion(tok)
	if err != nil {
		t.Fatalf("ParseAssertion = %v", err)
	}
	if a.KeyID != testKID || a.Issuer != testSA || a.Subject != testSA {
		t.Errorf("Assertion = %+v", a)
	}
	// A key that did not sign the token must still fail at VerifyAssertion.
	wrong := genRSA(t, 2048)
	if err := auth.VerifyAssertion(a, &wrong.PublicKey, auth.AssertionPolicy{Now: t0, MaxTTL: assertTTL}); !errors.Is(err, auth.ErrInvalidAssertion) {
		t.Errorf("VerifyAssertion with wrong key = %v, want ErrInvalidAssertion", err)
	}
}
