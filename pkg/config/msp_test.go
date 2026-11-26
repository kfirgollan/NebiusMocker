package config

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestMSPDefaults(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(nil))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	m := c.MSP
	if m.PostgreSQLBackend != MSPBackendMock || m.MLflowBackend != MSPBackendMock {
		t.Errorf("backends = %q/%q, want mock/mock", m.PostgreSQLBackend, m.MLflowBackend)
	}
	if m.PostgresImages["16"] == "" || len(m.PostgresImages) != 1 {
		t.Errorf("PostgresImages = %v, want only 16", m.PostgresImages)
	}
	if got := m.PostgresPresets["cpu-e2"]; len(got) == 0 || got[0] != "2vcpu-8gb" {
		t.Errorf("cpu-e2 presets = %v", got)
	}
	if m.StartTimeout != 3*time.Minute || m.PostgresMaxHosts != 3 || m.MLflowSizeOrder[0] != "medium" ||
		m.PostgresDiskUnit != 1<<30 || m.PostgresDiskCheckInterval != time.Minute {
		t.Errorf("unexpected defaults: %+v", m)
	}
	for _, p := range m.PostgresPresets["cpu-e2"] {
		if _, _, ok := ParsePreset(p); !ok {
			t.Errorf("default preset %q does not parse", p)
		}
	}
}

func TestMSPParsed(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND":             "docker",
		"NEBIUS_MOCKER_MSP_MLFLOW_BACKEND":                 "docker",
		"NEBIUS_MOCKER_MSP_DOCKER_HOST":                    "tcp://docker.internal:2375",
		"NEBIUS_MOCKER_MSP_ENDPOINT_HOST":                  "host.docker.internal",
		"NEBIUS_MOCKER_MSP_POSTGRESQL_IMAGES":              "17=postgres:17-alpine",
		"NEBIUS_MOCKER_MSP_POSTGRESQL_PRESETS":             "cpu-d3=4vcpu-16gb|8vcpu-32gb",
		"NEBIUS_MOCKER_MSP_POSTGRESQL_MAX_HOSTS":           "5",
		"NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_UNIT":           "8Mi",
		"NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_CHECK_INTERVAL": "2s",
		"NEBIUS_MOCKER_MSP_MLFLOW_SIZES":                   "small=2vcpu-8gb,medium=8vcpu-32gb",
		"NEBIUS_MOCKER_MSP_S3_ENDPOINT":                    "http://s3.internal:4580",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	m := c.MSP
	if m.PostgreSQLBackend != MSPBackendDocker || m.MLflowBackend != MSPBackendDocker || m.DockerHost != "tcp://docker.internal:2375" {
		t.Errorf("backends/host = %q %q %q", m.PostgreSQLBackend, m.MLflowBackend, m.DockerHost)
	}
	if diff := cmp.Diff(map[string]string{"16": DefaultPostgresImages()["16"], "17": "postgres:17-alpine"}, m.PostgresImages); diff != "" {
		t.Errorf("images (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"4vcpu-16gb", "8vcpu-32gb"}, m.PostgresPresets["cpu-d3"]); diff != "" {
		t.Errorf("presets (-want +got):\n%s", diff)
	}
	if m.PostgresDiskUnit != 8<<20 || m.PostgresDiskCheckInterval != 2*time.Second {
		t.Errorf("disk unit/interval = %d %s", m.PostgresDiskUnit, m.PostgresDiskCheckInterval)
	}
	if m.PostgresMaxHosts != 5 || m.EndpointHost != "host.docker.internal" || m.S3Endpoint != "http://s3.internal:4580" {
		t.Errorf("parsed = %+v", m)
	}
	if diff := cmp.Diff([]string{"small", "medium"}, m.MLflowSizeOrder); diff != "" {
		t.Errorf("size order (-want +got):\n%s", diff)
	}
}

func TestMSPDockerHostFallback(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(map[string]string{"DOCKER_HOST": "unix:///tmp/docker.sock", "NEBIUS_MOCKER_MSP_MLFLOW_BACKEND": "docker"}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if c.MSP.DockerHost != "unix:///tmp/docker.sock" {
		t.Errorf("DockerHost = %q, want DOCKER_HOST", c.MSP.DockerHost)
	}
}

func TestMSPRejects(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]map[string]string{
		"backend":   {"NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND": "k3s"},
		"image":     {"NEBIUS_MOCKER_MSP_POSTGRESQL_IMAGES": "sixteen=postgres"},
		"preset":    {"NEBIUS_MOCKER_MSP_POSTGRESQL_PRESETS": "cpu-e2=big"},
		"size":      {"NEBIUS_MOCKER_MSP_MLFLOW_SIZES": "medium=8vcpu-32gb,medium=8vcpu-32gb"},
		"diskUnit":  {"NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_UNIT": "big"},
		"diskCheck": {"NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_CHECK_INTERVAL": "0s"},
		"maxHosts":  {"NEBIUS_MOCKER_MSP_POSTGRESQL_MAX_HOSTS": "0"},
		"s3":        {"NEBIUS_MOCKER_MSP_S3_ENDPOINT": "s3.internal"},
		"bindIP":    {"NEBIUS_MOCKER_MSP_BIND_IP": "localhost"},
		"dockerURL": {"NEBIUS_MOCKER_MSP_DOCKER_HOST": "docker.internal"},
	} {
		if _, err := FromLookup(lookup(env)); err == nil {
			t.Errorf("%s: FromLookup(%v) succeeded, want an error", name, env)
		}
	}
}

func TestParsePreset(t *testing.T) {
	t.Parallel()
	cpus, mem, ok := ParsePreset("16vcpu-64gb")
	if !ok || cpus != 16 || mem != 64 {
		t.Errorf("ParsePreset = %d %d %v", cpus, mem, ok)
	}
	if _, _, ok := ParsePreset("gpu-h100"); ok {
		t.Error("ParsePreset(gpu-h100) ok, want false")
	}
}
