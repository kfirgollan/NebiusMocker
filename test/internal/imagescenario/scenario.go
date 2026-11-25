// Package imagescenario holds the Go SDK scenarios for nebius.compute.v1.ImageService.
// They run twice: in test/contract against the in-memory object store (make
// check, no Docker) and in test/compute against RustFS (make e2e-images), so
// the same assertions prove blobs really land in the object store.
package imagescenario

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/compute/backend/blobstore"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// Uploader puts an image file into an Object Storage bucket, creating the
// bucket on first use. End-to-end suites upload the way users do (S3 front,
// access key, AWS SDK); contract tests write to the in-memory store.
type Uploader func(t *testing.T, bucket, object string, data []byte)

// Env is a running mocker with IAM and Compute. Store is read (never
// written) to verify image payloads.
type Env struct {
	Inst   *testkit.Instance
	Images computesvc.ImageService
	Store  objstore.Store
	// Upload places source files; see Uploader.
	Upload Uploader
	// BucketPrefix is prepended to every scenario bucket name, so suites that
	// share one object store never reuse a bucket.
	BucketPrefix string
}

// Bucket is the bucket name the scenarios use for name.
func (e *Env) Bucket(name string) string { return e.BucketPrefix + name }

// Start runs IAM + Compute (plus extra modules) over store. Operations are
// delayed so the SDK's operation polling is exercised. Upload defaults to
// writing into store directly, which only Docker-free contract tests use.
func Start(t *testing.T, store objstore.Store, extra []mocker.Module, opts ...testkit.Option) *Env {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithObjStore(store), testkit.WithOperationDelay(30 * time.Millisecond)}, opts...)
	inst := testkit.StartT(t, append([]mocker.Module{iam.New(), compute.New()}, extra...), opts...)
	return &Env{
		Inst: inst, Images: computesvc.NewImageService(inst.SDK(t)), Store: store,
		Upload: func(t *testing.T, bucket, object string, data []byte) {
			t.Helper()
			PutSource(t, store, bucket, object, data)
		},
	}
}

// AssertCode fails unless err has the gRPC code want.
func AssertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v, want %v (err: %v)", got, want, err)
	}
}

