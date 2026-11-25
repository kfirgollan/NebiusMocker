//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"os"
	"strings"
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

// The plugin cache is private to this test binary (see tfcache).
func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }

// TestPublicImageDataSources reads seeded public images (by id, by name, a
// deprecated one) with no Docker: nothing is uploaded, so the in-memory object
// store is only read. Custom images are covered by custom_images_test.go
// (make e2e-images), because creating them needs a real upload.
func TestPublicImageDataSources(t *testing.T) {
	env := imagescenario.Start(t, memory.New(clock.Real{}), nil,
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{outsiderToken: outsiderID}),
	)
	ctx := t.Context()
	// A family resolves to an image the way users do it: the newest version's ID
	// (GetLatestByFamily) or its name in the public project.
	latest, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "ubuntu24.04-cuda12"})
	if err != nil {
		t.Fatal(err)
	}
	deprecated, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "ubuntu22.04-driverless"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`
data "nebius_compute_v1_image" "public_by_id" { id = %q }
data "nebius_compute_v1_image" "public_by_name" {
  parent_id = %q
  name      = %q
}
data "nebius_compute_v1_image" "deprecated" { id = %q }
output "public_by_id"   { value = data.nebius_compute_v1_image.public_by_id }
output "public_by_name" { value = data.nebius_compute_v1_image.public_by_name }
output "deprecated"     { value = data.nebius_compute_v1_image.deprecated }
`, latest.GetMetadata().GetId(), domain.PublicProjectID("e00"), latest.GetMetadata().GetName(), deprecated.GetMetadata().GetId())

	// The admin and a principal with no roles at all read the same public images.
	for _, token := range []string{env.Inst.Token, outsiderToken} {
		tf := newTerraform(t, env.Inst.Addr, token)
		tf.writeConfig(t, cfg)
		tf.init(t)
		tf.apply(t)
		out := tf.outputs(t)
		assertImage(t, out["public_by_id"], latest)
		assertImage(t, out["public_by_name"], latest)
		assertImage(t, out["deprecated"], deprecated)
		if got := str(out["deprecated"], "status", "image_family_deprecation", "message"); !strings.Contains(got, "deprecated") {
			t.Errorf("deprecated image: image_family_deprecation.message = %q", got)
		}
		tf.expectNoChanges(t)
		tf.destroy(t)
	}
}
