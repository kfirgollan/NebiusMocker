package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MK8sK3sConfig configures the k3s backend of Managed Kubernetes
// (NEBIUS_MOCKER_MK8S_BACKEND=k3s, NEBIUS_MOCKER_MK8S_K3S_*): one rancher/k3s
// server container per cluster and one k3s agent container per node, driven
// through the Docker Engine API.
type MK8sK3sConfig struct {
	// DockerHost is the Docker Engine endpoint, "unix:///path" or
	// "tcp://host:port" (NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST, else DOCKER_HOST,
	// else the local socket).
	DockerHost string
	// Images maps a control-plane version "<major>.<minor>" to the k3s image
	// that runs it (NEBIUS_MOCKER_MK8S_K3S_IMAGES, "1.35=rancher/k3s:v1.35.10-k3s1,…";
	// listed entries replace the defaults of the same version).
	Images map[string]string
	// BindIP is the host address the API server port is published on
	// (NEBIUS_MOCKER_MK8S_K3S_BIND_IP).
	BindIP string
	// EndpointHost is the host name in the cluster's endpoints and in the API
	// server certificate (NEBIUS_MOCKER_MK8S_K3S_ENDPOINT_HOST). Clients that
	// run in containers reach the published port as host.docker.internal.
	EndpointHost string
	// WebhookURL is where the k3s API servers reach the mocker's TokenReview
	// webhook, scheme://host:port of the edge
	// (NEBIUS_MOCKER_MK8S_K3S_WEBHOOK_URL). Empty: host.docker.internal on the
	// edge port, https when the edge serves TLS.
	WebhookURL string
	// ServerMemory and AgentMemory are the memory estimates (bytes) admission
	// reserves for a cluster's server and for each node agent
	// (NEBIUS_MOCKER_MK8S_K3S_SERVER_MEMORY / _AGENT_MEMORY, e.g. "768Mi").
	ServerMemory int64
	AgentMemory  int64
	// AgentCPU is the CPU estimate per agent, in cores
	// (NEBIUS_MOCKER_MK8S_K3S_AGENT_CPU).
	AgentCPU float64
	// MemoryReserve is host memory admission always leaves free
	// (NEBIUS_MOCKER_MK8S_K3S_MEMORY_RESERVE).
	MemoryReserve int64
	// MaxAgents caps the agents across all clusters; 0 means only the host's
	// capacity limits them (NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS).
	MaxAgents int
	// StartTimeout bounds how long a server or agent may take to become
	// ready (NEBIUS_MOCKER_MK8S_K3S_START_TIMEOUT).
	StartTimeout time.Duration
}

// DefaultK3sImages are the k3s images of the seeded control-plane versions
// (multi-arch: amd64 and arm64).
func DefaultK3sImages() map[string]string {
	return map[string]string{
		"1.33": "rancher/k3s:v1.33.13-k3s1",
		"1.34": "rancher/k3s:v1.34.13-k3s1",
		"1.35": "rancher/k3s:v1.35.10-k3s1",
		"1.36": "rancher/k3s:v1.36.6-k3s1",
		"1.37": "rancher/k3s:v1.37.2-k3s1",
	}
}

const mebibyte = 1 << 20

func defaultMK8sK3s() MK8sK3sConfig {
	return MK8sK3sConfig{
		Images:        DefaultK3sImages(),
		BindIP:        "127.0.0.1",
		EndpointHost:  "127.0.0.1",
		ServerMemory:  768 * mebibyte,
		AgentMemory:   384 * mebibyte,
		AgentCPU:      0.5,
		MemoryReserve: 1024 * mebibyte,
		StartTimeout:  3 * time.Minute,
	}
}

func mk8sK3sParsers(k *MK8sK3sConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"MK8S_K3S_DOCKER_HOST", func(s string) error { return parseDockerHost(s, &k.DockerHost) }},
		{"MK8S_K3S_IMAGES", func(s string) error { return parseK3sImages(s, k.Images) }},
		{"MK8S_K3S_BIND_IP", func(s string) error { return parseIP(s, &k.BindIP) }},
		{"MK8S_K3S_ENDPOINT_HOST", func(s string) error { k.EndpointHost = s; return nil }},
		{"MK8S_K3S_WEBHOOK_URL", func(s string) error { return parseWebhookURL(s, &k.WebhookURL) }},
		{"MK8S_K3S_SERVER_MEMORY", func(s string) error { return parseBytes(s, &k.ServerMemory) }},
		{"MK8S_K3S_AGENT_MEMORY", func(s string) error { return parseBytes(s, &k.AgentMemory) }},
		{"MK8S_K3S_AGENT_CPU", func(s string) error { return parseCores(s, &k.AgentCPU) }},
		{"MK8S_K3S_MEMORY_RESERVE", func(s string) error { return parseBytes(s, &k.MemoryReserve) }},
		{"MK8S_K3S_MAX_AGENTS", func(s string) error { return parsePositiveInt(s, &k.MaxAgents) }},
		{"MK8S_K3S_START_TIMEOUT", func(s string) error { return parsePositive(s, &k.StartTimeout) }},
	}
}

// applyDockerHost falls back to Docker's own DOCKER_HOST when
// NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST is not set.
func applyDockerHost(lookup func(string) (string, bool), k *MK8sK3sConfig) error {
	if k.DockerHost != "" {
		return nil
	}
	v, ok := lookup("DOCKER_HOST")
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	if err := parseDockerHost(strings.TrimSpace(v), &k.DockerHost); err != nil {
		return fmt.Errorf("DOCKER_HOST: %w", err)
	}
	return nil
}

func parseDockerHost(s string, out *string) error {
	if !strings.HasPrefix(s, "unix://") && !strings.HasPrefix(s, "tcp://") {
		return fmt.Errorf("want unix:///path or tcp://host:port, got %q", s)
	}
	*out = s
	return nil
}

func parseK3sImages(s string, images map[string]string) error {
	for entry := range strings.SplitSeq(s, ",") {
		version, image, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || !mk8sVersionRE.MatchString(version) || image == "" {
			return fmt.Errorf("want <major>.<minor>=<image>[,…], got %q", entry)
		}
		images[version] = image
	}
	return nil
}

func parseIP(s string, out *string) error {
	if net.ParseIP(s) == nil {
		return fmt.Errorf("want an IP address, got %q", s)
	}
	*out = s
	return nil
}

func parseWebhookURL(s string, out *string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("want http(s)://host:port, got %q", s)
	}
	*out = u.Scheme + "://" + u.Host
	return nil
}

// parseBytes accepts a byte count with an optional Ki/Mi/Gi suffix.
func parseBytes(s string, out *int64) error {
	mult := int64(1)
	num := s
	for suffix, m := range map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30} {
		if v, ok := strings.CutSuffix(s, suffix); ok {
			num, mult = v, m
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("want a positive size such as 512Mi or 1Gi, got %q", s)
	}
	*out = n * mult
	return nil
}

func parseCores(s string, out *float64) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return fmt.Errorf("want a positive number of cores such as 0.5, got %q", s)
	}
	*out = f
	return nil
}
