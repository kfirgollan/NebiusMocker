//go:build e2e_images

package compute

import (
	"fmt"
	"os"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
	"github.com/kfirgollan/NebiusMocker/test/internal/s3upload"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// One RustFS container and one private plugin cache per test binary.
func TestMain(m *testing.M) {
	code := rustfsfixture.Main(m)
	tfcache.Cleanup()
	os.Exit(code)
}

// TestCustomImageDataSources imports an image the way users do (bucket via
// BucketService, upload with the AWS SDK through the S3 front, Create from
// source_storage), reads it through the data source by id and by name,
// changes it through the SDK and checks Terraform sees the change with no drift.
// A principal with no roles cannot read it.
func TestCustomImageDataSources(t *testing.T) {
	srv := rustfsfixture.Start(t)
	st := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	env := imagescenario.Start(t, srv.Client(t), []mocker.Module{st},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(200*time.Millisecond),
		testkit.WithTokens(map[string]string{outsiderToken: outsiderID}),
	)
	env.Upload = s3upload.New(t, env.Inst, st.S3Addr()).Upload
	ctx := t.Context()
	bucket := s3upload.Unique("tf-images-")
	env.Upload(t, bucket, "app.qcow2", domain.SyntheticQCOW2(12<<30, 16<<10, "tf"))
	custom := imagescenario.CreateFromStorage(t, env, "app-image", "app", "7", bucket, "app.qcow2")

	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	tf.writeConfig(t, fmt.Sprintf(`
data "nebius_compute_v1_image" "custom_by_id" { id = %q }
data "nebius_compute_v1_image" "custom_by_name" {
  parent_id = %q
  name      = "app-image"
}
output "custom_by_id"   { value = data.nebius_compute_v1_image.custom_by_id }
output "custom_by_name" { value = data.nebius_compute_v1_image.custom_by_name }
`, custom.GetMetadata().GetId(), env.Inst.ProjectID))
	tf.init(t)
	tf.apply(t)
	out := tf.outputs(t)
	assertImage(t, out["custom_by_id"], custom)
	assertImage(t, out["custom_by_name"], custom)
	if got := str(out["custom_by_id"], "source_storage", "bucket_name"); got != bucket {
		t.Errorf("source_storage.bucket_name = %q, want %q", got, bucket)
	}
	tf.expectNoChanges(t)

	op, err := env.Images.Update(ctx, &computepb.UpdateImageRequest{
		Metadata: &commonpb.ResourceMetadata{Id: custom.GetMetadata().GetId(), ParentId: env.Inst.ProjectID, Name: "app-image", Labels: map[string]string{"team": "ml", "stage": "prod"}},
		Spec:     &computepb.ImageSpec{Description: "promoted", Source: custom.GetSpec().GetSource()},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	updated, err := env.Images.Get(ctx, &computepb.GetImageRequest{Id: custom.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if !tf.planChanges(t) {
		t.Fatal("plan after an out-of-band image change shows no output change")
	}
	tf.applyRefresh(t)
	out = tf.outputs(t)
	assertImage(t, out["custom_by_id"], updated)
	assertImage(t, out["custom_by_name"], updated)
	if str(out["custom_by_id"], "description") != "promoted" || str(out["custom_by_id"], "labels", "stage") != "prod" {
		t.Fatalf("terraform did not pick up the update: %v", out["custom_by_id"])
	}
	tf.expectNoChanges(t)
	tf.destroy(t)
	imagescenario.AssertBlob(t, env.Store, updated) // data sources never touch the image

	outsider := newTerraform(t, env.Inst.Addr, outsiderToken)
	outsider.writeConfig(t, fmt.Sprintf(`data "nebius_compute_v1_image" "private" { id = %q }`, custom.GetMetadata().GetId()))
	outsider.init(t)
	if err := outsider.tf.Apply(ctx); err == nil {
		t.Fatal("a principal with no roles read a private image through Terraform")
	}
	outsider.out.Reset()
}