// AssertBlob checks that the image's payload is a real object in the store
// whose size and sha256 match the image status and the object metadata, and
// returns the payload.
func AssertBlob(t *testing.T, st objstore.Store, img *computepb.Image) []byte {
	t.Helper()
	id := img.GetMetadata().GetId()
	r, info, err := st.Get(t.Context(), blobstore.Bucket, blobstore.Key(id))
	if err != nil {
		t.Fatalf("image %s: payload object: %v", id, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("image %s: read payload: %v", id, err)
	}
	sum := sha256.Sum256(data)
	switch {
	case int64(len(data)) != img.GetStatus().GetStorageSizeBytes() || info.Size != int64(len(data)):
		t.Fatalf("image %s: payload %d bytes (object says %d), status.storage_size_bytes %d", id, len(data), info.Size, img.GetStatus().GetStorageSizeBytes())
	case info.Metadata[domain.MetaSHA256] != hex.EncodeToString(sum[:]):
		t.Fatalf("image %s: object sha256 metadata %q, content sha256 %x", id, info.Metadata[domain.MetaSHA256], sum)
	case info.Metadata[domain.MetaImageID] != id:
		t.Fatalf("image %s: object image-id metadata %q", id, info.Metadata[domain.MetaImageID])
	}
	return data
}

// ListAll pages through List (or ListPublic when region is set) with pageSize.
func ListAll(t *testing.T, env *Env, parentID, region string, pageSize int64) []*computepb.Image {
	t.Helper()
	var out []*computepb.Image
	token := ""
	for range 1000 {
		var resp *computepb.ListImagesResponse
		var err error
		if region != "" {
			resp, err = env.Images.ListPublic(t.Context(), &computepb.ListPublicRequest{Region: region, PageSize: pageSize, PageToken: token})
		} else {
			resp, err = env.Images.List(t.Context(), &computepb.ListImagesRequest{ParentId: parentID, PageSize: pageSize, PageToken: token})
		}
		if err != nil {
			t.Fatalf("list (parent %q, region %q): %v", parentID, region, err)
		}
		if pageSize > 0 && int64(len(resp.GetItems())) > pageSize {
			t.Fatalf("page has %d items, page_size %d", len(resp.GetItems()), pageSize)
		}
		out = append(out, resp.GetItems()...)
		if token = resp.GetNextPageToken(); token == "" {
			return out
		}
	}
	t.Fatal("list did not terminate")
	return nil
}

// PublicCatalog checks ListPublic in every configured region against the
// built-in catalog, the payloads in the store, and GetLatestByFamily.
func PublicCatalog(t *testing.T, env *Env) {
	t.Helper()
	cat, err := domain.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for i := range cat.Families {
		want += len(cat.Families[i].Versions)
	}
	for _, region := range config.DefaultRegions() {
		items := ListAll(t, env, "", region.Name, 3)
		if len(items) != want {
			t.Fatalf("ListPublic(%s) = %d images, want %d", region.Name, len(items), want)
		}
		seen := map[string]bool{}
		for _, img := range items {
			id := img.GetMetadata().GetId()
			if seen[id] {
				t.Fatalf("ListPublic(%s): duplicate %s across pages", region.Name, id)
			}
			seen[id] = true
			if img.GetMetadata().GetParentId() != domain.PublicProjectID(region.RoutingCode) ||
				img.GetStatus().GetState() != computepb.ImageStatus_READY ||
				img.GetStatus().GetMinDiskSizeBytes() < 1<<30 ||
				img.GetSpec().GetCpuArchitecture() != computepb.ImageSpec_AMD64 ||
				img.GetMetadata().GetLabels()["os_name"] != "Ubuntu" {
				t.Fatalf("ListPublic(%s): unexpected image %v", region.Name, img)
			}
		}
	}
	items := ListAll(t, env, "", "eu-north1", 0)
	for _, img := range items[:3] {
		AssertBlob(t, env.Store, img)
	}
	_, err = env.Images.ListPublic(t.Context(), &computepb.ListPublicRequest{Region: "mars-north1"})
	AssertCode(t, err, codes.InvalidArgument)

	// GetByName and List on the public project work too.
	pub := domain.PublicProjectID("e00")
	byName, err := env.Images.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: pub, Name: "ubuntu24.04-driverless-20251015"})
	if err != nil || byName.GetSpec().GetImageFamilyHumanReadable() != "Ubuntu 24.04 LTS" {
		t.Fatalf("GetByName(public) = %v, %v", byName, err)
	}
	if got := ListAll(t, env, pub, "", 5); len(got) != want {
		t.Fatalf("List(%s) = %d, want %d", pub, len(got), want)
	}
	latestByFamily(t, env)
}

func latestByFamily(t *testing.T, env *Env) {
	t.Helper()
	ctx := t.Context()
	latest, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "ubuntu24.04-driverless"})
	if err != nil {
		t.Fatalf("GetLatestByFamily: %v", err)
	}
	if latest.GetSpec().GetVersion() != "20251015" || latest.GetMetadata().GetParentId() != domain.PublicProjectID("e00") {
		t.Fatalf("GetLatestByFamily(default parent) = %s in %s", latest.GetSpec().GetVersion(), latest.GetMetadata().GetParentId())
	}
	us, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "ubuntu24.04-cuda13.0", ParentId: domain.PublicProjectID("u00")})
	if err != nil || us.GetSpec().GetVersion() != "20251112" || us.GetMetadata().GetParentId() != domain.PublicProjectID("u00") {
		t.Fatalf("GetLatestByFamily(us-central1) = %v, %v", us, err)
	}
	// A deprecated family still resolves, and says it is deprecated.
	old, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "ubuntu22.04-cuda12"})
	if err != nil || old.GetStatus().GetImageFamilyDeprecation().GetDeprecatedAt() == nil || old.GetSpec().GetVersion() != "20250415" {
		t.Fatalf("GetLatestByFamily(deprecated family) = %v, %v", old, err)
	}
	_, err = env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "windows11"})
	AssertCode(t, err, codes.NotFound)
}

