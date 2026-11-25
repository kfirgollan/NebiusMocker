// Package nebiuscli installs the official Nebius CLI into a sandbox and runs it
// against an in-process NebiusMocker, for end-to-end tests.
//
// Nothing is installed system-wide and the developer's ~/.nebius is never
// touched:
//   - Binary downloads Nebius's documented installer
//     (https://storage.eu-north1.nebius.cloud/cli/install.sh) and runs it with
//     HOME, TMPDIR and NEBIUS_INSTALL_FOLDER pointing into the user cache dir
//     (<UserCacheDir>/nebius-mocker/nebius-cli/<Version>), once per version.
//   - New creates a per-test HOME holding a token file and a profile created
//     with "nebius profile create", pointing at the mocker.
//
// How the CLI is pointed at the mock:
//   - The profile endpoint is "localhost:<port>". The CLI resolves each service
//     as "<service-prefix>.<endpoint>" (e.g. cpl.iam.localhost:4566,
//     registry.localhost:4566), and "*.localhost" resolves to the loopback
//     address, so every service reaches the mock's single gRPC port. An IP
//     endpoint does not work ("registry.127.0.0.1" fails to resolve).
//   - Plaintext gRPC needs the global --insecure flag on every command; the
//     profile does not store it. Run adds it.
//   - Authentication is a static IAM token through the profile's --token-file
//     (read on every command), so no token exchange is involved.
package nebiuscli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Version is the pinned CLI release installed by Binary.
const Version = "0.12.312"

// InstallerURL is Nebius's documented CLI installer.
const InstallerURL = "https://storage.eu-north1.nebius.cloud/cli/install.sh"

// ProfileName is the name of the profile New creates and activates.
const ProfileName = "mocker"

// Profile describes what the CLI profile points at.
type Profile struct {
	// Addr is the mocker's gRPC address, "host:port" (e.g. testkit Instance.Addr).
	Addr string
	// Token is the bearer token the CLI sends (e.g. testkit Instance.Token).
	Token string
	// ParentID is the default parent (project) for commands; optional.
	ParentID string
	// TenantID is the default tenant; optional.
	TenantID string
}

// CLI runs the sandboxed nebius binary with an isolated HOME.
type CLI struct {
	bin  string
	home string
}

// Result is the outcome of one command.
type Result struct {
	Stdout string
	Stderr string
}

// Binary returns the path of the pinned CLI, installing it on first use. The
// test is skipped when the installer cannot be downloaded (offline); any other
// install failure fails it.
func Binary(tb testing.TB) string {
	tb.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		tb.Fatalf("nebiuscli: user cache dir: %v", err)
	}
	dir := filepath.Join(cache, "nebius-mocker", "nebius-cli", Version)
	bin := filepath.Join(dir, "nebius")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	if err := install(tb, dir); err != nil {
		tb.Fatalf("nebiuscli: install %s: %v", Version, err)
	}
	return bin
}

// install runs the official installer into a staging dir, then renames it into
// place so concurrent test binaries never see a half-installed CLI.
func install(tb testing.TB, dir string) error {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), "install-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }() // best-effort: staging leftovers
	script := filepath.Join(stage, "install.sh")
	if err := download(tb.Context(), InstallerURL, script); err != nil {
		tb.Skipf("nebiuscli: cannot download the CLI installer (offline?): %v", err)
	}
	home := filepath.Join(stage, "home")
	tmp := filepath.Join(stage, "tmp")
	binDir := filepath.Join(stage, "bin")
	for _, d := range []string{home, tmp} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(tb.Context(), "bash", script)
	cmd.Env = append(baseEnv(),
		"HOME="+home,
		"TMPDIR="+tmp,
		"SHELL=/bin/sh", // no shell rc patching or completion install
		"NEBIUS_INSTALL_FOLDER="+binDir,
		"NEBIUS_CLI_VERSION="+Version,
	)
	cmd.Stdin = nil // not a terminal: the installer skips its prompts
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("installer: %w\n%s", err, out)
	}
	if err := os.Rename(binDir, dir); err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, "nebius")); statErr == nil {
			return nil // another test binary won the race
		}
		return fmt.Errorf("move into cache: %w", err)
	}
	return nil
}

func download(ctx context.Context, url, dst string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download installer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort: read-only body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download installer: HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = io.Copy(f, resp.Body)
	return err
}

