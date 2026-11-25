package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AIConfig configures the AI endpoints area (NEBIUS_MOCKER_AI_*).
type AIConfig struct {
	// Listen is the address of the stub endpoint data plane, served over
	// HTTPS (NEBIUS_MOCKER_AI_LISTEN, default 127.0.0.1:4600).
	Listen string
	// PublicURL, when set, is the base URL written into endpoint statuses
	// instead of one derived from the bound listener
	// (NEBIUS_MOCKER_AI_PUBLIC_URL, e.g. "https://mocker.local:4600").
	PublicURL string
	// TCPListen is the address of the TLS stub listener for TCP ports
	// (NEBIUS_MOCKER_AI_TCP_LISTEN, default 127.0.0.1:4601).
	TCPListen string
	// TCPDomain is the DNS suffix of TCP port addresses,
	// tls://<endpoint-id>-p<port>.<TCPDomain>:<port of TCPListen>
	// (NEBIUS_MOCKER_AI_TCP_DOMAIN, default ai.localhost, which resolves to
	// loopback).
	TCPDomain string
	// TLSDir holds the local CA that signs the data plane's certificates;
	// clients trust <TLSDir>/ca.pem (NEBIUS_MOCKER_AI_TLS_DIR, default
	// <user config dir>/nebius-mocker/ca).
	TLSDir string
	// Platforms is the compute platform catalog endpoints are validated
	// against (NEBIUS_MOCKER_AI_PLATFORMS, JSON). It stands in for a compute
	// PlatformService until one exists.
	Platforms []AIPlatform
}

// AIPlatform is one platform of the AI catalog.
type AIPlatform struct {
	Name string `json:"name"`
	// GPU is the model matched by spec.gpu selectors ("H100"); empty for CPU platforms.
	GPU string `json:"gpu,omitempty"`
	// CPU is the family matched by spec.cpu selectors ("amd"); empty for GPU platforms.
	CPU     string     `json:"cpu,omitempty"`
	Presets []AIPreset `json:"presets"`
}

// AIPreset is one preset of an AIPlatform.
type AIPreset struct {
	Name  string `json:"name"`
	GPUs  int    `json:"gpus,omitempty"`
	VCPUs int    `json:"vcpus"`
}

// DefaultAIPlatforms is a small subset of Nebius platforms and presets.
func DefaultAIPlatforms() []AIPlatform {
	return []AIPlatform{
		{Name: "cpu-d3", CPU: "amd", Presets: []AIPreset{
			{Name: "2vcpu-8gb", VCPUs: 2},
			{Name: "4vcpu-16gb", VCPUs: 4},
			{Name: "8vcpu-32gb", VCPUs: 8},
			{Name: "16vcpu-64gb", VCPUs: 16},
			{Name: "32vcpu-128gb", VCPUs: 32},
			{Name: "48vcpu-192gb", VCPUs: 48},
		}},
		{Name: "cpu-e2", CPU: "intel", Presets: []AIPreset{
			{Name: "2vcpu-8gb", VCPUs: 2},
			{Name: "4vcpu-16gb", VCPUs: 4},
			{Name: "8vcpu-32gb", VCPUs: 8},
			{Name: "16vcpu-64gb", VCPUs: 16},
		}},
		{Name: "gpu-h100-sxm", GPU: "H100", Presets: []AIPreset{
			{Name: "1gpu-16vcpu-200gb", GPUs: 1, VCPUs: 16}, {Name: "8gpu-128vcpu-1600gb", GPUs: 8, VCPUs: 128},
		}},
		{Name: "gpu-h200-sxm", GPU: "H200", Presets: []AIPreset{
			{Name: "1gpu-16vcpu-200gb", GPUs: 1, VCPUs: 16}, {Name: "8gpu-128vcpu-1600gb", GPUs: 8, VCPUs: 128},
		}},
		{Name: "gpu-l40s-a", GPU: "L40S", Presets: []AIPreset{
			{Name: "1gpu-8vcpu-32gb", GPUs: 1, VCPUs: 8}, {Name: "1gpu-16vcpu-64gb", GPUs: 1, VCPUs: 16},
		}},
	}
}

func defaultAI() AIConfig {
	return AIConfig{
		Listen:    "127.0.0.1:4600",
		TCPListen: "127.0.0.1:4601",
		TCPDomain: "ai.localhost",
		TLSDir:    defaultCADir(),
		Platforms: DefaultAIPlatforms(),
	}
}

// defaultCADir is where the mocker keeps its local CA by default.
func defaultCADir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "nebius-mocker", "ca")
}

func parseAIPlatforms(v string, out *[]AIPlatform) error {
	var ps []AIPlatform
	if err := json.Unmarshal([]byte(v), &ps); err != nil {
		return fmt.Errorf("invalid platform catalog JSON: %w", err)
	}
	if len(ps) == 0 {
		return errors.New("platform catalog must not be empty")
	}
	for _, p := range ps {
		if p.Name == "" || len(p.Presets) == 0 {
			return fmt.Errorf("platform %q needs a name and at least one preset", p.Name)
		}
	}
	*out = ps
	return nil
}
