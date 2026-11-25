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
	DefaultProjectID       = "project-e00mock000000000default"
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
	// ProjectID is the seeded default project (child of TenantID, region eu-north1).
	ProjectID string
	// Regions are the emulated regions and their NID routing codes. The first
	// region is the default (the seeded project's region).
	Regions  []Region
	LogLevel slog.Level
	// ObjStore selects the object-storage backend shared by modules that keep blobs.
	ObjStore ObjStoreConfig
	// ComputeImagesFile, when set, replaces the built-in public image catalog
	// (YAML or JSON; see docs/services/compute.md).
	ComputeImagesFile string
	// Registry configures the Container Registry module.
	Registry RegistryConfig
}

// Region is one emulated Nebius region.
type Region struct {
	Name        string // e.g. "eu-north1"
	RoutingCode string // NID routing code, e.g. "e00"
}

// DefaultRegions are the regions emulated when NEBIUS_MOCKER_REGIONS is unset.
// The routing codes are placeholders, not confirmed upstream values (except
// e00 for eu-north1, which appears in public IDs).
func DefaultRegions() []Region {
	return []Region{
		{Name: "eu-north1", RoutingCode: "e00"},
		{Name: "eu-west1", RoutingCode: "e01"},
		{Name: "us-central1", RoutingCode: "u00"},
		{Name: "me-west1", RoutingCode: "m00"},
	}
}

// ObjStoreBackend selects the pkg/objstore implementation.
type ObjStoreBackend int

// Object-storage backends.
const (
	// ObjStoreMemory keeps objects in process memory (no data plane; tests).
	ObjStoreMemory ObjStoreBackend = iota
	// ObjStoreRustFS stores objects in a RustFS server.
	ObjStoreRustFS
)

// ObjStoreConfig configures the object-storage backend (NEBIUS_MOCKER_OBJSTORE*).
type ObjStoreConfig struct {
	Backend ObjStoreBackend
	// Endpoint, AccessKey, SecretKey and Region locate the RustFS server.
	Endpoint  string
	AccessKey string
	SecretKey string
	Region    string
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
		ProjectID:      DefaultProjectID,
		Regions:        DefaultRegions(),
		LogLevel:       slog.LevelInfo,
		ObjStore: ObjStoreConfig{
			Backend:   ObjStoreMemory,
			Endpoint:  "http://127.0.0.1:9000",
			AccessKey: "rustfsadmin", // RustFS's documented default credentials
			SecretKey: "rustfsadmin",
			Region:    "us-east-1",
		},
		Registry: defaultRegistry(),
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
		{"PROJECT_ID", func(v string) error { c.ProjectID = v; return nil }},
		{"LOG_LEVEL", func(v string) error { return c.LogLevel.UnmarshalText([]byte(v)) }},
		{"OBJSTORE", func(v string) error { return parseObjStore(v, &c.ObjStore.Backend) }},
		{"OBJSTORE_ENDPOINT", func(v string) error { c.ObjStore.Endpoint = v; return nil }},
		{"OBJSTORE_ACCESS_KEY", func(v string) error { c.ObjStore.AccessKey = v; return nil }},
		{"OBJSTORE_SECRET_KEY", func(v string) error { c.ObjStore.SecretKey = v; return nil }},
		{"OBJSTORE_REGION", func(v string) error { c.ObjStore.Region = v; return nil }},
		{"REGIONS", func(v string) error {
			r, err := parseRegions(v)
			c.Regions = r
			return err
		}},
		{"COMPUTE_IMAGES_FILE", func(v string) error { c.ComputeImagesFile = v; return nil }},
	}
	parsers = append(parsers, registryParsers(&c.Registry)...)
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

// parseRegions parses "name=code,…" (e.g. "eu-north1=e00,us-central1=u00").
func parseRegions(v string) ([]Region, error) {
	var out []Region
	seen := map[string]bool{}
	for _, pair := range splitList(v) {
		name, code, ok := strings.Cut(pair, "=")
		name, code = strings.TrimSpace(name), strings.TrimSpace(code)
		if !ok || name == "" || len(code) != 3 {
			return nil, fmt.Errorf("want region=routing-code pairs (3-character codes), got %q", pair)
		}
		if seen[name] || seen["="+code] {
			return nil, fmt.Errorf("duplicate region or routing code in %q", pair)
		}
		seen[name], seen["="+code] = true, true
		out = append(out, Region{Name: name, RoutingCode: code})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no regions in %q", v)
	}
	return out, nil
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

func parseObjStore(v string, out *ObjStoreBackend) error {
	switch v {
	case "memory":
		*out = ObjStoreMemory
	case "rustfs":
		*out = ObjStoreRustFS
	default:
		return fmt.Errorf("want memory or rustfs, got %q", v)
	}
	return nil
}
