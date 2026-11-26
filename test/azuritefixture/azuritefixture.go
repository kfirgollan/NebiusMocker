// Package azuritefixture runs the official Azurite (Azure Storage emulator) in
// Docker for end-to-end tests of Azure Blob Storage transfer sources.
//
// Start launches one container per test binary (on a random localhost port)
// and waits until it serves authenticated blob requests; Main removes it when
// the tests finish. Every container carries the label
// "nebius-mocker.test=azurite", so a Makefile target can remove leftovers of
// a killed run. Set NEBIUS_MOCKER_TEST_AZURITE_ENDPOINT to use an already
// running Azurite (for example "http://127.0.0.1:10000/devstoreaccount1").
//
// Usage:
//
//	func TestMain(m *testing.M) { os.Exit(azuritefixture.Main(m)) }
//
//	srv := azuritefixture.Start(t) // srv.Endpoint, srv.Account, srv.Key
//	srv.CreateContainer(t, "docs")
//	srv.Upload(t, "docs", "a/b.txt", []byte("hi"), azuritefixture.BlobOptions{})
package azuritefixture

import (
	"bytes"
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

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// Image is the pinned Azurite image (multi-arch: amd64 and arm64).
const Image = "mcr.microsoft.com/azure-storage/azurite:3.35.0"

// Label marks every container this package starts.
const Label = "nebius-mocker.test=azurite"

// DevAccount and DevKey are Azurite's documented well-known development
// storage account.
const (
	DevAccount = "devstoreaccount1"
	DevKey     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

const startTimeout = 90 * time.Second

// Server is a reachable Azurite instance.
type Server struct {
	// Endpoint is the blob service URL including the account path, e.g.
	// "http://127.0.0.1:55012/devstoreaccount1".
	Endpoint string
	Account  string
	Key      string
}

// Client returns an azblob client for the server using the account key.
func (s *Server) Client(tb testing.TB) *azblob.Client {
	tb.Helper()
	cred, err := azblob.NewSharedKeyCredential(s.Account, s.Key)
	if err != nil {
		tb.Fatalf("azurite credential: %v", err)
	}
	c, err := azblob.NewClientWithSharedKeyCredential(s.Endpoint, cred, nil)
	if err != nil {
		tb.Fatalf("azurite client: %v", err)
	}
	return c
}

// CreateContainer creates a private container.
func (s *Server) CreateContainer(tb testing.TB, name string) {
	tb.Helper()
	s.create(tb, name, nil)
}

// CreatePublicContainer creates a container whose listing and blobs are
// readable anonymously.
func (s *Server) CreatePublicContainer(tb testing.TB, name string) {
	tb.Helper()
	access := container.PublicAccessTypeContainer
	s.create(tb, name, &access)
}

func (s *Server) create(tb testing.TB, name string, access *container.PublicAccessType) {
	tb.Helper()
	if _, err := s.Client(tb).CreateContainer(tb.Context(), name, &container.CreateOptions{Access: access}); err != nil {
		tb.Fatalf("azurite: create container %q: %v", name, err)
	}
}

// BlobOptions are the attributes a seeded blob carries.
type BlobOptions struct {
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	Metadata           map[string]string
}

// Upload writes a block blob, as a user seeds an Azure container.
func (s *Server) Upload(tb testing.TB, containerName, name string, data []byte, o BlobOptions) {
	tb.Helper()
	h := blob.HTTPHeaders{}
	if o.ContentType != "" {
		h.BlobContentType = &o.ContentType
	}
	if o.CacheControl != "" {
		h.BlobCacheControl = &o.CacheControl
	}
	if o.ContentDisposition != "" {
		h.BlobContentDisposition = &o.ContentDisposition
	}
	if o.ContentEncoding != "" {
		h.BlobContentEncoding = &o.ContentEncoding
	}
	if o.ContentLanguage != "" {
		h.BlobContentLanguage = &o.ContentLanguage
	}
	var md map[string]*string
	if len(o.Metadata) > 0 {
		md = make(map[string]*string, len(o.Metadata))
		for k, v := range o.Metadata {
			md[k] = &v
		}
	}
	if _, err := s.Client(tb).UploadStream(tb.Context(), containerName, name, bytes.NewReader(data), &azblob.UploadStreamOptions{
		HTTPHeaders: &h, Metadata: md,
	}); err != nil {
		tb.Fatalf("azurite: upload %s/%s: %v", containerName, name, err)
	}
}

var (
	once     sync.Once
	shared   *Server
	errStart error
	started  string
)

// Start returns the test binary's Azurite server, starting it on first use.
// It skips the test when Docker is not available.
func Start(tb testing.TB) *Server {
	tb.Helper()
	once.Do(func() { shared, errStart = start() })
	if errStart != nil {
		if strings.Contains(errStart.Error(), "docker not found") {
			tb.Skipf("azurite: %v", errStart)
		}
		tb.Fatalf("azurite: %v", errStart)
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
	if started != "" {
		if out, err := exec.Command("docker", "rm", "-f", "-v", started).CombinedOutput(); err != nil { //nolint:noctx // teardown after all tests; no test context exists
			fmt.Fprintf(os.Stderr, "azurite: remove container %s: %v: %s\n", started, err, out)
		}
	}
}

func start() (*Server, error) {
	if ep, ok := os.LookupEnv("NEBIUS_MOCKER_TEST_AZURITE_ENDPOINT"); ok && ep != "" { //nolint:forbidigo // test-harness setting, not mocker configuration
		s := &Server{Endpoint: ep, Account: DevAccount, Key: DevKey}
		return s, waitReady(s)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker not found on PATH: %w", err)
	}
	name := "nebius-mocker-azurite-" + randomHex(4)
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout) //nolint:forbidigo // process-wide fixture shared by all tests of the binary
	defer cancel()
	reapOrphans(ctx)
	// --skipApiVersionCheck: the current azblob SDK speaks newer API versions
	// than the Azurite release knows.
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--name", name, "--label", Label,
		"--label", pidLabel+"="+strconv.Itoa(os.Getpid()),
		"-p", "127.0.0.1::10000",
		Image, "azurite-blob", "--blobHost", "0.0.0.0", "--loose", "--skipApiVersionCheck").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %w: %s", Image, err, out)
	}
	started = name
	port, err := exec.CommandContext(ctx, "docker", "port", name, "10000/tcp").Output()
	if err != nil {
		return nil, fmt.Errorf("docker port %s: %w", name, err)
	}
	// "127.0.0.1:55012" (possibly several lines, one per address family).
	first, _, _ := strings.Cut(strings.TrimSpace(string(port)), "\n")
	s := &Server{Endpoint: "http://" + strings.TrimSpace(first) + "/" + DevAccount, Account: DevAccount, Key: DevKey}
	if err := waitReady(s); err != nil {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput() //nolint:noctx // diagnostics only
		return nil, fmt.Errorf("%w; container logs:\n%s", err, logs)
	}
	return s, nil
}

// pidLabel records the test process that owns a container.
const pidLabel = "nebius-mocker.test.pid"

// reapOrphans removes labelled containers whose owning test process is gone
// (a killed or timed-out run). Containers of live runs are left alone.
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

// waitReady polls until the server answers an authenticated list of containers.
func waitReady(s *Server) error {
	cred, err := azblob.NewSharedKeyCredential(s.Account, s.Key)
	if err != nil {
		return err
	}
	c, err := azblob.NewClientWithSharedKeyCredential(s.Endpoint, cred, nil)
	if err != nil {
		return err
	}
	clk := clock.Real{}
	deadline := clk.Now().Add(startTimeout)
	var last error
	for clk.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) //nolint:forbidigo // process-wide fixture readiness probe
		pager := c.NewListContainersPager(nil)
		_, last = pager.NextPage(ctx)
		cancel()
		if last == nil {
			return nil
		}
		<-clk.After(250 * time.Millisecond)
	}
	return fmt.Errorf("azurite at %s not ready after %s: %w", s.Endpoint, startTimeout, last)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
