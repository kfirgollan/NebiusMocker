// Package rustfsfixture runs a RustFS server in Docker for end-to-end tests.
//
// Start launches one container per test binary (on a random localhost port)
// and waits until it serves authenticated S3 requests; Main removes it when
// the tests finish. Every container carries the label
// "nebius-mocker.test=rustfs", so `make e2e-storage` can remove leftovers of
// a killed run. Set NEBIUS_MOCKER_TEST_RUSTFS_ENDPOINT (and optionally
// _ACCESS_KEY / _SECRET_KEY) to use an already running server instead.
//
// Usage:
//
//	func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }
//
//	srv := rustfsfixture.Start(t) // srv.Endpoint, srv.AccessKey, srv.SecretKey, srv.Client(t)
package rustfsfixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	rfs "github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
)

// Image is the pinned RustFS image (multi-arch: amd64 and arm64).
const Image = "rustfs/rustfs:1.0.6"

// Label marks every container this package starts.
const Label = "nebius-mocker.test=rustfs"

const startTimeout = 90 * time.Second

// Server is a reachable RustFS instance.
type Server struct {
	Endpoint  string
	AccessKey string
	SecretKey string
}

// Client returns an objstore client for the server.
func (s *Server) Client(tb testing.TB) *rfs.Client {
	tb.Helper()
	c, err := rfs.New(rfs.Config{Endpoint: s.Endpoint, AccessKey: s.AccessKey, SecretKey: s.SecretKey})
	if err != nil {
		tb.Fatalf("rustfs client: %v", err)
	}
	return c
}

var (
	once      sync.Once
	shared    *Server
	errStart  error
	container string
)

// Start returns the test binary's RustFS server, starting it on first use.
// It skips the test when Docker is not available.
func Start(tb testing.TB) *Server {
	tb.Helper()
	once.Do(func() { shared, errStart = start() })
	if errStart != nil {
		if strings.Contains(errStart.Error(), "docker not found") {
			tb.Skipf("rustfs: %v", errStart)
		}
		tb.Fatalf("rustfs: %v", errStart)
	}
	return shared
}

// Main runs the tests and then removes the container Start created.
func Main(m *testing.M) int {
	code := m.Run()
	Cleanup()
	return code
}

// Cleanup removes the container Start created (for TestMains that also run
// other teardown; Main calls it).
func Cleanup() {
	if container != "" {
		if out, err := exec.Command("docker", "rm", "-f", "-v", container).CombinedOutput(); err != nil { //nolint:noctx // teardown after all tests; no test context exists
			fmt.Fprintf(os.Stderr, "rustfs: remove container %s: %v: %s\n", container, err, out)
		}
	}
}

func start() (*Server, error) {
	if ep, ok := os.LookupEnv("NEBIUS_MOCKER_TEST_RUSTFS_ENDPOINT"); ok && ep != "" { //nolint:forbidigo // test-harness setting, not mocker configuration
		s := &Server{Endpoint: ep, AccessKey: envOr("NEBIUS_MOCKER_TEST_RUSTFS_ACCESS_KEY", "rustfsadmin"), SecretKey: envOr("NEBIUS_MOCKER_TEST_RUSTFS_SECRET_KEY", "rustfsadmin")}
		return s, waitReady(s)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker not found on PATH: %w", err)
	}
	s := &Server{AccessKey: "nmock-" + randomHex(6), SecretKey: randomHex(20)}
	name := "nebius-mocker-rustfs-" + randomHex(4)
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout) //nolint:forbidigo // process-wide fixture shared by all tests of the binary
	defer cancel()
	reapOrphans(ctx)
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--name", name, "--label", Label,
		"--label", pidLabel+"="+strconv.Itoa(os.Getpid()),
		"-p", "127.0.0.1::9000",
		"-e", "RUSTFS_ACCESS_KEY="+s.AccessKey, "-e", "RUSTFS_SECRET_KEY="+s.SecretKey,
		Image).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %w: %s", Image, err, out)
	}
	container = name
	port, err := exec.CommandContext(ctx, "docker", "port", name, "9000/tcp").Output()
	if err != nil {
		return nil, fmt.Errorf("docker port %s: %w", name, err)
	}
	// "127.0.0.1:55012" (possibly several lines, one per address family).
	first, _, _ := strings.Cut(strings.TrimSpace(string(port)), "\n")
	s.Endpoint = "http://" + strings.TrimSpace(first)
	if err := waitReady(s); err != nil {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput() //nolint:noctx // diagnostics only
		return nil, fmt.Errorf("%w; container logs:\n%s", err, logs)
	}
	return s, nil
}

// pidLabel records the test process that owns a container.
const pidLabel = "nebius-mocker.test.pid"

// reapOrphans removes labelled containers whose owning test process is gone
// (a killed or timed-out run). Containers of live runs, e.g. a concurrent
// suite in another package or checkout, are left alone.
func reapOrphans(ctx context.Context) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label="+Label,
		"--format", `{{.ID}} {{.Label "`+pidLabel+`"}}`).Output()
	if err != nil {
		return // best-effort
	}
	for line := range strings.Lines(string(out)) {
		id, pidStr, _ := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(pidStr)
		if id == "" || (err == nil && processAlive(pid)) {
			continue
		}
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", id).Run() // best-effort
	}
}

// processAlive reports whether a local process with pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// waitReady polls until the server answers an authenticated ListBuckets.
func waitReady(s *Server) error {
	c, err := rfs.New(rfs.Config{Endpoint: s.Endpoint, AccessKey: s.AccessKey, SecretKey: s.SecretKey})
	if err != nil {
		return err
	}
	clk := clock.Real{}
	deadline := clk.Now().Add(startTimeout)
	var last error
	for clk.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) //nolint:forbidigo // process-wide fixture readiness probe
		last = c.Ping(ctx)
		cancel()
		if last == nil {
			return nil
		}
		<-clk.After(250 * time.Millisecond)
	}
	return fmt.Errorf("rustfs at %s not ready after %s: %w", s.Endpoint, startTimeout, last)
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" { //nolint:forbidigo // test-harness setting, not mocker configuration
		return v
	}
	return def
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
