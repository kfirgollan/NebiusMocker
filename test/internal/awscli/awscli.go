// Package awscli runs the AWS CLI (the S3 client the Nebius Object Storage
// docs use) in a sandbox: its config and credentials files live in a per-test
// directory, so the developer's ~/.aws is never read or written.
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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CLI is a sandboxed aws binary.
type CLI struct {
	bin string
	dir string
}

// New returns a sandboxed AWS CLI; it skips the test when aws is not on PATH.
func New(tb testing.TB) *CLI {
	tb.Helper()
	bin, err := exec.LookPath("aws")
	if err != nil {
		tb.Skip("aws CLI not found on PATH")
	}
	return &CLI{bin: bin, dir: tb.TempDir()}
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
	cmd := exec.CommandContext(ctx, c.bin, args...) //nolint:gosec // test helper: runs the aws binary from PATH with test-chosen args
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
