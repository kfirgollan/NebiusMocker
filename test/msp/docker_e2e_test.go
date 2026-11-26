//go:build e2e_msp

package msp

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
)

// testLabel marks the containers these tests start themselves (the user
// machines); make e2e-msp removes leftovers by it. The clusters' containers
// are the mocker's and carry nebius-mocker.backend=msp-postgresql|msp-mlflow.
const testLabel = "nebius-mocker.test=msp"

// apiHosts are the names the nebius CLI dials for a profile endpoint of
// api.nebius.cloud (gosdk's convention resolver).
var apiHosts = []string{
	"api.nebius.cloud", "cpl.iam.api.nebius.cloud", "tokens.iam.api.nebius.cloud",
	"postgresql.msp.api.nebius.cloud", "mlflow.msp.api.nebius.cloud", "msp.api.nebius.cloud",
	"vpc.api.nebius.cloud", "storage.api.nebius.cloud",
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

// leftovers lists containers carrying the label filter (e.g.
// "nebius-mocker.backend=msp-postgresql").
func (d *docker) leftovers(t *testing.T, filter string) []string {
	t.Helper()
	return strings.Fields(d.run(t, "ps", "-a", "--filter", "label="+filter, "--format", "{{.Names}}"))
}

// userMachine is a Linux container playing the user's machine: it resolves
// the API names and host.docker.internal to the Docker host (like /etc/hosts
// entries on a laptop) and trusts the mocker's CAs.
type userMachine struct {
	d    *docker
	name string
}

type machineOpts struct {
	image string
	// mounts are extra "-v" values.
	mounts []string
}

func startMachine(t *testing.T, d *docker, o machineOpts) *userMachine {
	t.Helper()
	name := "nebius-mocker-msp-user-" + randomSuffix(t)
	args := []string{"run", "-d", "--name", name, "--label", testLabel, "--entrypoint", "sleep"}
	for _, m := range o.mounts {
		args = append(args, "-v", m)
	}
	for _, h := range append(slices.Clone(apiHosts), "host.docker.internal") {
		args = append(args, "--add-host", h+":host-gateway")
	}
	d.run(t, append(args, o.image, "infinity")...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		_, _ = d.try(ctx, "rm", "-f", "-v", name) // best-effort: make e2e-msp also sweeps by label
	})
	return &userMachine{d: d, name: name}
}

// startCLIMachine is a machine with the nebius CLI (documented installer)
// and psql: the PostgreSQL image, which is Alpine.
func startCLIMachine(t *testing.T, e *env) *userMachine {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cliDir := filepath.Join(cache, "nebius-mocker", "nebius-cli-linux", nebiuscli.Version+"-"+e.d.arch(t))
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	u := startMachine(t, e.d, machineOpts{image: postgresImage, mounts: []string{
		e.ca.CAFile + ":/nebius-mocker-ca.crt:ro",
		filepath.Dir(e.mspCA) + ":/msp-ca:ro",
		cliDir + ":/opt/nebius-cli",
	}})
	// The documented installer needs bash, curl and GNU coreutils.
	u.sh(t, "apk add --no-cache --quiet bash curl coreutils ca-certificates jq")
	u.sh(t, "cp /nebius-mocker-ca.crt /usr/local/share/ca-certificates/nebius-mocker.crt && update-ca-certificates >/dev/null 2>&1")
	u.sh(t, fmt.Sprintf(`[ -x /opt/nebius-cli/nebius ] || (curl -sSL %s | NEBIUS_INSTALL_FOLDER=/opt/nebius-cli NEBIUS_CLI_VERSION=%s bash >/dev/null)`,
		nebiuscli.InstallerURL, nebiuscli.Version))
	// What the PostgreSQL docs tell users: the CA in ~/.postgresql/root.crt.
	u.sh(t, "mkdir -p ~/.postgresql && cp /msp-ca/ca.pem ~/.postgresql/root.crt")
	return u
}

// sh runs script in the machine and returns its output.
func (u *userMachine) sh(t *testing.T, script string) string {
	t.Helper()
	out, err := u.try(t.Context(), script)
	if err != nil {
		t.Fatalf("user machine: %v\n--- script\n%s\n--- output\n%s", err, script, out)
	}
	return out
}

func (u *userMachine) try(ctx context.Context, script string) (string, error) {
	return u.d.try(ctx, "exec", u.name, "sh", "-ec", "export PATH=/opt/nebius-cli:$PATH\n"+script)
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
