package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Managed service backends (NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND,
// NEBIUS_MOCKER_MSP_MLFLOW_BACKEND).
const (
	// MSPBackendMock runs no server: clusters get endpoints nothing listens
	// on, backups are records without data.
	MSPBackendMock = "mock"
	// MSPBackendDocker runs each cluster as real containers (PostgreSQL and
	// PgBouncer; an MLflow tracking server) through the Docker Engine API.
	MSPBackendDocker = "docker"
)

// MSPConfig configures the managed services, Managed PostgreSQL and Managed
// MLflow (NEBIUS_MOCKER_MSP_*). The catalogs (versions, presets, sizes) are
// what the control plane accepts with every backend; the image settings are
// used by the docker backend only.
type MSPConfig struct {
	// PostgreSQLBackend is "mock" (default) or "docker"
	// (NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND).
	PostgreSQLBackend string
	// MLflowBackend is "mock" (default) or "docker"
	// (NEBIUS_MOCKER_MSP_MLFLOW_BACKEND).
	MLflowBackend string
	// DockerHost is the Docker Engine endpoint, "unix:///path" or
	// "tcp://host:port" (NEBIUS_MOCKER_MSP_DOCKER_HOST, else DOCKER_HOST, else
	// the local socket).
	DockerHost string
	// BindIP is the host address the clusters' ports are published on
	// (NEBIUS_MOCKER_MSP_BIND_IP).
	BindIP string
	// EndpointHost is the host in the clusters' endpoints and in their TLS
	// certificates (NEBIUS_MOCKER_MSP_ENDPOINT_HOST). Clients that run in
	// containers reach the published ports as host.docker.internal.
	EndpointHost string
	// TLSDir holds the local CA that signs the clusters' server
	// certificates; clients trust <TLSDir>/ca.pem
	// (NEBIUS_MOCKER_MSP_TLS_DIR, default: the mocker's CA directory, shared
	// with the AI data plane).
	TLSDir string
	// StartTimeout bounds how long a cluster's containers may take to
	// become ready (NEBIUS_MOCKER_MSP_START_TIMEOUT).
	StartTimeout time.Duration

	// PostgresImages maps a PostgreSQL major version (spec.config.version)
	// to the image that runs it; the keys are the version catalog
	// (NEBIUS_MOCKER_MSP_POSTGRESQL_IMAGES, "16=postgres:16.11-alpine,…";
	// listed entries replace the defaults of the same version).
	PostgresImages map[string]string
	// PgBouncerImage runs the connection pooler in front of the primary
	// (NEBIUS_MOCKER_MSP_PGBOUNCER_IMAGE).
	PgBouncerImage string
	// PostgresPresets is the resources catalog: platform -> presets
	// "<n>vcpu-<m>gb" (NEBIUS_MOCKER_MSP_POSTGRESQL_PRESETS,
	// "cpu-e2=2vcpu-8gb|4vcpu-16gb;…"; listed platforms replace the defaults).
	PostgresPresets map[string][]string
	// PostgresDiskTypes are the accepted spec.config.template.disk.type values
	// (NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_TYPES, comma-separated).
	PostgresDiskTypes []string
	// PostgresMaxHosts caps spec.config.template.hosts.count
	// (NEBIUS_MOCKER_MSP_POSTGRESQL_MAX_HOSTS).
	PostgresMaxHosts int

	// MLflowImage is the official MLflow image the tracking server is built
	// from; the docker backend adds the S3 and auth extras on top of it
	// once, as MLflowImage + "-nebius-mocker" (NEBIUS_MOCKER_MSP_MLFLOW_IMAGE).
	MLflowImage string
	// MLflowVersion is status.mlflow_version; it must match MLflowImage
	// (NEBIUS_MOCKER_MSP_MLFLOW_VERSION).
	MLflowVersion string
	// MLflowSizes is the spec.size catalog: size -> "<n>vcpu-<m>gb"; the
	// first entry of MLflowSizeOrder is the default
	// (NEBIUS_MOCKER_MSP_MLFLOW_SIZES, "medium=8vcpu-32gb,…").
	MLflowSizes map[string]string
	// MLflowSizeOrder lists MLflowSizes smallest first.
	MLflowSizeOrder []string
	// S3Endpoint is where the MLflow containers reach the Object Storage S3
	// front (NEBIUS_MOCKER_MSP_S3_ENDPOINT). Empty: http on
	// host.docker.internal and the port of NEBIUS_MOCKER_STORAGE_S3_LISTEN.
	S3Endpoint string
}

// DefaultPostgresImages are the PostgreSQL images of the version catalog
// (multi-arch: amd64 and arm64). docs.nebius.com/postgresql: "currently
// supports only version 16".
func DefaultPostgresImages() map[string]string {
	return map[string]string{"16": "postgres:16.11-alpine"}
}

// DefaultPostgresPresets follows docs.nebius.com/postgresql: platform cpu-e2
// with the compute non-GPU presets.
func DefaultPostgresPresets() map[string][]string {
	return map[string][]string{
		"cpu-e2": {"2vcpu-8gb", "4vcpu-16gb", "8vcpu-32gb", "16vcpu-64gb", "32vcpu-128gb", "48vcpu-192gb", "64vcpu-256gb", "80vcpu-320gb"},
	}
}

func defaultMSP() MSPConfig {
	return MSPConfig{
		PostgreSQLBackend: MSPBackendMock,
		MLflowBackend:     MSPBackendMock,
		BindIP:            "127.0.0.1",
		EndpointHost:      "127.0.0.1",
		TLSDir:            defaultCADir(),
		StartTimeout:      3 * time.Minute,
		PostgresImages:    DefaultPostgresImages(),
		PgBouncerImage:    "edoburu/pgbouncer:v1.25.2-p0",
		PostgresPresets:   DefaultPostgresPresets(),
		PostgresDiskTypes: []string{"network-ssd"},
		PostgresMaxHosts:  3,
		MLflowImage:       "ghcr.io/mlflow/mlflow:v2.22.0",
		MLflowVersion:     "2.22.0",
		// docs.nebius.com/mlflow: "medium" (8 vCPUs, 32 GiB RAM) is the only size.
		MLflowSizes:     map[string]string{"medium": "8vcpu-32gb"},
		MLflowSizeOrder: []string{"medium"},
	}
}

func mspParsers(m *MSPConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"MSP_POSTGRESQL_BACKEND", func(s string) error { return parseMSPBackend(s, &m.PostgreSQLBackend) }},
		{"MSP_MLFLOW_BACKEND", func(s string) error { return parseMSPBackend(s, &m.MLflowBackend) }},
		{"MSP_DOCKER_HOST", func(s string) error { return parseDockerHost(s, &m.DockerHost) }},
		{"MSP_BIND_IP", func(s string) error { return parseIP(s, &m.BindIP) }},
		{"MSP_ENDPOINT_HOST", func(s string) error { m.EndpointHost = s; return nil }},
		{"MSP_TLS_DIR", func(s string) error { m.TLSDir = s; return nil }},
		{"MSP_START_TIMEOUT", func(s string) error { return parsePositive(s, &m.StartTimeout) }},
		{"MSP_POSTGRESQL_IMAGES", func(s string) error { return parsePostgresImages(s, m.PostgresImages) }},
		{"MSP_PGBOUNCER_IMAGE", func(s string) error { m.PgBouncerImage = s; return nil }},
		{"MSP_POSTGRESQL_PRESETS", func(s string) error { return parsePostgresPresets(s, m.PostgresPresets) }},
		{"MSP_POSTGRESQL_DISK_TYPES", func(s string) error { m.PostgresDiskTypes = splitList(s); return nil }},
		{"MSP_POSTGRESQL_MAX_HOSTS", func(s string) error { return parsePositiveInt(s, &m.PostgresMaxHosts) }},
		{"MSP_MLFLOW_IMAGE", func(s string) error { m.MLflowImage = s; return nil }},
		{"MSP_MLFLOW_VERSION", func(s string) error { m.MLflowVersion = s; return nil }},
		{"MSP_MLFLOW_SIZES", func(s string) error { return parseMLflowSizes(s, m) }},
		{"MSP_S3_ENDPOINT", func(s string) error { return parseWebhookURL(s, &m.S3Endpoint) }},
	}
}