// New installs the CLI if needed and creates an isolated HOME with an active
// profile pointing at p.Addr, authenticated with p.Token via a token file.
func New(tb testing.TB, p Profile) *CLI {
	tb.Helper()
	c := &CLI{bin: Binary(tb), home: tb.TempDir()}
	tokenFile := filepath.Join(c.home, "token")
	if err := os.WriteFile(tokenFile, []byte(p.Token), 0o600); err != nil {
		tb.Fatalf("nebiuscli: write token file: %v", err)
	}
	args := []string{"profile", "create", ProfileName, "--endpoint", Endpoint(tb, p.Addr), "--token-file", tokenFile}
	if p.ParentID != "" {
		args = append(args, "--parent-id", p.ParentID)
	}
	if p.TenantID != "" {
		args = append(args, "--tenant-id", p.TenantID)
	}
	if res, err := c.exec(tb.Context(), nil, args); err != nil {
		tb.Fatalf("nebiuscli: %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, res.Stdout, res.Stderr)
	}
	return c
}

// AddServiceAccountProfile creates an extra profile in the sandbox HOME that
// authenticates as a service account from a credentials file (the output of
// "nebius iam auth-public-key generate"), as the Nebius docs show:
//
//	nebius profile create <name> --endpoint … --service-account-file <file> --parent-id …
//
// Select it per command with "--profile <name>".
func (c *CLI) AddServiceAccountProfile(tb testing.TB, name, addr, credentialsFile, parentID string) {
	tb.Helper()
	args := []string{"profile", "create", name, "--endpoint", Endpoint(tb, addr), "--service-account-file", credentialsFile}
	if parentID != "" {
		args = append(args, "--parent-id", parentID)
	}
	// Creating a service account profile exchanges a token right away, so the
	// plaintext --insecure flag is needed here too.
	if res, err := c.exec(tb.Context(), nil, withGlobalFlags(args)); err != nil {
		tb.Fatalf("nebiuscli: %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, res.Stdout, res.Stderr)
	}
	// "profile create" activates the new profile; keep the default one active
	// so plain commands keep running as the profile New created.
	if res, err := c.exec(tb.Context(), nil, []string{"profile", "activate", ProfileName}); err != nil {
		tb.Fatalf("nebiuscli: profile activate %s: %v\nstdout:\n%s\nstderr:\n%s", ProfileName, err, res.Stdout, res.Stderr)
	}
}

// Endpoint converts a "host:port" listen address into the CLI endpoint
// "localhost:port" (see the package comment for why).
func Endpoint(tb testing.TB, addr string) string {
	tb.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		tb.Fatalf("nebiuscli: bad mocker address %q: %v", addr, err)
	}
	return net.JoinHostPort("localhost", port)
}

// Home is the sandbox HOME (it contains .nebius/config.yaml).
func (c *CLI) Home() string { return c.home }

// Bin is the path of the nebius binary.
func (c *CLI) Bin() string { return c.bin }

// Env is the environment commands run with: the minimal inherited variables,
// HOME set to the sandbox, and no NEBIUS_* overrides from the developer.
func (c *CLI) Env() []string { return append(baseEnv(), "HOME="+c.home) }

// Run executes "nebius <args> --insecure --no-check-update --no-browser" and
// fails with an error carrying stderr when the command exits non-zero.
func (c *CLI) Run(ctx context.Context, args ...string) (Result, error) {
	return c.exec(ctx, nil, withGlobalFlags(args))
}

// JSON runs the command with "--format json" appended and decodes stdout into
// v, failing the test on any error.
func (c *CLI) JSON(tb testing.TB, v any, args ...string) {
	tb.Helper()
	res, err := c.Run(tb.Context(), append(append([]string{}, args...), "--format", "json")...)
	if err != nil {
		tb.Fatalf("nebiuscli: %v", err)
	}
	if err := json.Unmarshal([]byte(res.Stdout), v); err != nil {
		tb.Fatalf("nebiuscli: decode JSON output of %s: %v\n%s", strings.Join(args, " "), err, res.Stdout)
	}
}

// RunStdin is Run with stdin.
func (c *CLI) RunStdin(ctx context.Context, stdin io.Reader, args ...string) (Result, error) {
	return c.exec(ctx, stdin, withGlobalFlags(args))
}

// Command returns an unstarted *exec.Cmd for "nebius <args>" plus the global
// flags, for callers that pipe its output (e.g. into docker login).
func (c *CLI) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bin, withGlobalFlags(args)...) //nolint:gosec // test helper runs the pinned CLI
	cmd.Env = c.Env()
	return cmd
}

func (c *CLI) exec(ctx context.Context, stdin io.Reader, args []string) (Result, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...) //nolint:gosec // test helper runs the pinned CLI
	cmd.Env = c.Env()
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return res, fmt.Errorf("nebius %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(res.Stderr))
	}
	return res, nil
}

func withGlobalFlags(args []string) []string {
	return append(append([]string{}, args...), "--insecure", "--no-check-update", "--no-browser")
}

// baseEnv passes through only what child tools need to run (PATH for bash,
// curl and docker; proxy settings for the installer download).
func baseEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"} {
		if v, ok := os.LookupEnv(k); ok { //nolint:forbidigo // child processes must inherit the developer's PATH and proxies
			env = append(env, k+"="+v)
		}
	}
	return env
}
