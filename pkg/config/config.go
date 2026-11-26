// Package config is the only place that reads the process environment.
//
// Every setting has an environment variable NEBIUS_MOCKER_<NAME>; see
// docs/architecture/11-configuration.md.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
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
	// ComputeFabricsFile, when set, replaces the built-in InfiniBand fabric
	// catalog of GPU clusters (YAML or JSON; see docs/services/compute.md).
	ComputeFabricsFile string
	// ComputePlatformsFile, when set, replaces the built-in platform catalog
	// (YAML or JSON; see docs/services/compute.md).
	ComputePlatformsFile string
	// ComputePublicIPReleaseAfter is how long a stopped instance keeps a
	// dynamic public IP address (public_ip_address without allocation_id,
	// static=false) before it is released (docs: more than one hour).
	ComputePublicIPReleaseAfter time.Duration
	// Registry configures the Container Registry module.
	Registry RegistryConfig
	// TLS enables TLS on the mocker's listeners; empty means plaintext.
	TLS TLSConfig
	// Storage configures the Object Storage module (NEBIUS_MOCKER_STORAGE_*).
	Storage StorageConfig
	// MysteryBox holds the secrets manager's limits and payload key.
	MysteryBox MysteryBoxConfig
	// DNS configures the DNS module and its responder (NEBIUS_MOCKER_DNS_*).
	DNS DNSConfig
	// MK8s configures Managed Kubernetes (NEBIUS_MOCKER_MK8S_*).
	MK8s MK8sConfig
	// VPC is the seeded VPC address space (NEBIUS_MOCKER_VPC_*).
	VPC VPCConfig
	// AI configures the AI endpoints area and its stub data plane.
	AI AIConfig
	// Audit configures the audit event log (NEBIUS_MOCKER_AUDIT_*).
	Audit AuditConfig
	// IAM configures the IAM module's seed (NEBIUS_MOCKER_IAM_*).
	IAM IAMConfig
	// Auth configures issued access tokens and the service-account token
	// exchange (NEBIUS_MOCKER_TOKEN_*, NEBIUS_MOCKER_IAM_JWT_MAX_TTL).
	Auth AuthConfig
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

// StorageConfig configures Object Storage.
type StorageConfig struct {
	// S3Listen is the S3 data-plane address; "off" disables the S3 front.
	S3Listen string
	// S3Domain is the S3 endpoint host: status.domain_name is "<bucket>.<S3Domain>",
	// and requests to that host are virtual-hosted style.
	S3Domain string
	// Region is the bucket region reported to S3 clients.
	Region string
	// PurgeGrace is how long a deleted bucket can be undeleted (default ttl).
	PurgeGrace time.Duration
}

// MysteryBoxConfig configures the mysterybox module (NEBIUS_MOCKER_MYSTERYBOX_*).
// Zero limits take the module's defaults (see docs/services/mysterybox.md).
type MysteryBoxConfig struct {
	// MaxEntries is the number of payload entries one version may hold.
	MaxEntries int
	// MaxValueBytes is the size of one payload value.
	MaxValueBytes int
	// MaxVersions is the number of versions one secret may hold.
	MaxVersions int
	// PurgeDelay is how long a deleted secret or version can be restored.
	PurgeDelay time.Duration
	// Key is the base64 AES-256 key payloads are sealed with; empty means a
	// random key per process.
	Key string
}