// applyDockerHosts applies Docker's own DOCKER_HOST to every backend whose
// NEBIUS_MOCKER_*_DOCKER_HOST is not set.
func applyDockerHosts(lookup func(string) (string, bool), c *Config) error {
	if err := applyDockerHost(lookup, &c.MK8sK3s); err != nil {
		return err
	}
	return applyMSPDockerHost(lookup, &c.MSP)
}

// applyMSPDockerHost falls back to Docker's own DOCKER_HOST when
// NEBIUS_MOCKER_MSP_DOCKER_HOST is not set and a docker backend is selected
// (the mock backends never talk to Docker).
func applyMSPDockerHost(lookup func(string) (string, bool), m *MSPConfig) error {
	if m.DockerHost != "" || (m.PostgreSQLBackend != MSPBackendDocker && m.MLflowBackend != MSPBackendDocker) {
		return nil
	}
	v, ok := lookup("DOCKER_HOST")
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	if err := parseDockerHost(strings.TrimSpace(v), &m.DockerHost); err != nil {
		return fmt.Errorf("DOCKER_HOST: %w", err)
	}
	return nil
}

func parseMSPBackend(s string, out *string) error {
	if s != MSPBackendMock && s != MSPBackendDocker {
		return fmt.Errorf("want %q or %q, got %q", MSPBackendMock, MSPBackendDocker, s)
	}
	*out = s
	return nil
}

