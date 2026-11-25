//go:build e2e_images

// Package compute runs NebiusMocker's Compute images against a real RustFS
// object store (Docker) with the official Go SDK and the official nebius CLI.
//
// Run with: make e2e-images
package compute

import (
	"os"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// One RustFS container for the whole test binary, removed when it exits.
func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

// startOnRustFS runs IAM + Compute with their blobs in RustFS, enforcing
// authorization.
func startOnRustFS(t *testing.T) *imagescenario.Env {
	t.Helper()
	srv := rustfsfixture.Start(t)
	opts := []testkit.Option{
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(100 * time.Millisecond),
	}
	return imagescenario.Start(t, srv.Client(t), opts...)
}
