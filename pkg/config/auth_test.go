package config

import (
	"strings"
	"testing"
	"time"
)

func TestFromLookupAuth(t *testing.T) {
	t.Parallel()
	def := Default().Auth
	if def.TokenSecret != "" || def.TokenTTL != 12*time.Hour || def.JWTMaxTTL != time.Hour {
		t.Fatalf("default auth config = %+v", def)
	}
	secret := strings.Repeat("s", MinTokenSecretBytes)
	env := map[string]string{
		"NEBIUS_MOCKER_TOKEN_SECRET":    secret,
		"NEBIUS_MOCKER_TOKEN_TTL":       "30m",
		"NEBIUS_MOCKER_IAM_JWT_MAX_TTL": "5m",
	}
	c, err := FromLookup(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if c.Auth.TokenSecret != secret || c.Auth.TokenTTL != 30*time.Minute || c.Auth.JWTMaxTTL != 5*time.Minute {
		t.Fatalf("auth config = %+v", c.Auth)
	}
	for name, bad := range map[string]string{
		"NEBIUS_MOCKER_TOKEN_SECRET":    "too-short",
		"NEBIUS_MOCKER_TOKEN_TTL":       "0s",
		"NEBIUS_MOCKER_IAM_JWT_MAX_TTL": "-1m",
	} {
		_, err := FromLookup(func(k string) (string, bool) {
			if k == name {
				return bad, true
			}
			return "", false
		})
		if err == nil {
			t.Errorf("%s=%q accepted", name, bad)
		}
	}
}
