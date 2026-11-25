//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"strings"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// TestImageFromTerraformDisk: Terraform creates a nebius_compute_v1_disk, an
// image is made from it with the Go SDK (the provider has no image resource),
// and the image data source reads it back with its disk source and size.
func TestImageFromTerraformDisk(t *testing.T) {
	// Long enough for one terraform apply to read the disk while the image is CREATING.
	env := imagescenario.Start(t, memory.New(clock.Real{}), nil, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithOperationDelay(8*time.Second))
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	disk := fmt.Sprintf(`
resource "nebius_compute_v1_disk" "data" {
  parent_id      = %q
  name           = "tf-data-disk"
  type           = "NETWORK_SSD"
  size_gibibytes = 32
}
`, env.Inst.ProjectID)
	tf.writeConfig(t, disk+`output "disk_id" { value = nebius_compute_v1_disk.data.id }
`)
	tf.init(t)
	tf.apply(t)
	raw, err := tf.tf.Output(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	diskID := string(raw["disk_id"].Value[1 : len(raw["disk_id"].Value)-1]) // JSON string

	op, err := env.Images.Create(t.Context(), &computepb.CreateImageRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.Inst.ProjectID, Name: "tf-disk-image"},
		Spec:     &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskId{SourceDiskId: diskID}},
	})
	if err != nil {
		t.Fatalf("Create image from %s: %v", diskID, err)
	}
	// While the image is being created the disk is locked (disk.proto), and
	// Terraform's view of the disk shows it.
	lockView := disk + fmt.Sprintf(`
data "nebius_compute_v1_disk" "seen" { id = %q }
output "seen" { value = data.nebius_compute_v1_disk.seen }
`, diskID)
	tf.writeConfig(t, lockView)
	tf.applyRefresh(t)
	if op.Done() {
		t.Fatal("the image finished before Terraform read the disk; raise the operation delay")
	}
	if got := str(tf.outputs(t)["seen"], "status", "lock_state", "images"); !strings.Contains(got, op.ResourceID()) {
		t.Fatalf("disk lock_state.images while the image is CREATING = %q, want it to contain %s", got, op.ResourceID())
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	tf.applyRefresh(t)
	if got := str(tf.outputs(t)["seen"], "status", "lock_state", "images"); strings.Contains(got, op.ResourceID()) {
		t.Fatalf("disk lock_state.images after the image is READY = %q, want the lock released", got)
	}
	img, err := env.Images.Get(t.Context(), &computepb.GetImageRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatal(err)
	}

	tf.writeConfig(t, disk+fmt.Sprintf(`
data "nebius_compute_v1_image" "from_disk" {
  parent_id = %q
  name      = "tf-disk-image"
}
output "from_disk" { value = data.nebius_compute_v1_image.from_disk }
`, env.Inst.ProjectID))
	tf.apply(t)
	out := tf.outputs(t)
	assertImage(t, out["from_disk"], img)
	if got := str(out["from_disk"], "source_disk_id"); got != diskID {
		t.Fatalf("source_disk_id = %q, want %q", got, diskID)
	}
	if got := str(out["from_disk"], "status", "min_disk_size_bytes"); got != fmt.Sprint(int64(32<<30)) {
		t.Fatalf("min_disk_size_bytes = %s, want %d", got, int64(32<<30))
	}
	tf.expectNoChanges(t)

	// The image references the disk but does not own it; delete it first, then
	// Terraform destroys the disk.
	dop, err := env.Images.Delete(t.Context(), &computepb.DeleteImageRequest{Id: img.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dop.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	tf.writeConfig(t, disk)
	tf.destroy(t)
}
