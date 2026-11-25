package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// ErrInvalidAssertion wraps every reason a JWT assertion is refused.
var ErrInvalidAssertion = errors.New("invalid JWT assertion")

// assertionLeeway tolerates clock skew between the client that signed the
// assertion and the mocker's clock (iat/nbf slightly in the future).
const assertionLeeway = 30 * time.Second

// Assertion is the unverified header and claims of a service-account JWT, as
// needed to find the key to verify it with.
type Assertion struct {
	// KeyID is the "kid" header: the auth public key ID.
	KeyID   string
	Issuer  string
	Subject string
	raw     string
}

// ParseAssertion reads an assertion's kid, iss and sub without verifying it.
// It fails if the token is not a JWS with an RS256 "alg" header: "none" and
// HMAC algorithms are refused before any key is looked up.
func ParseAssertion(token string) (Assertion, error) {
	var c jwt.RegisteredClaims
	t, _, err := jwt.NewParser(jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()})).ParseUnverified(token, &c)
	if err != nil {
		return Assertion{}, fmt.Errorf("%w: %w", ErrInvalidAssertion, err)
	}
	if alg, _ := t.Header["alg"].(string); alg != jwt.SigningMethodRS256.Alg() {
		return Assertion{}, fmt.Errorf("%w: alg %q is not supported, only RS256", ErrInvalidAssertion, alg)
	}
	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return Assertion{}, fmt.Errorf("%w: header kid (the auth public key ID) is required", ErrInvalidAssertion)
	}
	return Assertion{KeyID: kid, Issuer: c.Issuer, Subject: c.Subject, raw: token}, nil
}

// AssertionPolicy bounds the assertions VerifyAssertion accepts.
type AssertionPolicy struct {
	// Now is the mocker's current time.
	Now time.Time
	// MaxTTL is the longest accepted lifetime: exp may be at most MaxTTL
	// after now (and after iat, when present).
	MaxTTL time.Duration
}

// VerifyAssertion checks a's RS256 signature against key and its claims: exp
// present and in the future but within MaxTTL, iat and nbf (when present) not
// in the future, and iss = sub (a service account signs for itself).
func VerifyAssertion(a Assertion, key *rsa.PublicKey, p AssertionPolicy) error {
	var c jwt.RegisteredClaims
	parser := jwt.NewParser(jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}), jwt.WithoutClaimsValidation())
	if _, err := parser.ParseWithClaims(a.raw, &c, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodRS256 {
			return nil, fmt.Errorf("alg %q is not supported, only RS256", t.Method.Alg())
		}
		return key, nil
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAssertion, err)
	}
	return checkClaims(c, p)
}

func checkClaims(c jwt.RegisteredClaims, p AssertionPolicy) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidAssertion, fmt.Sprintf(format, args...))
	}
	if c.Issuer == "" || c.Subject == "" {
		return invalid("claims iss and sub (the service account ID) are required")
	}
	if c.Issuer != c.Subject {
		return invalid("iss %q and sub %q must be the same service account", c.Issuer, c.Subject)
	}
	if c.ExpiresAt == nil {
		return invalid("claim exp is required")
	}
	exp := c.ExpiresAt.Time
	if !exp.After(p.Now) {
		return invalid("the assertion expired at %s", exp.UTC().Format(time.RFC3339))
	}
	if exp.Sub(p.Now) > p.MaxTTL+assertionLeeway {
		return invalid("exp is %s ahead, at most %s is allowed", exp.Sub(p.Now).Round(time.Second), p.MaxTTL)
	}
	if c.IssuedAt != nil {
		iat := c.IssuedAt.Time
		if iat.After(p.Now.Add(assertionLeeway)) {
			return invalid("iat %s is in the future", iat.UTC().Format(time.RFC3339))
		}
		if exp.Sub(iat) > p.MaxTTL+assertionLeeway {
			return invalid("lifetime exp-iat is %s, at most %s is allowed", exp.Sub(iat).Round(time.Second), p.MaxTTL)
		}
	}
	if c.NotBefore != nil && c.NotBefore.After(p.Now.Add(assertionLeeway)) {
		return invalid("nbf %s is in the future", c.NotBefore.UTC().Format(time.RFC3339))
	}
	return nil
}
