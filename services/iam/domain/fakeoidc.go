package domain

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// DefaultFakeOIDCTokenTTL is the lifetime of a fake-issuer token minted
// without a ttl.
const DefaultFakeOIDCTokenTTL = time.Hour

// fakeOIDCName is the shape of a fake issuer name (one URL path segment).
var fakeOIDCName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// FakeOIDCIssuers is NebiusMocker's built-in OIDC issuer, for exercising
// federated credentials without a real identity provider: every name is an
// issuer with one RSA-2048 signing key, created on first use and kept in
// memory (a restart forgets the keys, so tokens minted before it no longer
// verify). It is safe for concurrent use.
type FakeOIDCIssuers struct {
	clk clock.Clock

	mu   sync.Mutex
	keys map[string]fakeOIDCKey
}

type fakeOIDCKey struct {
	kid  string
	priv *rsa.PrivateKey
}

// NewFakeOIDCIssuers returns an empty set of fake issuers on clk.
func NewFakeOIDCIssuers(clk clock.Clock) (*FakeOIDCIssuers, error) {
	if clk == nil {
		return nil, errors.New("iam: fake OIDC issuer clock must not be nil")
	}
	return &FakeOIDCIssuers{clk: clk, keys: map[string]fakeOIDCKey{}}, nil
}

// FakeOIDCMintCmd is what a fake-issuer token asserts.
type FakeOIDCMintCmd struct {
	// Issuer is the iss claim: the issuer's URL as the client reached it.
	Issuer string
	// Subject (sub) is required; Audience (aud) is optional.
	Subject  string
	Audience string
	// TTL is the token lifetime; zero means DefaultFakeOIDCTokenTTL.
	TTL time.Duration
}

// JWKS returns the public JSON Web Key Set of issuer name.
func (f *FakeOIDCIssuers) JWKS(name string) ([]byte, error) {
	k, err := f.key(name)
	if err != nil {
		return nil, err
	}
	b, err := auth.JWKSet{Keys: []auth.JWK{{KeyID: k.kid, Alg: jwt.SigningMethodRS256.Alg(), Key: &k.priv.PublicKey}}}.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("fake OIDC issuer %s: %w", name, err)
	}
	return b, nil
}

// Mint signs an RS256 JWT of issuer name.
func (f *FakeOIDCIssuers) Mint(name string, cmd FakeOIDCMintCmd) (string, error) {
	if cmd.Subject == "" {
		return "", errs.BadRequest(errs.Violation{Field: "sub", Message: "required"})
	}
	ttl := cmd.TTL
	if ttl == 0 {
		ttl = DefaultFakeOIDCTokenTTL
	}
	if ttl < 0 {
		return "", errs.BadRequest(errs.Violation{Field: "ttl", Message: "must be positive"})
	}
	k, err := f.key(name)
	if err != nil {
		return "", err
	}
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", fmt.Errorf("fake OIDC token id: %w", err)
	}
	now := f.clk.Now()
	claims := jwt.MapClaims{
		"iss": cmd.Issuer, "sub": cmd.Subject, "iat": now.Unix(), "exp": now.Add(ttl).Unix(), "jti": hex.EncodeToString(jti[:]),
	}
	if cmd.Audience != "" {
		claims["aud"] = cmd.Audience
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = k.kid
	s, err := tok.SignedString(k.priv)
	if err != nil {
		return "", fmt.Errorf("fake OIDC issuer %s: sign: %w", name, err)
	}
	return s, nil
}

// key returns the signing key of issuer name, creating it on first use.
func (f *FakeOIDCIssuers) key(name string) (fakeOIDCKey, error) {
	if !fakeOIDCName.MatchString(name) {
		return fakeOIDCKey{}, errs.BadRequest(errs.Violation{
			Field: "name", Message: "issuer name must be 1-63 letters, digits, '.', '_' or '-', starting with a letter or digit",
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.keys[name]; ok {
		return k, nil
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fakeOIDCKey{}, fmt.Errorf("fake OIDC issuer %s: generate key: %w", name, err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return fakeOIDCKey{}, fmt.Errorf("fake OIDC issuer %s: encode key: %w", name, err)
	}
	sum := sha256.Sum256(der)
	k := fakeOIDCKey{kid: name + "-" + hex.EncodeToString(sum[:8]), priv: priv}
	f.keys[name] = k
	return k, nil
}
