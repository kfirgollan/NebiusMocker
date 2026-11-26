package config

import (
	"fmt"
	"time"
)

// AuthConfig configures the access tokens the mocker issues and the
// service-account JWT exchange (docs/architecture/06-iam-and-authorization.md).
type AuthConfig struct {
	// TokenSecret is the HMAC key issued tokens are signed with
	// (NEBIUS_MOCKER_TOKEN_SECRET, at least 32 bytes). Empty means a random
	// secret per process: tokens then do not survive a restart. Processes
	// that share it accept each other's tokens.
	TokenSecret string
	// TokenTTL is the lifetime of issued access tokens (expires_in);
	// NEBIUS_MOCKER_TOKEN_TTL, default 12h like Nebius IAM tokens.
	TokenTTL time.Duration
	// JWTMaxTTL is the longest lifetime (exp - now) a service-account JWT
	// assertion may have; NEBIUS_MOCKER_IAM_JWT_MAX_TTL, default 1h.
	JWTMaxTTL time.Duration
	// FakeOIDC serves the built-in fake OIDC issuer under /_mocker/oidc/ on
	// the edge port, for federated credentials; NEBIUS_MOCKER_IAM_FAKE_OIDC,
	// default true.
	FakeOIDC bool
}

// MinTokenSecretBytes is the shortest accepted NEBIUS_MOCKER_TOKEN_SECRET.
const MinTokenSecretBytes = 32

func defaultAuth() AuthConfig {
	return AuthConfig{TokenTTL: 12 * time.Hour, JWTMaxTTL: time.Hour, FakeOIDC: true}
}

func authParsers(c *AuthConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"TOKEN_SECRET", func(v string) error {
			if len(v) < MinTokenSecretBytes {
				return fmt.Errorf("must be at least %d bytes", MinTokenSecretBytes)
			}
			c.TokenSecret = v
			return nil
		}},
		{"TOKEN_TTL", func(v string) error { return parsePositive(v, &c.TokenTTL) }},
		{"IAM_JWT_MAX_TTL", func(v string) error { return parsePositive(v, &c.JWTMaxTTL) }},
		{"IAM_FAKE_OIDC", func(v string) error { return parseBool(v, &c.FakeOIDC) }},
	}
}
