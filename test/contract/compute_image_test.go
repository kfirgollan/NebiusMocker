package contract

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

func startImages(t *testing.T, opts ...testkit.Option) *imagescenario.Env {
	t.Helper()
	return imagescenario.Start(t, memory.New(clock.Real{}), nil, opts...)
}

func TestImagePublicCatalog(t *testing.T) {
	t.Parallel()
	imagescenario.PublicCatalog(t, startImages(t))
}

func TestImageCustomLifecycle(t *testing.T) {
	t.Parallel()
	imagescenario.CustomLifecycle(t, startImages(t))
}

func TestImageErrors(t *testing.T) {
	t.Parallel()
	imagescenario.Errors(t, startImages(t))
}

// TestImageCatalogOverride replaces the catalog through
// NEBIUS_MOCKER_COMPUTE_IMAGES_FILE and checks GetLatestByFamily against
// per-version deprecation and region limits.
func TestImageCatalogOverride(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "images.yaml")
	catalog := `
families:
  - name: acme-os
    human_readable: Acme OS
    min_disk_size_gibibytes: 8
    cpu_architecture: ARM64
    versions:
      - { version: "1" }
      - { version: "2" }
      - { version: "3", deprecation: { at: 2020-01-01T00:00:00Z, message: broken build } }
  - name: acme-gpu
    regions: [us-central1]
    versions:
      - { version: "1" }
      - { version: "2", deprecation: { at: 2999-01-01T00:00:00Z, message: scheduled } }
`
	if err := os.WriteFile(file, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	env := startImages(t, testkit.WithConfig(func(c *config.Config) { c.ComputeImagesFile = file }))
	ctx := t.Context()

	latest, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "acme-os"})
	if err != nil {
		t.Fatal(err)
	}
	if latest.GetSpec().GetVersion() != "2" || latest.GetSpec().GetCpuArchitecture() != computepb.ImageSpec_ARM64 ||
		latest.GetStatus().GetMinDiskSizeBytes() != 8<<30 || latest.GetStatus().GetImageFamilyDeprecation() != nil {
		t.Fatalf("latest acme-os = %v, want version 2 (3 is deprecated)", latest)
	}
	// A deprecation in the future does not hide the version yet.
	gpu, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "acme-gpu", ParentId: domain.PublicProjectID("u00")})
	if err != nil || gpu.GetSpec().GetVersion() != "2" || gpu.GetStatus().GetImageFamilyDeprecation().GetMessage() != "scheduled" {
		t.Fatalf("latest acme-gpu = %v, %v", gpu, err)
	}
	_, err = env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "acme-gpu"})
	imagescenario.AssertCode(t, err, codes.NotFound) // not in eu-north1
	if n := len(imagescenario.ListAll(t, env, "", "eu-north1", 0)); n != 3 {
		t.Fatalf("ListPublic(eu-north1) = %d images, want 3", n)
	}
	if n := len(imagescenario.ListAll(t, env, "", "us-central1", 0)); n != 5 {
		t.Fatalf("ListPublic(us-central1) = %d images, want 5", n)
	}
	imagescenario.AssertBlob(t, env.Store, latest)
}

// TestImageAuthz checks image permissions under enforcement: public images
// are readable by any authenticated principal; custom images follow the
// project's roles.
func TestImageAuthz(t *testing.T) {
	t.Parallel()
	env := startImages(t,
		testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired(),
		testkit.WithTokens(map[string]string{outsiderToken: outsiderID}),
	)
	ctx := t.Context()
	env.Upload(t, "authz", "a.raw", []byte("payload"))
	custom := imagescenario.CreateFromStorage(t, env, "private", "", "", "authz", "a.raw")
	customID := custom.GetMetadata().GetId()
	as := func(token string) computesvc.ImageService {
		return computesvc.NewImageService(env.Inst.SDKAs(t, token))
	}
	viewer, editor, outsider := as(config.DefaultViewerToken), as(config.DefaultEditorToken), as(outsiderToken)
	createReq := &computepb.CreateImageRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.Inst.ProjectID, Name: "by-role"},
		Spec:     &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceStorage_{SourceStorage: &computepb.ImageSpec_SourceStorage{BucketName: "authz", ObjectName: "a.raw"}}},
	}

	// Everyone authenticated reads the public catalog.
	for name, svc := range map[string]computesvc.ImageService{"viewer": viewer, "outsider": outsider} {
		pub, err := svc.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "ubuntu24.04-driverless"})
		if err != nil {
			t.Fatalf("%s GetLatestByFamily: %v", name, err)
		}
		if _, err := svc.Get(ctx, &computepb.GetImageRequest{Id: pub.GetMetadata().GetId()}); err != nil {
			t.Fatalf("%s Get(public): %v", name, err)
		}
		if _, err := svc.ListPublic(ctx, &computepb.ListPublicRequest{Region: "eu-west1"}); err != nil {
			t.Fatalf("%s ListPublic: %v", name, err)
		}
		if _, err := svc.List(ctx, &computepb.ListImagesRequest{ParentId: pub.GetMetadata().GetParentId()}); err != nil {
			t.Fatalf("%s List(public project): %v", name, err)
		}
	}
	// Viewer reads custom images but cannot create.
	if _, err := viewer.Get(ctx, &computepb.GetImageRequest{Id: customID}); err != nil {
		t.Fatalf("viewer Get(custom): %v", err)
	}
	if _, err := viewer.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.Inst.ProjectID, Name: "private"}); err != nil {
		t.Fatalf("viewer GetByName(custom): %v", err)
	}
	_, err := outsider.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.Inst.ProjectID, Name: "private"})
	imagescenario.AssertCode(t, err, codes.NotFound)
	_, err = viewer.Create(ctx, createReq)
	imagescenario.AssertCode(t, err, codes.PermissionDenied)
	// Editor creates.
	op, err := editor.Create(ctx, createReq)
	if err != nil {
		t.Fatalf("editor Create: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("editor Create wait: %v", err)
	}
	// The outsider sees nothing of the project.
	_, err = outsider.Get(ctx, &computepb.GetImageRequest{Id: customID})
	imagescenario.AssertCode(t, err, codes.NotFound)
	_, err = outsider.List(ctx, &computepb.ListImagesRequest{ParentId: env.Inst.ProjectID})
	imagescenario.AssertCode(t, err, codes.PermissionDenied)
	_, err = outsider.ListOperationsByParent(ctx, &computepb.ListOperationsByParentRequest{ParentId: env.Inst.ProjectID})
	imagescenario.AssertCode(t, err, codes.PermissionDenied)
	_, err = outsider.Delete(ctx, &computepb.DeleteImageRequest{Id: customID})
	imagescenario.AssertCode(t, err, codes.PermissionDenied)
	// Anonymous callers are rejected even for the public catalog.
	_, err = as("").ListPublic(ctx, &computepb.ListPublicRequest{Region: "eu-north1"})
	imagescenario.AssertCode(t, err, codes.Unauthenticated)
}
