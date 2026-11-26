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

func TestFromLookupFakeOIDC(t *testing.T) {
	t.Parallel()
	if !Default().Auth.FakeOIDC {
		t.Fatal("the fake OIDC issuer is off by default, want on")
	}
	lookup := func(v string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k == "NEBIUS_MOCKER_IAM_FAKE_OIDC" {
				return v, true
			}
			return "", false
		}
	}
	c, err := FromLookup(lookup("false"))
	if err != nil || c.Auth.FakeOIDC {
		t.Fatalf("NEBIUS_MOCKER_IAM_FAKE_OIDC=false: FakeOIDC = %v, err %v, want false", c.Auth.FakeOIDC, err)
	}
	if _, err := FromLookup(lookup("maybe")); err == nil {
		t.Error("NEBIUS_MOCKER_IAM_FAKE_OIDC=maybe accepted")
	}
}