// PutSource writes a source file straight into the in-memory object store.
// It is a shortcut for Docker-free contract tests only; end-to-end suites
// must upload through the S3 front (see test/internal/s3upload).
func PutSource(t *testing.T, st objstore.Store, bucket, object string, data []byte) {
	t.Helper()
	if err := st.EnsureBucket(t.Context(), bucket); err != nil {
		t.Fatalf("ensure bucket %s: %v", bucket, err)
	}
	if _, err := st.Put(t.Context(), bucket, object, bytes.NewReader(data), int64(len(data)), objstore.PutOptions{}); err != nil {
		t.Fatalf("put %s/%s: %v", bucket, object, err)
	}
}

// CreateFromStorage imports bucket/object as an image and waits for it.
func CreateFromStorage(t *testing.T, env *Env, name, family, version, bucket, object string) *computepb.Image {
	t.Helper()
	op, err := env.Images.Create(t.Context(), &computepb.CreateImageRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.Inst.ProjectID, Name: name, Labels: map[string]string{"team": "ml"}},
		Spec: &computepb.ImageSpec{
			Description: "custom " + name, ImageFamily: family, Version: version,
			Source: &computepb.ImageSpec_SourceStorage_{SourceStorage: &computepb.ImageSpec_SourceStorage{BucketName: bucket, ObjectName: object}},
		},
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create(%s) wait: %v", name, err)
	}
	img, err := env.Images.Get(t.Context(), &computepb.GetImageRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatalf("Get(%s): %v", op.ResourceID(), err)
	}
	return img
}

// CustomLifecycle imports images from a bucket, then exercises Get,
// GetByName, List paging, GetLatestByFamily, Update, Delete and
// ListOperationsByParent, checking the payloads in the store throughout.
func CustomLifecycle(t *testing.T, env *Env) {
	t.Helper()
	ctx := t.Context()
	bucket := env.Bucket("my-images")
	virtual := int64(10<<30 + 1) // rounds up to an 11 GiB minimum disk
	src := domain.SyntheticQCOW2(virtual, 70<<10, "custom-v1")
	env.Upload(t, bucket, "build/v1.qcow2", src)
	env.Upload(t, bucket, "build/v2.raw", bytes.Repeat([]byte{7}, 3<<20))

	v1 := CreateFromStorage(t, env, "app-v1", "app", "1", bucket, "build/v1.qcow2")
	if v1.GetStatus().GetState() != computepb.ImageStatus_READY || v1.GetStatus().GetMinDiskSizeBytes() != 11<<30 ||
		v1.GetSpec().GetCpuArchitecture() != computepb.ImageSpec_AMD64 || v1.GetMetadata().GetResourceVersion() != 1 {
		t.Fatalf("imported image = %v", v1)
	}
	if got := AssertBlob(t, env.Store, v1); !bytes.Equal(got, src) {
		t.Fatal("stored payload differs from the source object")
	}
	v2 := CreateFromStorage(t, env, "app-v2", "app", "2", bucket, "build/v2.raw")
	if v2.GetStatus().GetMinDiskSizeBytes() != 1<<30 || v2.GetStatus().GetStorageSizeBytes() != 3<<20 {
		t.Fatalf("raw image status = %v", v2.GetStatus())
	}
	AssertBlob(t, env.Store, v2)
	v3 := CreateFromStorage(t, env, "other", "", "", bucket, "build/v2.raw")

	byName, err := env.Images.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.Inst.ProjectID, Name: "app-v1"})
	if err != nil || byName.GetMetadata().GetId() != v1.GetMetadata().GetId() {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	listed := ListAll(t, env, env.Inst.ProjectID, "", 2)
	ids := func(imgs []*computepb.Image) []string {
		var out []string
		for _, i := range imgs {
			out = append(out, i.GetMetadata().GetId())
		}
		return out
	}
	if want := ids([]*computepb.Image{v1, v2, v3}); !slices.Equal(ids(listed), want) {
		t.Fatalf("List = %v, want %v", ids(listed), want)
	}
	latest, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "app", ParentId: env.Inst.ProjectID})
	if err != nil || latest.GetMetadata().GetId() != v2.GetMetadata().GetId() {
		t.Fatalf("GetLatestByFamily(app) = %v, %v", latest, err)
	}

	updated := update(t, env, v1)
	deleteImage(t, env, v3)
	operationsByParent(t, env, []string{v1.GetMetadata().GetId(), v2.GetMetadata().GetId(), v3.GetMetadata().GetId()}, updated)
}

