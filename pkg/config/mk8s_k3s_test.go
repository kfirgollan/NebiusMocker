package config

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestMK8sK3sDefaults(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(nil))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := MK8sK3sConfig{
		Images:        DefaultK3sImages(),
		BindIP:        "127.0.0.1",
		EndpointHost:  "127.0.0.1",
		ServerMemory:  768 << 20,
		AgentMemory:   384 << 20,
		AgentCPU:      0.5,
		MemoryReserve: 1 << 30,
		StartTimeout:  3 * time.Minute,
	}
	if diff := cmp.Diff(want, c.MK8sK3s); diff != "" {
		t.Fatalf("default k3s config mismatch (-want +got):\n%s", diff)
	}
	for _, v := range []string{"1.33", "1.34", "1.35", "1.36", "1.37"} {
		if !strings.HasPrefix(c.MK8sK3s.Images[v], "rancher/k3s:v"+v+".") {
			t.Errorf("default image of %s = %q, want a rancher/k3s:v%s.* tag", v, c.MK8sK3s.Images[v], v)
		}
	}
}

func TestMK8sK3sParsed(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST":    "tcp://docker.internal:2375",
		"NEBIUS_MOCKER_MK8S_K3S_IMAGES":         "1.35=registry.local/k3s:custom, 1.40=registry.local/k3s:new",
		"NEBIUS_MOCKER_MK8S_K3S_BIND_IP":        "0.0.0.0",
		"NEBIUS_MOCKER_MK8S_K3S_ENDPOINT_HOST":  "k8s.localhost",
		"NEBIUS_MOCKER_MK8S_K3S_WEBHOOK_URL":    "https://mocker.internal:4566/",
		"NEBIUS_MOCKER_MK8S_K3S_SERVER_MEMORY":  "1Gi",
		"NEBIUS_MOCKER_MK8S_K3S_AGENT_MEMORY":   "512Mi",
		"NEBIUS_MOCKER_MK8S_K3S_AGENT_CPU":      "1.5",
		"NEBIUS_MOCKER_MK8S_K3S_MEMORY_RESERVE": "2048",
		"NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS":     "7",
		"NEBIUS_MOCKER_MK8S_K3S_START_TIMEOUT":  "90s",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	k := c.MK8sK3s
	if k.DockerHost != "tcp://docker.internal:2375" || k.BindIP != "0.0.0.0" || k.EndpointHost != "k8s.localhost" ||
		k.WebhookURL != "https://mocker.internal:4566" || k.ServerMemory != 1<<30 || k.AgentMemory != 512<<20 ||
		k.AgentCPU != 1.5 || k.MemoryReserve != 2048 || k.MaxAgents != 7 || k.StartTimeout != 90*time.Second {
		t.Fatalf("parsed k3s config = %+v", k)
	}
	if k.Images["1.35"] != "registry.local/k3s:custom" || k.Images["1.40"] != "registry.local/k3s:new" {
		t.Errorf("images = %v, want the listed entries", k.Images)
	}
	if k.Images["1.36"] != DefaultK3sImages()["1.36"] {
		t.Errorf("image of 1.36 = %q, want the default kept", k.Images["1.36"])
	}
}

func TestMK8sK3sEmptyKeepsDefault(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(map[string]string{"NEBIUS_MOCKER_MK8S_K3S_BIND_IP": "", "NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS": ""}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if c.MK8sK3s.BindIP != "127.0.0.1" || c.MK8sK3s.MaxAgents != 0 {
		t.Fatalf("empty values changed the defaults: %+v", c.MK8sK3s)
	}
}

func TestMK8sK3sRejectsBadValues(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, value string }{
		{"DOCKER_HOST", "http://docker:2375"},
		{"DOCKER_HOST", "/var/run/docker.sock"},
		{"IMAGES", "1.35"},
		{"IMAGES", "1.35="},
		{"IMAGES", "latest=rancher/k3s"},
		{"BIND_IP", "localhost"},
		{"WEBHOOK_URL", "mocker:4566"},
		{"WEBHOOK_URL", "ftp://mocker:4566"},
		{"WEBHOOK_URL", "http://mocker:4566/some/path"},
		{"SERVER_MEMORY", "0"},
		{"SERVER_MEMORY", "lots"},
		{"AGENT_MEMORY", "-1Mi"},
		{"AGENT_CPU", "0"},
		{"AGENT_CPU", "many"},
		{"MEMORY_RESERVE", "1Ti"},
		{"MAX_AGENTS", "0"},
		{"MAX_AGENTS", "-3"},
		{"MAX_AGENTS", "many"},
		{"START_TIMEOUT", "0s"},
		{"START_TIMEOUT", "soon"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"="+tt.value, func(t *testing.T) {
			t.Parallel()
			name := "NEBIUS_MOCKER_MK8S_K3S_" + tt.name
			_, err := FromLookup(lookup(map[string]string{name: tt.value}))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("FromLookup(%s=%q) = %v, want an error naming %s", name, tt.value, err, name)
			}
		})
	}
}

func TestMK8sK3sDockerHostFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{name: "unset", env: nil, want: ""},
		{name: "DOCKER_HOST only", env: map[string]string{"DOCKER_HOST": "unix:///var/run/docker.sock"}, want: "unix:///var/run/docker.sock"},
		{name: "DOCKER_HOST is trimmed", env: map[string]string{"DOCKER_HOST": "  tcp://d:2375 "}, want: "tcp://d:2375"},
		{name: "blank DOCKER_HOST", env: map[string]string{"DOCKER_HOST": "  "}, want: ""},
		{
			name: "mocker setting wins",
			env:  map[string]string{"NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST": "tcp://mine:2375", "DOCKER_HOST": "unix:///other.sock"},
			want: "tcp://mine:2375",
		},
		{
			name: "mocker setting wins over a bad DOCKER_HOST",
			env:  map[string]string{"NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST": "tcp://mine:2375", "DOCKER_HOST": "ssh://x"},
			want: "tcp://mine:2375",
		},
		{name: "bad DOCKER_HOST", env: map[string]string{"DOCKER_HOST": "ssh://x"}, wantErr: "DOCKER_HOST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := FromLookup(lookup(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("FromLookup = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromLookup: %v", err)
			}
			if c.MK8sK3s.DockerHost != tt.want {
				t.Fatalf("DockerHost = %q, want %q", c.MK8sK3s.DockerHost, tt.want)
			}
		})
	}
}
