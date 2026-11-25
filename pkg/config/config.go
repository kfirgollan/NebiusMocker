// Package config is the only place that reads the process environment.
//
// Every setting has an environment variable NEBIUS_MOCKER_<NAME>; see
// docs/architecture/11-configuration.md.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

// AuthnMode controls whether requests without credentials are accepted.
type AuthnMode int

// Authentication modes.
const (
	// AuthnOptional accepts anonymous requests but rejects invalid tokens.
	AuthnOptional AuthnMode = iota
	// AuthnRequired rejects requests without a valid token.
	AuthnRequired
)

// Built-in dev tokens and the principals they authenticate as. The IAM module
// seeds each principal into the matching default group (admins, editors,
// viewers). They are documented local-development tokens, not credentials.
const (
	DefaultDevToken        = "nmock-dev-admin" //nolint:gosec // documented local dev token
	DefaultDevPrincipal    = "tenantuseraccount-e00mock0000000000devadmin"
	DefaultEditorToken     = "nmock-dev-editor" //nolint:gosec // documented local dev token
	DefaultEditorPrincipal = "tenantuseraccount-e00mock000000000deveditor"
	DefaultViewerToken     = "nmock-dev-viewer" //nolint:gosec // documented local dev token
	DefaultViewerPrincipal = "tenantuseraccount-e00mock000000000devviewer"
	DefaultTenantID        = "tenant-e00mock0000000000default"
)

// Config is the full process configuration.
type Config struct {
	// Listen is the edge gRPC address, e.g. ":4566" or "127.0.0.1:0".
	Listen string
	// Services lists enabled areas; empty means all linked modules.
	Services []string
	Authn    AuthnMode
	// Authz selects off, audit (log would-be denials) or enforce.
	Authz authz.Mode
	// Tokens maps static bearer tokens to principal IDs.
	Tokens map[string]string
	// OperationDelay is how long operations stay running; 0 completes them inline.
	OperationDelay time.Duration
	IDMode         nid.Mode
	RoutingCode    string
	// TenantID is the seeded default tenant.
	TenantID string
	LogLevel slog.Level
}

// Default returns the configuration used when no environment is set.
func Default() Config {
	return Config{
		Listen: ":4566",
		Authn:  AuthnOptional,
		Authz:  authz.ModeAudit,
		Tokens: map[string]string{
			DefaultDevToken:    DefaultDevPrincipal,
			DefaultEditorToken: DefaultEditorPrincipal,
			DefaultViewerToken: DefaultViewerPrincipal,
		},
		OperationDelay: 0,
		IDMode:         nid.Random,
		RoutingCode:    "e00",
		TenantID:       DefaultTenantID,
		LogLevel:       slog.LevelInfo,
	}
}

// FromEnv returns Default overridden by NEBIUS_MOCKER_* environment variables.
func FromEnv() (Config, error) {
	return FromLookup(os.LookupEnv)
}

// FromLookup is FromEnv with an injectable lookup, for tests.
func FromLookup(lookup func(string) (string, bool)) (Config, error) {
	c := Default()
	parsers := []struct {
		name  string
		apply func(string) error
	}{
		{"LISTEN", func(v string) error { c.Listen = v; return nil }},
		{"SERVICES", func(v string) error { c.Services = splitList(v); return nil }},
		{"AUTHN", func(v string) error { return parseAuthn(v, &c.Authn) }},
		{"AUTHZ", func(v string) error { return parseAuthz(v, &c.Authz) }},
		{"DEV_TOKENS", func(v string) error {
			t, err := parseTokens(v)
			c.Tokens = t
			return err
		}},
		{"OP_DELAY", func(v string) error { return parseDuration(v, &c.OperationDelay) }},
		{"IDS", func(v string) error { return parseIDMode(v, &c.IDMode) }},
		{"ROUTING_CODE", func(v string) error { c.RoutingCode = v; return nil }},
		{"TENANT_ID", func(v string) error { c.TenantID = v; return nil }},
		{"LOG_LEVEL", func(v string) error { return c.LogLevel.UnmarshalText([]byte(v)) }},
	}
	for _, p := range parsers {
		v, ok := lookup("NEBIUS_MOCKER_" + p.name)
		if !ok || strings.TrimSpace(v) == "" {
			continue // empty values keep the default
		}
		if err := p.apply(strings.TrimSpace(v)); err != nil {
			return Config{}, fmt.Errorf("NEBIUS_MOCKER_%s: %w", p.name, err)
		}
	}
	return c, nil
}

func splitList(v string) []string {
	var out []string
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseAuthn(v string, out *AuthnMode) error {
	switch v {
	case "optional":
		*out = AuthnOptional
	case "required":
		*out = AuthnRequired
	default:
		return fmt.Errorf("want optional or required, got %q", v)
	}
	return nil
}

func parseAuthz(v string, out *authz.Mode) error {
	switch v {
	case "off":
		*out = authz.ModeOff
	case "audit":
		*out = authz.ModeAudit
	case "enforce":
		*out = authz.ModeEnforce
	default:
		return fmt.Errorf("want off, audit or enforce, got %q", v)
	}
	return nil
}

func parseTokens(v string) (map[string]string, error) {
	tokens := map[string]string{}
	for _, pair := range splitList(v) {
		tok, principal, ok := strings.Cut(pair, "=")
		if !ok || tok == "" || principal == "" {
			return nil, fmt.Errorf("want token=principal pairs, got %q", pair)
		}
		tokens[tok] = principal
	}
	return tokens, nil
}

func parseDuration(v string, out *time.Duration) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	if d < 0 {
		return fmt.Errorf("must not be negative, got %s", d)
	}
	*out = d
	return nil
}

func parseIDMode(v string, out *nid.Mode) error {
	switch v {
	case "random":
		*out = nid.Random
	case "sequential":
		*out = nid.Sequential
	default:
		return fmt.Errorf("want random or sequential, got %q", v)
	}
	return nil
}