func update(t *testing.T, env *Env, img *computepb.Image) *computepb.Image {
	t.Helper()
	ctx := t.Context()
	id := img.GetMetadata().GetId()
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	// The spec's source oneof is required by the API's validation even on
	// Update, so clients resend it unchanged.
	source := img.GetSpec().GetSource()
	op, err := env.Images.Update(maskCtx, &computepb.UpdateImageRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: env.Inst.ProjectID, Name: "app-v1-renamed", Labels: map[string]string{"tier": "gold"}},
		Spec:     &computepb.ImageSpec{Description: "renamed", RecommendedPlatforms: []string{"gpu-h100-sxm"}, Source: source},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Update wait: %v", err)
	}
	got, err := env.Images.Get(ctx, &computepb.GetImageRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	md := got.GetMetadata()
	if md.GetName() != "app-v1-renamed" || md.GetLabels()["tier"] != "gold" || md.GetLabels()["team"] != "" ||
		got.GetSpec().GetDescription() != "renamed" || got.GetSpec().GetImageFamily() != "app" || got.GetSpec().GetVersion() != "1" ||
		got.GetSpec().GetSourceStorage().GetObjectName() != "build/v1.qcow2" || md.GetResourceVersion() != 2 ||
		got.GetStatus().GetState() != computepb.ImageStatus_READY {
		t.Fatalf("after update: %v", got)
	}
	// Immutable fields cannot change.
	_, err = env.Images.Update(ctx, &computepb.UpdateImageRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: env.Inst.ProjectID},
		Spec:     &computepb.ImageSpec{ImageFamily: "other-family", Source: source},
	})
	AssertCode(t, err, codes.InvalidArgument)
	_, err = env.Images.Update(ctx, &computepb.UpdateImageRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: env.Inst.ProjectID},
		Spec: &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceStorage_{
			SourceStorage: &computepb.ImageSpec_SourceStorage{BucketName: env.Bucket("my-images"), ObjectName: "build/v2.raw"},
		}},
	})
	AssertCode(t, err, codes.InvalidArgument)
	_, err = env.Images.Update(ctx, &computepb.UpdateImageRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: env.Inst.ProjectID, ResourceVersion: 1},
		Spec:     &computepb.ImageSpec{Description: "stale", Source: source},
	})
	AssertCode(t, err, codes.Aborted)
	return got
}

func deleteImage(t *testing.T, env *Env, img *computepb.Image) {
	t.Helper()
	ctx := t.Context()
	id := img.GetMetadata().GetId()
	op, err := env.Images.Delete(ctx, &computepb.DeleteImageRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Delete wait: %v", err)
	}
	_, err = env.Images.Get(ctx, &computepb.GetImageRequest{Id: id})
	AssertCode(t, err, codes.NotFound)
	if _, err := env.Store.Stat(ctx, blobstore.Bucket, blobstore.Key(id)); err == nil {
		t.Fatalf("payload of deleted image %s is still in the store", id)
	}
}

