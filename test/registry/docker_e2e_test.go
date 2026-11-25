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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// testLabel marks every container these tests start; make e2e-registry
// removes leftovers by it.
const testLabel = "nebius-mocker.test=registry"

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
	out, err := exec.CommandContext(t.Context(), "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		t.Fatalf("docker context inspect: %v", err)
	}
	cfgDir := t.TempDir()
	// A non-empty "auths" makes docker store credentials in this file instead
	// of auto-detecting the OS credential store.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"auths":{"nebius-mocker.invalid":{}}}`), 0o600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	env := []string{"DOCKER_CONFIG=" + cfgDir, "DOCKER_HOST=" + strings.TrimSpace(string(out)), "HOME=" + t.TempDir()}
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

// runContainer starts a detached, labelled container and removes it (with its
// volumes) when the test ends. It returns the container name.
func (d *dockerEnv) runContainer(t *testing.T, prefix string, args ...string) string {
	t.Helper()
	name := prefix + "-" + randomSuffix(t)
	d.run(t, slices.Concat([]string{"run", "-d", "--name", name, "--label", testLabel}, args)...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		if t.Failed() {
			logs, _ := d.try(ctx, "logs", "--tail", "60", name)
			t.Logf("logs of %s:\n%s", name, logs)
		}
		_, _ = d.try(ctx, "rm", "-f", "-v", name) // best-effort: make e2e-registry also sweeps by label
	})
	return name
}

// waitLog waits until the container logs contain want.
func (d *dockerEnv) waitLog(t *testing.T, name, want string) {
	t.Helper()
	eventually(t, time.Minute, func() error {
		out, err := d.try(t.Context(), "logs", name)
		if err == nil && strings.Contains(out, want) {
			return nil
		}
		return fmt.Errorf("%s logs do not contain %q yet:\n%s", name, want, out)
	})
}

// eventually retries fn every second until it succeeds or timeout passes.
func eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	clk := clock.Real{}
	deadline := clk.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return
		}
		if clk.Now().After(deadline) {
			t.Fatalf("timed out after %s: %v", timeout, err)
		}
		<-clk.After(time.Second)
	}
}

// freePort returns a currently unused loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = ln.Close() }() // best-effort: probing only
	return ln.Addr().(*net.TCPAddr).Port
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// imageContext writes a one-file image context FROM scratch (no base image to
// pull) with unique content, so every run pushes a new digest.
func imageContext(t *testing.T) (dockerfile, file string) {
	t.Helper()
	return "FROM scratch\nCOPY hello.txt /hello.txt\n", fmt.Sprintf("hello from nebius-mocker %s\n", randomSuffix(t))
}

// buildImage builds the image context on the host daemon.
func (d *dockerEnv) buildImage(t *testing.T, ref string) {
	t.Helper()
	dir := t.TempDir()
	dockerfile, content := imageContext(t)
	for name, data := range map[string]string{"Dockerfile": dockerfile, "hello.txt": content} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d.run(t, "build", "--quiet", "--tag", ref, dir)
}

func itoa(n int) string { return strconv.Itoa(n) }
