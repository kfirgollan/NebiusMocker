package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
)

var (
	extRSA = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })
	extEC  = sync.OnceValues(func() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) })
	extT0  = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
)

func extKeys(t *testing.T) (rsaKey *rsa.PrivateKey, ecKey *ecdsa.PrivateKey) {
	t.Helper()
	r, err := extRSA()
	if err != nil {
		t.Fatal(err)
	}
	e, err := extEC()
	if err != nil {
		t.Fatal(err)
	}
	return r, e
}

func extSign(t *testing.T, m jwt.SigningMethod, key any, kid string, edit func(jwt.MapClaims)) string {
	t.Helper()
	c := jwt.MapClaims{"iss": "https://issuer.example", "sub": "ci", "iat": extT0.Unix(), "exp": extT0.Add(time.Hour).Unix()}
	if edit != nil {
		edit(c)
	}
	tok := jwt.NewWithClaims(m, c)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWKSetRoundTrip(t *testing.T) {
	t.Parallel()
	r, e := extKeys(t)
	doc, err := auth.JWKSet{Keys: []auth.JWK{{KeyID: "r", Alg: "RS256", Key: &r.PublicKey}, {KeyID: "e", Key: &e.PublicKey}}}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	set, err := auth.ParseJWKSet(doc)
	if err != nil || len(set.Keys) != 2 {
		t.Fatalf("ParseJWKSet = %+v, %v", set, err)
	}
	if !set.Keys[0].Key.(*rsa.PublicKey).Equal(&r.PublicKey) || !set.Keys[1].Key.(*ecdsa.PublicKey).Equal(&e.PublicKey) {
		t.Error("keys changed in the round trip")
	}
	if set.Keys[0].KeyID != "r" || set.Keys[0].Alg != "RS256" || set.Keys[1].KeyID != "e" {
		t.Errorf("key metadata = %+v", set.Keys)
	}
}

func TestParseJWKSetSkipsAndRefuses(t *testing.T) {
	t.Parallel()
	r, _ := extKeys(t)
	good, err := auth.JWKSet{Keys: []auth.JWK{{KeyID: "r", Key: &r.PublicKey}}}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var withExtras map[string][]map[string]any
	if err := json.Unmarshal(good, &withExtras); err != nil {
		t.Fatal(err)
	}
	withExtras["keys"] = append(withExtras["keys"],
		map[string]any{"kty": "OKP", "crv": "Ed25519", "x": "AA"},
		map[string]any{"kty": "EC", "crv": "P-384", "x": "AA", "y": "AA"},
		map[string]any{"kty": "RSA", "use": "enc", "n": "AA", "e": "AQAB"},
		map[string]any{"kty": "RSA", "alg": "PS256", "n": "AA", "e": "AQAB"},
	)
	mixed, err := json.Marshal(withExtras)
	if err != nil {
		t.Fatal(err)
	}
	if set, err := auth.ParseJWKSet(mixed); err != nil || len(set.Keys) != 1 {
		t.Errorf("mixed set = %+v, %v, want the one RSA signing key", set, err)
	}
	for name, doc := range map[string]string{
		"not JSON":         "nope",
		"no keys":          `{"keys":[]}`,
		"only unsupported": `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AA"}]}`,
		"short RSA":        `{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB"}]}`,
		"RSA without e":    `{"keys":[{"kty":"RSA","n":"AQAB"}]}`,
		"EC off curve":     `{"keys":[{"kty":"EC","crv":"P-256","x":"` + strings.Repeat("A", 43) + `","y":"` + strings.Repeat("A", 43) + `"}]}`,
		"EC bad base64":    `{"keys":[{"kty":"EC","crv":"P-256","x":"!!","y":"!!"}]}`,
	} {
		if _, err := auth.ParseJWKSet([]byte(doc)); !errors.Is(err, auth.ErrInvalidJWKSet) {
			t.Errorf("%s: %v, want ErrInvalidJWKSet", name, err)
		}
	}
}

func TestVerifyExternalJWT(t *testing.T) {
	t.Parallel()
	r, e := extKeys(t)
	set := auth.JWKSet{Keys: []auth.JWK{{KeyID: "r", Key: &r.PublicKey}, {KeyID: "e", Key: &e.PublicKey}}}
	for name, tok := range map[string]string{
		"RS256": extSign(t, jwt.SigningMethodRS256, r, "r", nil),
		"ES256": extSign(t, jwt.SigningMethodES256, e, "e", nil),
		"skew":  extSign(t, jwt.SigningMethodRS256, r, "r", func(c jwt.MapClaims) { c["iat"] = extT0.Add(20 * time.Second).Unix() }),
	} {
		j, err := auth.ParseExternalJWT(tok)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if err := auth.VerifyExternalJWT(j, set, extT0); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Without kid, a one-key set is enough.
	j, _ := auth.ParseExternalJWT(extSign(t, jwt.SigningMethodES256, e, "", nil))
	if err := auth.VerifyExternalJWT(j, auth.JWKSet{Keys: set.Keys[1:]}, extT0); err != nil {
		t.Errorf("no kid, one key: %v", err)
	}
}

func TestVerifyExternalJWTRefusals(t *testing.T) {
	t.Parallel()
	r, e := extKeys(t)
	set := auth.JWKSet{Keys: []auth.JWK{{KeyID: "r", Key: &r.PublicKey}, {KeyID: "e", Key: &e.PublicKey}, {KeyID: "pinned", Alg: "ES256", Key: &r.PublicKey}}}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		token   string
		mention string
		unknown bool
	}{
		"unknown kid":    {extSign(t, jwt.SigningMethodRS256, r, "zzz", nil), "kid", true},
		"no kid":         {extSign(t, jwt.SigningMethodRS256, r, "", nil), "kid is required", false},
		"wrong key type": {extSign(t, jwt.SigningMethodES256, e, "r", nil), "cannot verify", false},
		"alg pinned":     {extSign(t, jwt.SigningMethodRS256, r, "pinned", nil), "for alg ES256", false},
		"bad signature":  {extSign(t, jwt.SigningMethodRS256, other, "r", nil), "verification", false},
		"expired":        {extSign(t, jwt.SigningMethodRS256, r, "r", func(c jwt.MapClaims) { c["exp"] = extT0.Unix() }), "expired", false},
		"no exp":         {extSign(t, jwt.SigningMethodRS256, r, "r", func(c jwt.MapClaims) { delete(c, "exp") }), "exp is required", false},
		"iat future":     {extSign(t, jwt.SigningMethodRS256, r, "r", func(c jwt.MapClaims) { c["iat"] = extT0.Add(time.Minute).Unix() }), "iat", false},
		"nbf future":     {extSign(t, jwt.SigningMethodRS256, r, "r", func(c jwt.MapClaims) { c["nbf"] = extT0.Add(time.Minute).Unix() }), "nbf", false},
	} {
		j, err := auth.ParseExternalJWT(tt.token)
		if err != nil {
			t.Errorf("%s: parse: %v", name, err)
			continue
		}
		err = auth.VerifyExternalJWT(j, set, extT0)
		if !errors.Is(err, auth.ErrInvalidExternalJWT) || !strings.Contains(err.Error(), tt.mention) {
			t.Errorf("%s: %v, want ErrInvalidExternalJWT mentioning %q", name, err, tt.mention)
		}
		if errors.Is(err, auth.ErrUnknownKeyID) != tt.unknown {
			t.Errorf("%s: ErrUnknownKeyID = %v, want %v", name, errors.Is(err, auth.ErrUnknownKeyID), tt.unknown)
		}
	}
}

func TestParseExternalJWTRefusals(t *testing.T) {
	t.Parallel()
	r, _ := extKeys(t)
	ps, err := jwt.NewWithClaims(jwt.SigningMethodPS256, jwt.MapClaims{"iss": "i", "sub": "s"}).SignedString(r)
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct{ token, mention string }{
		"garbage": {"a.b.c", ""},
		"HS256":   {extSign(t, jwt.SigningMethodHS256, []byte("0123456789abcdef0123456789abcdef"), "", nil), `"HS256" is not supported`},
		"none":    {extSign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "", nil), `"none" is not supported`},
		"PS256":   {ps, `"PS256" is not supported`},
		"no sub":  {extSign(t, jwt.SigningMethodRS256, r, "", func(c jwt.MapClaims) { delete(c, "sub") }), "iss and sub"},
	} {
		if _, err := auth.ParseExternalJWT(tt.token); !errors.Is(err, auth.ErrInvalidExternalJWT) || !strings.Contains(err.Error(), tt.mention) {
			t.Errorf("%s: %v, want ErrInvalidExternalJWT mentioning %q", name, err, tt.mention)
		}
	}
}
