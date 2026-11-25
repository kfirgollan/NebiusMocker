//go:build e2e_storage

package awscli_test

import (
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/test/internal/awscli"
)

// TestInstallAndConfigure installs the pinned AWS CLI into the cache and checks
// that the sandboxed profile is readable back through "aws configure get".
func TestInstallAndConfigure(t *testing.T) {
	cli := awscli.New(t)
	cli.Configure(t, "AKIADUMMY", "dummy-secret", "eu-north1", "http://localhost:1")

	out, err := cli.Run(t.Context(), "configure", "get", "region")
	if err != nil {
		t.Fatalf("configure get region: %v", err)
	}
	if got := strings.TrimSpace(out); got != "eu-north1" {
		t.Fatalf("region = %q, want eu-north1", got)
	}
}