// AuditConfig configures audit event capture.
type AuditConfig struct {
	// MaxEvents bounds the in-memory event log; the oldest events are evicted.
	MaxEvents int
	// Reads also records Get/List calls (default: only mutating calls and
	// authorization denials, like Nebius control-plane audit logs).
	Reads bool
	// ExportDuration is how long an audit event export stays RUNNING
	// before it writes its objects; 0 exports inside the Start call.
	ExportDuration time.Duration
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
		// docs.nebius.com/compute/virtual-machines/network: a dynamic address
		// returns to the range once the VM is STOPPED for more than one hour.
		ComputePublicIPReleaseAfter: time.Hour,
		LogLevel:                    slog.LevelInfo,
		VPC:                         defaultVPC(),
		ObjStore: ObjStoreConfig{
			Backend:   ObjStoreMemory,
			Endpoint:  "http://127.0.0.1:9000",
			AccessKey: "rustfsadmin", // RustFS's documented default credentials
			SecretKey: "rustfsadmin",
			Region:    "us-east-1",
		},
		Registry: defaultRegistry(),
		Audit:    AuditConfig{MaxEvents: 50_000},
		Storage: StorageConfig{
			S3Listen:   ":4580",
			S3Domain:   "storage.eu-north1.mocker.localhost",
			Region:     "eu-north1",
			PurgeGrace: 7 * 24 * time.Hour,
		},
		AI:   defaultAI(),
		DNS:  defaultDNS(),
		MK8s: defaultMK8s(),
		Auth: defaultAuth(),
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
		{"COMPUTE_PLATFORMS_FILE", func(v string) error { c.ComputePlatformsFile = v; return nil }},
		{"COMPUTE_PUBLIC_IP_RELEASE_AFTER", func(v string) error { return parsePositive(v, &c.ComputePublicIPReleaseAfter) }},
		{"STORAGE_S3_LISTEN", func(v string) error { c.Storage.S3Listen = v; return nil }},
		{"STORAGE_S3_DOMAIN", func(v string) error { c.Storage.S3Domain = v; return nil }},
		{"STORAGE_REGION", func(v string) error { c.Storage.Region = v; return nil }},
		{"STORAGE_PURGE_GRACE", func(v string) error { return parsePositive(v, &c.Storage.PurgeGrace) }},
		{"AUDIT_MAX_EVENTS", func(v string) error { return parsePositiveInt(v, &c.Audit.MaxEvents) }},
		{"AUDIT_READS", func(v string) error { return parseBool(v, &c.Audit.Reads) }},
		{"AUDIT_EXPORT_DURATION", func(v string) error { return parseDuration(v, &c.Audit.ExportDuration) }},
		{"MYSTERYBOX_MAX_ENTRIES", func(v string) error { return parsePositiveInt(v, &c.MysteryBox.MaxEntries) }},
		{"MYSTERYBOX_MAX_VALUE_BYTES", func(v string) error { return parsePositiveInt(v, &c.MysteryBox.MaxValueBytes) }},
		{"MYSTERYBOX_MAX_VERSIONS", func(v string) error { return parsePositiveInt(v, &c.MysteryBox.MaxVersions) }},
		{"MYSTERYBOX_PURGE_DELAY", func(v string) error { return parseDuration(v, &c.MysteryBox.PurgeDelay) }},
		{"MYSTERYBOX_KEY", func(v string) error { c.MysteryBox.Key = v; return nil }},
		{"AI_LISTEN", func(v string) error { c.AI.Listen = v; return nil }},
		{"AI_PUBLIC_URL", func(v string) error { c.AI.PublicURL = v; return nil }},
		{"AI_TCP_LISTEN", func(v string) error { c.AI.TCPListen = v; return nil }},
		{"AI_TCP_DOMAIN", func(v string) error { c.AI.TCPDomain = v; return nil }},
		{"AI_TLS_DIR", func(v string) error { c.AI.TLSDir = v; return nil }},
		{"AI_PLATFORMS", func(v string) error { return parseAIPlatforms(v, &c.AI.Platforms) }},
	}
	parsers = append(parsers, registryParsers(&c.Registry)...)
	parsers = append(parsers, vpcParsers(&c.VPC)...)
	parsers = append(parsers, dnsParsers(&c.DNS)...)
	parsers = append(parsers, mk8sParsers(&c.MK8s)...)
	parsers = append(parsers, tlsParsers(&c.TLS)...)
	parsers = append(parsers, iamParsers(&c.IAM)...)
	parsers = append(parsers, authParsers(&c.Auth)...)
	parsers = append(parsers, computeFabricsParsers(&c.ComputeFabricsFile)...)
	for _, p := range parsers {
		v, ok := lookup("NEBIUS_MOCKER_" + p.name)
		if !ok || strings.TrimSpace(v) == "" {
			continue // empty values keep the default
		}
		if err := p.apply(strings.TrimSpace(v)); err != nil {
			return Config{}, fmt.Errorf("NEBIUS_MOCKER_%s: %w", p.name, err)
		}
	}
	if _, err := c.SeededProjects(); err != nil {
		return Config{}, fmt.Errorf("NEBIUS_MOCKER_IAM_PROJECTS: %w", err)
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

func parsePositiveInt(v string, out *int) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("must be positive, got %d", n)
	}
	*out = n
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

func parsePositive(v string, out *time.Duration) error {
	if err := parseDuration(v, out); err != nil {
		return err
	}
	if *out == 0 {
		return fmt.Errorf("must be positive, got %s", v)
	}
	return nil
}

func parseBool(v string, out *bool) error {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("want true or false, got %q", v)
	}
	*out = b
	return nil
}
