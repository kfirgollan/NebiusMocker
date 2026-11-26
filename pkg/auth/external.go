package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// Federated (external) JWT failures. ParseExternalJWT and VerifyExternalJWT
// wrap ErrInvalidExternalJWT; VerifyExternalJWT also wraps ErrUnknownKeyID
// when the key set has no key with the token's kid, so a caller holding a
// cached set can refetch it.
var (
	ErrInvalidExternalJWT = errors.New("invalid federated JWT")
	ErrUnknownKeyID       = errors.New("no key with this kid in the issuer's key set")
)

// externalAlgs are the signature algorithms federated JWTs may use. Others
// ("none", HMAC, RSA-PSS, other curves) are refused by name.
var externalAlgs = []string{jwt.SigningMethodRS256.Alg(), jwt.SigningMethodES256.Alg()}

// ExternalJWT is the unverified header and claims of a JWT issued by an
// external OIDC provider (a federated credential), as needed to find the
// federated credentials and keys to verify it with.
type ExternalJWT struct {
	// KeyID is the optional "kid" header.
	KeyID   string
	Alg     string
	Issuer  string
	Subject string
	raw     string
}

// ParseExternalJWT reads a federated JWT's alg, kid, iss and sub without
// verifying it. alg must be RS256 or ES256; iss and sub are required.
func ParseExternalJWT(token string) (ExternalJWT, error) {
	var c jwt.RegisteredClaims
	t, _, err := jwt.NewParser().ParseUnverified(token, &c)
	if err != nil {
		return ExternalJWT{}, fmt.Errorf("%w: %w", ErrInvalidExternalJWT, err)
	}
	alg, _ := t.Header["alg"].(string)
	if !slices.Contains(externalAlgs, alg) {
		return ExternalJWT{}, fmt.Errorf("%w: alg %q is not supported, only RS256 and ES256", ErrInvalidExternalJWT, alg)
	}
	if c.Issuer == "" || c.Subject == "" {
		return ExternalJWT{}, fmt.Errorf("%w: claims iss and sub are required", ErrInvalidExternalJWT)
	}
	kid, _ := t.Header["kid"].(string)
	return ExternalJWT{KeyID: kid, Alg: alg, Issuer: c.Issuer, Subject: c.Subject, raw: token}, nil
}

// VerifyExternalJWT checks j's signature against the key of set named by its
// kid (or the set's only key, when j has no kid) and its claims at now: exp
// present and in the future, iat and nbf (when present) not in the future,
// with a 30s clock-skew allowance. It does not check aud: the caller decides
// which audiences it trusts.
func VerifyExternalJWT(j ExternalJWT, set JWKSet, now time.Time) error {
	key, err := set.keyFor(j)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidExternalJWT, err)
	}
	var c jwt.RegisteredClaims
	parser := jwt.NewParser(jwt.WithValidMethods([]string{j.Alg}), jwt.WithoutClaimsValidation())
	if _, err := parser.ParseWithClaims(j.raw, &c, func(*jwt.Token) (any, error) { return key, nil }); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidExternalJWT, err)
	}
	if err := checkExternalClaims(c, now); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidExternalJWT, err)
	}
	return nil
}

// keyFor picks the verification key of j.
func (s JWKSet) keyFor(j ExternalJWT) (any, error) {
	if j.KeyID == "" {
		if len(s.Keys) != 1 {
			return nil, fmt.Errorf("header kid is required: the issuer's key set has %d keys", len(s.Keys))
		}
		return compatibleKey(s.Keys[0], j.Alg)
	}
	var lastErr error
	for _, k := range s.Keys {
		if k.KeyID != j.KeyID {
			continue
		}
		key, err := compatibleKey(k, j.Alg)
		if err == nil {
			return key, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: kid %q", ErrUnknownKeyID, j.KeyID)
}

// compatibleKey returns k's key if it can verify alg.
func compatibleKey(k JWK, alg string) (any, error) {
	if k.Alg != "" && k.Alg != alg {
		return nil, fmt.Errorf("key %q is for alg %s, the token uses %s", k.KeyID, k.Alg, alg)
	}
	switch pub := k.Key.(type) {
	case *rsa.PublicKey:
		if alg == jwt.SigningMethodRS256.Alg() {
			return pub, nil
		}
	case *ecdsa.PublicKey:
		if alg == jwt.SigningMethodES256.Alg() && pub.Curve == elliptic.P256() {
			return pub, nil
		}
	}
	return nil, fmt.Errorf("key %q cannot verify alg %s", k.KeyID, alg)
}

func checkExternalClaims(c jwt.RegisteredClaims, now time.Time) error {
	if c.ExpiresAt == nil {
		return errors.New("claim exp is required")
	}
	if exp := c.ExpiresAt.Time; !exp.After(now) {
		return fmt.Errorf("the token expired at %s", exp.UTC().Format(time.RFC3339))
	}
	if c.IssuedAt != nil && c.IssuedAt.After(now.Add(assertionLeeway)) {
		return fmt.Errorf("iat %s is in the future", c.IssuedAt.UTC().Format(time.RFC3339))
	}
	if c.NotBefore != nil && c.NotBefore.After(now.Add(assertionLeeway)) {
		return fmt.Errorf("nbf %s is in the future", c.NotBefore.UTC().Format(time.RFC3339))
	}
	return nil
}
