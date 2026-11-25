// Package awscli installs a pinned AWS CLI v2 into a sandbox and runs it (the
// S3 client the Nebius Object Storage docs use), for end-to-end tests.
//
// Nothing is installed system-wide and the developer's ~/.aws is never read or
// written:
//   - Binary downloads the official AWS CLI v2 release of Version from
//     awscli.amazonaws.com into the user cache dir
//     (<UserCacheDir>/nebius-mocker/aws-cli/<Version>), once per version. On
//     macOS the .pkg is unpacked with "pkgutil --expand-full" (no root, no
//     installer); on Linux the zip's ./aws/install runs with -i pointing into
//     a staging dir. The finished tree is renamed into place, so concurrent
//     test binaries never see a half-installed CLI. The binary is
//     <cache>/aws-cli/<Version>/aws-cli/aws (macOS) or
//     <cache>/aws-cli/<Version>/aws-cli/v2/<Version>/dist/aws (Linux).
//   - New gives each test its own HOME, AWS_CONFIG_FILE and
//     AWS_SHARED_CREDENTIALS_FILE in a temp dir and drops inherited AWS_*
//     settings, so tests never see or change real configuration.
//
// Configure follows the Nebius quick start:
//
//	aws configure set aws_access_key_id …
//	aws configure set aws_secret_access_key …
//	aws configure set region <region>
//	aws configure set endpoint_url <endpoint>
package awscli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Version is the pinned AWS CLI v2 release installed by Binary. It must be a
// release whose macOS .pkg is a universal binary (2.22.35 is x86_64 only).
const Version = "2.42.3"

const downloadBase = "https://awscli.amazonaws.com/"

// CLI is a sandboxed aws binary.
type CLI struct {
	bin string
	dir string
}

// New returns a sandboxed AWS CLI, installing the pinned version on first use.
func New(tb testing.TB) *CLI {
	tb.Helper()
	return &CLI{bin: Binary(tb), dir: tb.TempDir()}
}

// Binary returns the path of the pinned AWS CLI, installing it on first use.
// The test is skipped when the release cannot be downloaded (offline); any
// other install failure fails it.
func Binary(tb testing.TB) string {
	tb.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		tb.Fatalf("awscli: user cache dir: %v", err)
	}
	dir := filepath.Join(cache, "nebius-mocker", "aws-cli", Version)
	bin := binPath(dir)
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	if err := install(tb, dir); err != nil {
		tb.Fatalf("awscli: install %s: %v", Version, err)
	}
	probe := &CLI{bin: bin, dir: tb.TempDir()}
	if out, err := probe.Run(tb.Context(), "--version"); err != nil || !strings.Contains(out, "aws-cli/"+Version) {
		tb.Fatalf("awscli: %s --version = %q (%v), want aws-cli/%s", bin, out, err, Version)
	}
	return bin
}

func binPath(dir string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(dir, "aws-cli", "aws")
	}
	return filepath.Join(dir, "aws-cli", "v2", Version, "dist", "aws")
}

// install builds the CLI tree in a staging dir next to dir, then renames it
// into place.
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
	root := filepath.Join(stage, "root")       // becomes dir
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	url, err := downloadURL()
	if err != nil {
		return err
	}
	archive := filepath.Join(stage, "download")
	if err := download(tb.Context(), url, archive); err != nil {
		tb.Skipf("awscli: cannot download the AWS CLI (offline?): %v", err)
	}
	if runtime.GOOS == "darwin" {
		err = unpackDarwin(tb.Context(), archive, stage, root)
	} else {
		err = installLinux(tb.Context(), archive, stage, root)
	}
	if err != nil {
		return err
	}
	if err := os.Rename(root, dir); err != nil {
		if _, statErr := os.Stat(binPath(dir)); statErr == nil {
			return nil // another test binary won the race
		}
		return fmt.Errorf("move into cache: %w", err)
	}
	return nil
}

func downloadURL() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return downloadBase + "AWSCLIV2-" + Version + ".pkg", nil
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return downloadBase + "awscli-exe-linux-x86_64-" + Version + ".zip", nil
		case "arm64":
			return downloadBase + "awscli-exe-linux-aarch64-" + Version + ".zip", nil
		}
	}
	return "", fmt.Errorf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
}

// unpackDarwin expands the .pkg without root or the macOS installer and moves
// its payload (the aws-cli directory) into root.
func unpackDarwin(ctx context.Context, pkg, stage, root string) error {
	expanded := filepath.Join(stage, "x")
	if out, err := exec.CommandContext(ctx, "pkgutil", "--expand-full", pkg, expanded).CombinedOutput(); err != nil {
		return fmt.Errorf("pkgutil --expand-full: %w\n%s", err, out)
	}
	return os.Rename(filepath.Join(expanded, "aws-cli.pkg", "Payload", "aws-cli"), filepath.Join(root, "aws-cli"))
}

// installLinux unzips the release and runs its installer into root without
// sudo. The -b link dir is staging-only: the binary is used from the dist dir,
// which is relocatable (the installer's symlinks are not).
func installLinux(ctx context.Context, zip, stage, root string) error {
	src := filepath.Join(stage, "src")
	if out, err := exec.CommandContext(ctx, "unzip", "-q", zip, "-d", src).CombinedOutput(); err != nil {
		return fmt.Errorf("unzip: %w\n%s", err, out)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(src, "aws", "install"), //nolint:gosec // test helper: runs the installer just unpacked
		"-i", filepath.Join(root, "aws-cli"), "-b", filepath.Join(stage, "bin"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("aws/install: %w\n%s", err, out)
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
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort: read-only body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = io.Copy(f, resp.Body)
	return err
}

// Configure writes the profile the Nebius docs describe.
func (c *CLI) Configure(tb testing.TB, keyID, secret, region, endpoint string) {
	tb.Helper()
	for _, kv := range [][2]string{
		{"aws_access_key_id", keyID},
		{"aws_secret_access_key", secret},
		{"region", region},
		{"endpoint_url", endpoint},
	} {
		if _, err := c.Run(tb.Context(), "configure", "set", kv[0], kv[1]); err != nil {
			tb.Fatalf("aws configure set %s: %v", kv[0], err)
		}
	}
}

// Run runs aws with args and returns stdout; a non-zero exit is an error
// that includes stderr.
func (c *CLI) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...) //nolint:gosec // test helper: runs the pinned aws binary with test-chosen args
	cmd.Env = c.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("aws %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// env is a minimal environment: no inherited AWS_* settings.
func (c *CLI) env() []string {
	env := []string{
		"HOME=" + c.dir,
		"AWS_CONFIG_FILE=" + filepath.Join(c.dir, "config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(c.dir, "credentials"),
		"AWS_PAGER=",
		"AWS_EC2_METADATA_DISABLED=true",
	}
	for _, k := range []string{"PATH", "TMPDIR", "LANG"} {
		if v, ok := os.LookupEnv(k); ok { //nolint:forbidigo // the child process needs the developer's PATH
			env = append(env, k+"="+v)
		}
	}
	return env
}
