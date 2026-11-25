//go:build e2e_images

package compute

import (
	"testing"
	"time"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute/backend/blobstore"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// The Go SDK scenarios of test/contract, with every payload in RustFS.
// Tests share one RustFS server, so they run sequentially.

func TestSDKPublicCatalogOnRustFS(t *testing.T) {
	imagescenario.PublicCatalog(t, startOnRustFS(t).Env)
}

func TestSDKCustomImagesOnRustFS(t *testing.T) {
	imagescenario.CustomLifecycle(t, startOnRustFS(t).Env)
}

func TestSDKErrorsOnRustFS(t *testing.T) {
	imagescenario.Errors(t, startOnRustFS(t).Env)
}

func TestSDKImageFromDiskOnRustFS(t *testing.T) {
	imagescenario.FromDiskAndSnapshot(t, startOnRustFS(t, testkit.WithOperationDelay(700*time.Millisecond)).Env)
}

// TestCatalogSurvivesRestart: a second mocker on the same RustFS finds the
// seeded payloads already there (same deterministic IDs and checksums).
func TestCatalogSurvivesRestart(t *testing.T) {
	first := startOnRustFS(t)
	img, err := first.Images.GetLatestByFamily(t.Context(), &computepb.GetImageLatestByFamilyRequest{ImageFamily: "ubuntu24.04-cuda13.0"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := first.Store.Stat(t.Context(), blobstore.Bucket, blobstore.Key(img.GetMetadata().GetId()))
	if err != nil {
		t.Fatal(err)
	}
	second := startOnRustFS(t)
	again, err := second.Images.Get(t.Context(), &computepb.GetImageRequest{Id: img.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("public image ID not stable across restarts: %v", err)
	}
	after := imagescenario.AssertBlob(t, second.Store, again)
	if int64(len(after)) != before.Size || before.Metadata[domain.MetaSHA256] == "" {
		t.Fatalf("payload changed across restarts: %d vs %d bytes", len(after), before.Size)
	}
}
