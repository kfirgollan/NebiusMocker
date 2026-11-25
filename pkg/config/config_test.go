package config

import (
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

func lookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestFromLookup(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_LISTEN":     "127.0.0.1:9000",
		"NEBIUS_MOCKER_SERVICES":   "iam, vpc",
		"NEBIUS_MOCKER_AUTHN":      "required",
		"NEBIUS_MOCKER_AUTHZ":      "enforce",
		"NEBIUS_MOCKER_DEV_TOKENS": "a=tenantuseraccount-e00x,b=serviceaccount-e00y",
		"NEBIUS_MOCKER_OP_DELAY":   "250ms",
		"NEBIUS_MOCKER_IDS":        "sequential",
		"NEBIUS_MOCKER_TENANT_ID":  "", // empty keeps the default
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if c.Listen != "127.0.0.1:9000" || len(c.Services) != 2 || c.Authn != AuthnRequired || c.Authz != authz.ModeEnforce ||
		c.Tokens["b"] != "serviceaccount-e00y" || c.OperationDelay != 250*time.Millisecond ||
		c.IDMode != nid.Sequential || c.TenantID != DefaultTenantID {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestFromLookupErrors(t *testing.T) {
	t.Parallel()
	for _, env := range []map[string]string{
		{"NEBIUS_MOCKER_AUTHN": "sometimes"},
		{"NEBIUS_MOCKER_DEV_TOKENS": "no-equals-sign"},
		{"NEBIUS_MOCKER_OP_DELAY": "-1s"},
		{"NEBIUS_MOCKER_IDS": "uuid"},
		{"NEBIUS_MOCKER_AUTHZ": "maybe"},
	} {
		if _, err := FromLookup(lookup(env)); err == nil {
			t.Errorf("FromLookup(%v) succeeded, want error", env)
		}
	}
}
