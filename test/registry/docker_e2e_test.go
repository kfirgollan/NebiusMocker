//go:build e2e_registry

package registry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerEnv runs the docker CLI against the developer's daemon with an
// isolated client config (DOCKER_CONFIG in a temp dir), so logins never touch
// ~/.docker or the OS keychain.
type dockerEnv struct {
	env []string
}

func newDockerEnv(t *testing.T) *dockerEnv {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found on PATH")
	}
	// Resolve the active context's daemon before isolating the client config,
	// which also holds the context selection.
	host := strings.TrimSpace(mustOutput(t, exec.CommandContext(t.Context(), "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")))
	cfgDir := t.TempDir()
	// A non-empty "auths" makes docker store credentials in this file instead
	// of auto-detecting the OS credential store.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"auths":{"nebius-mocker.invalid":{}}}`), 0o600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	env := []string{"DOCKER_CONFIG=" + cfgDir, "DOCKER_HOST=" + host, "HOME=" + t.TempDir()}
	if path, ok := os.LookupEnv("PATH"); ok { //nolint:forbidigo // docker needs PATH for its plugins (buildx)
		env = append(env, "PATH="+path)
	}
	return &dockerEnv{env: env}
}

func (d *dockerEnv) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "docker", args...)
	c.Env = d.env
	return c
}

// run executes docker and returns combined output, failing the test on error.
func (d *dockerEnv) run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := d.try(t.Context(), args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (d *dockerEnv) try(ctx context.Context, args ...string) (string, error) {
	c := d.cmd(ctx, args...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return out.String(), err
}

// isDockerDesktop reports whether the daemon runs in Docker Desktop's VM,
// which cannot reach the host's loopback where the mocker listens.
func (d *dockerEnv) isDockerDesktop(t *testing.T) bool {
	t.Helper()
	return strings.Contains(d.run(t, "info", "--format", "{{.OperatingSystem}}"), "Docker Desktop")
}

// forwardLoopback makes host 127.0.0.1:<port> reachable as the daemon's own
// 127.0.0.1:<port> (an insecure-by-default registry address) by running
// nginx (Harbor's own goharbor/nginx-photon image) with host networking in
// the Docker Desktop VM, proxying to host.docker.internal. On a native Linux
// daemon loopback is shared and nothing is needed.
func (d *dockerEnv) forwardLoopback(t *testing.T, addr, nginxImage, confTemplate string) {
	t.Helper()
	if !d.isDockerDesktop(t) {
		return
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("forwarder: %v", err)
	}
	tmpl, err := os.ReadFile(confTemplate)
	if err != nil {
		t.Fatalf("forwarder config: %v", err)
	}
	conf := filepath.Join(t.TempDir(), "nginx.conf")
	if err := os.WriteFile(conf, bytes.ReplaceAll(tmpl, []byte("__PORT__"), []byte(port)), 0o600); err != nil {
		t.Fatalf("forwarder config: %v", err)
	}
	name := "nebius-mocker-regfwd-" + randomSuffix(t)
	d.run(t, "run", "-d", "--name", name, "--label", "nebius-mocker.test=registry",
		"--network", "host", "-v", conf+":/etc/nginx/nginx.conf:ro", "--entrypoint", "nginx", nginxImage, "-g", "daemon off;")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		_, _ = d.try(ctx, "rm", "-f", "-v", name) // best-effort: make e2e-registry also sweeps by label
	})
}

func mustOutput(t *testing.T, c *exec.Cmd) string {
	t.Helper()
	out, err := c.Output()
	if err != nil {
		t.Fatalf("%s: %v", strings.Join(c.Args, " "), err)
	}
	return string(out)
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// buildImage builds a one-file image FROM scratch (no base image to pull)
// with unique content, so every run pushes a new digest.
func (d *dockerEnv) buildImage(t *testing.T, ref string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"Dockerfile": "FROM scratch\nCOPY hello.txt /hello.txt\n",
		"hello.txt":  fmt.Sprintf("hello from nebius-mocker %s\n", randomSuffix(t)),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d.run(t, "build", "--quiet", "--tag", ref, dir)
}
