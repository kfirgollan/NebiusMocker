package config

import "fmt"

// RegistryBackend selects where Container Registry images live.
type RegistryBackend int

// Container Registry backends.
const (
	// RegistryMemory keeps image metadata in process memory and serves no
	// docker data plane (unit and contract tests).
	RegistryMemory RegistryBackend = iota
	// RegistryHarbor stores images in a Harbor server; the mocker fronts its
	// docker endpoint with Nebius IAM tokens.
	RegistryHarbor
)

// RegistryConfig configures the Container Registry module (NEBIUS_MOCKER_REGISTRY_*).
type RegistryConfig struct {
	// Backend is memory (default) or harbor.
	Backend RegistryBackend
	// Listen is the docker-facing registry endpoint (harbor backend only);
	// "off" disables it.
	Listen string
	// FQDN is reported as status.registry_fqdn and is the host part of image
	// names. Empty means "127.0.0.1:<listen port>", which docker treats as an
	// insecure-allowed (plain HTTP) registry.
	FQDN string
	// HarborURL, HarborUsername and HarborPassword reach Harbor with admin rights.
	HarborURL      string
	HarborUsername string
	HarborPassword string
}

// DefaultRegistryListen is the docker-facing registry port.
const DefaultRegistryListen = ":4590"

func defaultRegistry() RegistryConfig {
	return RegistryConfig{
		Backend:        RegistryMemory,
		Listen:         DefaultRegistryListen,
		HarborURL:      "http://127.0.0.1:8080",
		HarborUsername: "admin",
		HarborPassword: "Harbor12345", // Harbor's documented default admin password
	}
}

// registryParsers returns the NEBIUS_MOCKER_REGISTRY_* settings.
func registryParsers(r *RegistryConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"REGISTRY_BACKEND", func(v string) error { return parseRegistryBackend(v, &r.Backend) }},
		{"REGISTRY_LISTEN", func(v string) error { r.Listen = v; return nil }},
		{"REGISTRY_FQDN", func(v string) error { r.FQDN = v; return nil }},
		{"REGISTRY_HARBOR_URL", func(v string) error { r.HarborURL = v; return nil }},
		{"REGISTRY_HARBOR_USERNAME", func(v string) error { r.HarborUsername = v; return nil }},
		{"REGISTRY_HARBOR_PASSWORD", func(v string) error { r.HarborPassword = v; return nil }},
	}
}

func parseRegistryBackend(v string, out *RegistryBackend) error {
	switch v {
	case "memory":
		*out = RegistryMemory
	case "harbor":
		*out = RegistryHarbor
	default:
		return fmt.Errorf("want memory or harbor, got %q", v)
	}
	return nil
}
