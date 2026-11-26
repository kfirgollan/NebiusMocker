//go:build e2e_mk8s

package mk8s

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
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
	"github.com/kfirgollan/NebiusMocker/test/internal/testca"
)

// testLabel marks the containers these tests start themselves (the user
// machine); make e2e-mk8s removes leftovers by it. The k3s containers are
// the mocker's and carry nebius-mocker.backend=k3s.
const testLabel = "nebius-mocker.test=mk8s"

// userImage plays the user's Linux machine.
const userImage = "alpine:3.22"

// apiHosts are the names the nebius CLI dials for a profile endpoint of
// api.nebius.cloud: "<service prefix>.api.nebius.cloud" (gosdk's convention
// resolver) for every service the scenario uses.
var apiHosts = []string{
	"api.nebius.cloud", "cpl.iam.api.nebius.cloud", "tokens.iam.api.nebius.cloud",
	"mk8s.api.nebius.cloud", "compute.api.nebius.cloud", "vpc.api.nebius.cloud",
}

// docker runs the docker CLI against the active context's daemon.
type docker struct {
	host string
}

func newDocker(t *testing.T) *docker {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found on PATH")
	}
	out, err := exec.CommandContext(t.Context(), "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		t.Skipf("docker context inspect (is Docker running?): %v", err)
	}
	return &docker{host: strings.TrimSpace(string(out))}
}

func (d *docker) try(ctx context.Context, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "docker", append([]string{"--host", d.host}, args...)...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return out.String(), err
}

func (d *docker) run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := d.try(t.Context(), args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// arch is the daemon's architecture as Go names it (amd64, arm64).
func (d *docker) arch(t *testing.T) string {
	t.Helper()
	a := strings.TrimSpace(d.run(t, "version", "--format", "{{.Server.Arch}}"))
	if a == "aarch64" {
		a = "arm64"
	}
	return a
}

// k3sContainers lists the mocker's k3s containers with label filter
// (e.g. "nebius-mocker.cluster=<id>").
func (d *docker) k3sContainers(t *testing.T, filter string) []string {
	t.Helper()
	out := d.run(t, "ps", "-a", "--filter", "label=nebius-mocker.backend=k3s", "--filter", "label="+filter, "--format", "{{.Names}}")
	return strings.Fields(out)
}

// userMachine is a Linux container playing the user's machine: it trusts
// the mocker's CA, resolves the API names to the Docker host (like
// /etc/hosts entries on a laptop), and has the nebius CLI (documented
// installer) and kubectl.
type userMachine struct {
	d    *docker
	name string
}

func startUserMachine(t *testing.T, d *docker, ca *testca.CA) *userMachine {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	arch := d.arch(t)
	cliDir := filepath.Join(cache, "nebius-mocker", "nebius-cli-linux", nebiuscli.Version+"-"+arch)
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "nebius-mocker-mk8s-user-" + randomSuffix(t)
	args := []string{
		"run", "-d", "--name", name, "--label", testLabel,
		"-v", ca.CAFile + ":/nebius-mocker-ca.crt:ro",
		"-v", kubectlBinary(t, "linux", arch) + ":/opt/nebius-mocker/bin/kubectl:ro",
		"-v", cliDir + ":/opt/nebius-cli",
	}
	for _, h := range append(slices.Clone(apiHosts), "host.docker.internal") {
		args = append(args, "--add-host", h+":host-gateway")
	}
	d.run(t, append(args, userImage, "sleep", "infinity")...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		_, _ = d.try(ctx, "rm", "-f", "-v", name) // best-effort: make e2e-mk8s also sweeps by label
	})
	u := &userMachine{d: d, name: name}
	// The documented installer needs bash, curl and GNU coreutils.
	u.sh(t, "apk add --no-cache --quiet bash curl coreutils ca-certificates jq")
	u.sh(t, "cp /nebius-mocker-ca.crt /usr/local/share/ca-certificates/nebius-mocker.crt && update-ca-certificates >/dev/null 2>&1")
	u.sh(t, fmt.Sprintf(`[ -x /opt/nebius-cli/nebius ] || (curl -sSL %s | NEBIUS_INSTALL_FOLDER=/opt/nebius-cli NEBIUS_CLI_VERSION=%s bash >/dev/null)`,
		nebiuscli.InstallerURL, nebiuscli.Version))
	return u
}

// sh runs script in the user machine and returns its output.
func (u *userMachine) sh(t *testing.T, script string) string {
	t.Helper()
	out, err := u.try(t.Context(), script)
	if err != nil {
		t.Fatalf("user machine: %v\n--- script\n%s\n--- output\n%s", err, script, out)
	}
	return out
}

func (u *userMachine) try(ctx context.Context, script string) (string, error) {
	return u.d.try(ctx, "exec", u.name, "sh", "-ec",
		"export PATH=/opt/nebius-cli:/opt/nebius-mocker/bin:$PATH\n"+script)
}

// eventually retries fn every 2s until it succeeds or timeout passes.
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
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-clk.After(2 * time.Second):
		}
	}
}

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