var (
	pgMajorRE  = regexp.MustCompile(`^[1-9]\d*$`)
	presetRE   = regexp.MustCompile(`^([1-9]\d*)vcpu-([1-9]\d*)gb$`)
	mspNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	errPresets = "want <platform>=<n>vcpu-<m>gb|…[;…]"
)

func parsePostgresImages(s string, images map[string]string) error {
	for entry := range strings.SplitSeq(s, ",") {
		version, image, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || !pgMajorRE.MatchString(version) || image == "" {
			return fmt.Errorf("want <major>=<image>[,…], got %q", entry)
		}
		images[version] = image
	}
	return nil
}

func parsePostgresPresets(s string, presets map[string][]string) error {
	for entry := range strings.SplitSeq(s, ";") {
		platform, list, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || !mspNameRE.MatchString(platform) || list == "" {
			return fmt.Errorf("%s, got %q", errPresets, entry)
		}
		names := strings.Split(list, "|")
		for _, n := range names {
			if !presetRE.MatchString(n) {
				return fmt.Errorf("%s, got preset %q", errPresets, n)
			}
		}
		presets[platform] = names
	}
	return nil
}

func parseMLflowSizes(s string, m *MSPConfig) error {
	sizes := map[string]string{}
	var order []string
	for entry := range strings.SplitSeq(s, ",") {
		name, res, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || !mspNameRE.MatchString(name) || !presetRE.MatchString(res) || slices.Contains(order, name) {
			return fmt.Errorf("want <size>=<n>vcpu-<m>gb[,…] smallest first, got %q", entry)
		}
		sizes[name] = res
		order = append(order, name)
	}
	m.MLflowSizes, m.MLflowSizeOrder = sizes, order
	return nil
}

// ParsePreset splits a "<n>vcpu-<m>gb" preset into vCPUs and GiB of memory.
func ParsePreset(preset string) (cpus, memoryGiB int64, ok bool) {
	m := presetRE.FindStringSubmatch(preset)
	if m == nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscan(m[1], &cpus); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscan(m[2], &memoryGiB); err != nil {
		return 0, 0, false
	}
	return cpus, memoryGiB, true
}