func operationsByParent(t *testing.T, env *Env, created []string, updated *computepb.Image) {
	t.Helper()
	var all []*commonpb.Operation
	token := ""
	for {
		resp, err := env.Images.ListOperationsByParent(t.Context(), &computepb.ListOperationsByParentRequest{
			ParentId: env.Inst.ProjectID, PageSize: 2, PageToken: token,
		})
		if err != nil {
			t.Fatalf("ListOperationsByParent: %v", err)
		}
		all = append(all, resp.GetOperations()...)
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	// 3 creates, 1 update, 1 delete (the failed updates created no operation), newest first.
	descs := make([]string, 0, len(all))
	for _, op := range all {
		descs = append(descs, op.GetDescription()+" "+op.GetResourceId())
		if op.GetStatus() == nil || op.GetStatus().GetCode() != 0 {
			t.Fatalf("operation %s not finished successfully: %v", op.GetId(), op.GetStatus())
		}
	}
	want := []string{
		"Delete image " + created[2], "Update image " + updated.GetMetadata().GetId(),
		"Create image " + created[2], "Create image " + created[1], "Create image " + created[0],
	}
	if !slices.Equal(descs, want) {
		t.Fatalf("ListOperationsByParent = %v, want %v", descs, want)
	}
}

// Errors checks the error kinds of invalid requests.
func Errors(t *testing.T, env *Env) {
	t.Helper()
	ctx := t.Context()
	errBucket := env.Bucket("err-bucket")
	env.Upload(t, errBucket, "img.raw", []byte("0123456789"))
	create := func(md *commonpb.ResourceMetadata, spec *computepb.ImageSpec) error {
		op, err := env.Images.Create(ctx, &computepb.CreateImageRequest{Metadata: md, Spec: spec})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return err
	}
	storage := func(bucket, object string) *computepb.ImageSpec {
		return &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceStorage_{SourceStorage: &computepb.ImageSpec_SourceStorage{BucketName: bucket, ObjectName: object}}}
	}
	project := env.Inst.ProjectID
	AssertCode(t, create(&commonpb.ResourceMetadata{ParentId: project, Name: "a"}, storage(errBucket, "missing.raw")), codes.InvalidArgument)
	AssertCode(t, create(&commonpb.ResourceMetadata{ParentId: project, Name: "a"}, storage(env.Bucket("no-such-bucket"), "img.raw")), codes.InvalidArgument)
	AssertCode(t, create(&commonpb.ResourceMetadata{ParentId: project, Name: "a"},
		&computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskId{SourceDiskId: "computedisk-e00mock0000000000001"}}), codes.NotFound)
	AssertCode(t, create(&commonpb.ResourceMetadata{ParentId: "project-e00mock00000000nothere", Name: "a"}, storage(errBucket, "img.raw")), codes.NotFound)
	AssertCode(t, create(&commonpb.ResourceMetadata{ParentId: domain.PublicProjectID("e00"), Name: "a"}, storage(errBucket, "img.raw")), codes.PermissionDenied)

	spec := storage(errBucket, "img.raw")
	spec.ImageFamily, spec.Version = "fam", "1"
	if err := create(&commonpb.ResourceMetadata{ParentId: project, Name: "dup"}, spec); err != nil {
		t.Fatalf("create dup: %v", err)
	}
	AssertCode(t, create(&commonpb.ResourceMetadata{ParentId: project, Name: "dup"}, storage(errBucket, "img.raw")), codes.AlreadyExists)
	AssertCode(t, create(&commonpb.ResourceMetadata{ParentId: project, Name: "dup2"}, spec), codes.AlreadyExists)

	public, err := env.Images.GetLatestByFamily(ctx, &computepb.GetImageLatestByFamilyRequest{ImageFamily: "ubuntu24.04-driverless"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.Images.Delete(ctx, &computepb.DeleteImageRequest{Id: public.GetMetadata().GetId()})
	AssertCode(t, err, codes.PermissionDenied)
	_, err = env.Images.Update(ctx, &computepb.UpdateImageRequest{Metadata: &commonpb.ResourceMetadata{
		Id: public.GetMetadata().GetId(), ParentId: public.GetMetadata().GetParentId(), Name: "mine",
	}})
	AssertCode(t, err, codes.PermissionDenied)
	_, err = env.Images.Get(ctx, &computepb.GetImageRequest{Id: "computeimage-e00mock0000000000999"})
	AssertCode(t, err, codes.NotFound)
	_, err = env.Images.List(ctx, &computepb.ListImagesRequest{ParentId: project, PageToken: "garbage!"})
	AssertCode(t, err, codes.InvalidArgument)
	_, err = env.Images.List(ctx, &computepb.ListImagesRequest{ParentId: project, PageSize: 5000})
	AssertCode(t, err, codes.InvalidArgument)
}
