package domain

import (
	"bytes"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func storageSpec(bucket, object string) *computepb.ImageSpec {
	return &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceStorage_{
		SourceStorage: &computepb.ImageSpec_SourceStorage{BucketName: bucket, ObjectName: object},
	}}
}

func (f *fixture) create(t *testing.T, name string, spec *computepb.ImageSpec) (*computepb.Image, error) {
	t.Helper()
	op, err := f.svc.Create(t.Context(), CreateImageCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name}, Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	img, gerr := f.images.Get(t.Context(), op.ResourceID)
	if gerr != nil {
		t.Fatalf("created image %s missing: %v", op.ResourceID, gerr)
	}
	return img, op.Err
}

func TestCreateDerivesStatusFromPayload(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	payload := SyntheticQCOW2(5<<30, 10000, "x")
	f.blobs.sources["b/disk.qcow2"] = payload
	img, err := f.create(t, "img", storageSpec("b", "disk.qcow2"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	st := img.GetStatus()
	if st.GetState() != computepb.ImageStatus_READY || st.GetStorageSizeBytes() != int64(len(payload)) ||
		st.GetMinDiskSizeBytes() != 5<<30 || st.GetReconciling() || img.GetSpec().GetCpuArchitecture() != computepb.ImageSpec_AMD64 {
		t.Fatalf("status = %v, spec = %v", st, img.GetSpec())
	}
	stored := f.blobs.images[img.GetMetadata().GetId()]
	if !bytes.Equal(stored.data, payload) || stored.meta[MetaFormat] != "qcow2" || stored.meta[MetaSource] != "s3://b/disk.qcow2" ||
		stored.meta[MetaVirtualSize] != "5368709120" || len(stored.meta[MetaSHA256]) != 64 {
		t.Fatalf("stored blob meta = %v", stored.meta)
	}
}

func TestCreateFailsWhenSourceChangesMidImport(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.blobs.sources["b/o"] = []byte("first version")
	f.blobs.mutate = func(opens int, src map[string][]byte) {
		if opens == 2 { // between the hashing pass and the copy pass
			src["b/o"] = []byte("second versio")
		}
	}
	img, err := f.create(t, "img", storageSpec("b", "o"))
	if err == nil {
		t.Fatal("import of a changing source succeeded")
	}
	if img.GetStatus().GetState() != computepb.ImageStatus_ERROR || img.GetStatus().GetStateDescription() == "" {
		t.Fatalf("status = %v, want ERROR with a description", img.GetStatus())
	}
	if _, ok := f.blobs.images[img.GetMetadata().GetId()]; ok {
		t.Fatal("partial payload left in the store")
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		parent   string
		spec     *computepb.ImageSpec
		wantKind errs.Kind
	}{
		{name: "missing parent", spec: storageSpec("b", "o"), wantKind: errs.KindBadRequest},
		{name: "unknown project", parent: "project-e00mock00000000unknown", spec: storageSpec("b", "o"), wantKind: errs.KindNotFound},
		{name: "public project", parent: PublicProjectID("u00"), spec: storageSpec("b", "o"), wantKind: errs.KindPermissionDenied},
		{name: "no source", parent: testProject, spec: &computepb.ImageSpec{}, wantKind: errs.KindBadRequest},
		{name: "empty bucket", parent: testProject, spec: storageSpec("", "o"), wantKind: errs.KindBadRequest},
		{name: "missing object", parent: testProject, spec: storageSpec("b", "nope"), wantKind: errs.KindBadRequest},
		{name: "unknown disk", parent: testProject, spec: &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskId{SourceDiskId: "computedisk-e00x"}}, wantKind: errs.KindNotFound},
		{name: "unknown snapshot", parent: testProject, spec: &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskSnapshotId{SourceDiskSnapshotId: "computedisksnapshot-e00x"}}, wantKind: errs.KindNotFound},
		{name: "ok", parent: testProject, spec: storageSpec("b", "o")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.blobs.sources["b/o"] = []byte("data")
			_, err := f.svc.Create(t.Context(), CreateImageCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tt.parent, Name: "n"}, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
		})
	}
}

func TestFamilyVersionIsUnique(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.blobs.sources["b/o"] = []byte("data")
	spec := storageSpec("b", "o")
	spec.ImageFamily, spec.Version = "fam", "1"
	if _, err := f.create(t, "a", spec); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Create(t.Context(), CreateImageCmd{Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "b"}, Spec: spec})
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate family/version: %v", err)
	}
}

func TestGetLatestByFamily(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	now := f.clk.Now()
	add := func(id, parent, family, version string, state computepb.ImageStatus_State, deprecatedAt time.Time) {
		img := &computepb.Image{
			Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, CreatedAt: timestamppb.New(now)},
			Spec:     &computepb.ImageSpec{ImageFamily: family, Version: version},
			Status:   &computepb.ImageStatus{State: state},
		}
		if !deprecatedAt.IsZero() {
			img.Status.ImageFamilyDeprecation = &computepb.ImageStatus_ImageFamilyDeprecationStatus{DeprecatedAt: timestamppb.New(deprecatedAt)}
		}
		if err := f.images.Create(t.Context(), img); err != nil {
			t.Fatal(err)
		}
	}
	pub := PublicProjectID("e00")
	add("computeimage-e00a", pub, "os", "20250101", computepb.ImageStatus_READY, time.Time{})
	add("computeimage-e00b", pub, "os", "20250301", computepb.ImageStatus_READY, now.Add(time.Hour)) // deprecated soon
	add("computeimage-e00c", pub, "os", "20250401", computepb.ImageStatus_CREATING, time.Time{})     // not READY
	add("computeimage-e00d", pub, "old", "1", computepb.ImageStatus_READY, now.Add(-time.Hour))
	add("computeimage-e00e", pub, "old", "2", computepb.ImageStatus_READY, now.Add(-time.Hour))
	add("computeimage-u00a", PublicProjectID("u00"), "os", "20260101", computepb.ImageStatus_READY, time.Time{})

	latest := func(family, parent string) string {
		t.Helper()
		img, err := f.svc.GetLatestByFamily(t.Context(), family, parent)
		if err != nil {
			t.Fatalf("GetLatestByFamily(%s, %q): %v", family, parent, err)
		}
		return img.GetMetadata().GetId()
	}
	if got := latest("os", ""); got != "computeimage-e00b" {
		t.Fatalf("latest os = %s, want b (newest READY, deprecation not yet effective)", got)
	}
	f.clk.Advance(2 * time.Hour)
	if got := latest("os", ""); got != "computeimage-e00a" {
		t.Fatalf("latest os after deprecation = %s, want a", got)
	}
	if got := latest("old", ""); got != "computeimage-e00e" {
		t.Fatalf("latest of a fully deprecated family = %s, want newest (e)", got)
	}
	if got := latest("os", PublicProjectID("u00")); got != "computeimage-u00a" {
		t.Fatalf("latest os in u00 = %s", got)
	}
	if _, err := f.svc.GetLatestByFamily(t.Context(), "missing", ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("missing family: %v", err)
	}
	if _, err := f.svc.GetLatestByFamily(t.Context(), "", ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("empty family: %v", err)
	}
}

func TestKeepImmutable(t *testing.T) {
	t.Parallel()
	prev := storageSpec("b", "o")
	prev.ImageFamily, prev.Version, prev.CpuArchitecture, prev.ImageFamilyHumanReadable = "fam", "1", computepb.ImageSpec_AMD64, "Fam"
	tests := []struct {
		name    string
		next    func() *computepb.ImageSpec
		wantErr bool
	}{
		{name: "empty keeps all", next: func() *computepb.ImageSpec { return &computepb.ImageSpec{Description: "d"} }},
		{name: "same values", next: func() *computepb.ImageSpec {
			n := storageSpec("b", "o")
			n.ImageFamily, n.Version, n.CpuArchitecture = "fam", "1", computepb.ImageSpec_AMD64
			return n
		}},
		{name: "family", next: func() *computepb.ImageSpec { return &computepb.ImageSpec{ImageFamily: "x"} }, wantErr: true},
		{name: "version", next: func() *computepb.ImageSpec { return &computepb.ImageSpec{Version: "2"} }, wantErr: true},
		{name: "human readable", next: func() *computepb.ImageSpec { return &computepb.ImageSpec{ImageFamilyHumanReadable: "X"} }, wantErr: true},
		{name: "arch", next: func() *computepb.ImageSpec { return &computepb.ImageSpec{CpuArchitecture: computepb.ImageSpec_ARM64} }, wantErr: true},
		{name: "source object", next: func() *computepb.ImageSpec { return storageSpec("b", "other") }, wantErr: true},
		{name: "source kind", next: func() *computepb.ImageSpec {
			return &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskId{SourceDiskId: "computedisk-e00x"}}
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			next := tt.next()
			err := keepImmutable(prev, next)
			if (err != nil) != tt.wantErr {
				t.Fatalf("keepImmutable err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && (next.GetImageFamily() != "fam" || next.GetVersion() != "1" || next.GetSourceStorage().GetObjectName() != "o" ||
				next.GetCpuArchitecture() != computepb.ImageSpec_AMD64 || next.GetImageFamilyHumanReadable() != "Fam") {
				t.Fatalf("immutable fields not kept: %v", next)
			}
		})
	}
}

func TestUpdateAndDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.blobs.sources["b/o"] = []byte("data")
	img, err := f.create(t, "img", storageSpec("b", "o"))
	if err != nil {
		t.Fatal(err)
	}
	id := img.GetMetadata().GetId()
	ctx := t.Context()
	update := func(md *commonpb.ResourceMetadata, spec *computepb.ImageSpec) error {
		op, err := f.svc.Update(ctx, UpdateImageCmd{Metadata: md, Spec: spec})
		if err != nil {
			return err
		}
		return op.Err
	}
	if err := update(&commonpb.ResourceMetadata{Id: id, Name: "img"}, &computepb.ImageSpec{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.images.Get(ctx, id); got.GetMetadata().GetResourceVersion() != 1 {
		t.Fatalf("no-op update bumped resource_version to %d", got.GetMetadata().GetResourceVersion())
	}
	f.clk.Advance(time.Minute)
	if err := update(&commonpb.ResourceMetadata{Id: id, Name: "renamed"}, &computepb.ImageSpec{Description: "d"}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.images.Get(ctx, id)
	if got.GetMetadata().GetName() != "renamed" || got.GetMetadata().GetResourceVersion() != 2 ||
		!got.GetMetadata().GetUpdatedAt().AsTime().Equal(f.clk.Now()) || got.GetStatus().GetState() != computepb.ImageStatus_READY {
		t.Fatalf("after update: %v", got)
	}
	if err := update(&commonpb.ResourceMetadata{Id: id, ParentId: "project-e00other"}, nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("parent change: %v", err)
	}
	if err := update(&commonpb.ResourceMetadata{Id: id, ResourceVersion: 1}, nil); !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("stale version: %v", err)
	}
	op, err := f.svc.Delete(ctx, id, nil)
	if err != nil || op.Err != nil {
		t.Fatalf("delete: %v %v", err, op.Err)
	}
	if _, err := f.images.Get(ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("image still exists: %v", err)
	}
	if _, ok := f.blobs.images[id]; ok {
		t.Fatal("payload still exists")
	}
	page, err := f.svc.ListOperationsByParent(ctx, testProject, 2, "")
	if err != nil || len(page.Items) != 2 || page.Items[0].Description != "Delete image" || page.NextPageToken == "" {
		t.Fatalf("ListOperationsByParent page 1 = %+v, %v", page, err)
	}
	page, err = f.svc.ListOperationsByParent(ctx, testProject, 2, page.NextPageToken)
	if err != nil || len(page.Items) != 2 || page.Items[1].Description != "Create image" || page.NextPageToken != "" {
		t.Fatalf("ListOperationsByParent page 2 = %+v, %v", page, err)
	}
}

func TestPublicImagesAreReadOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cat, err := ParseCatalog([]byte("families: [{name: os, versions: [{version: '1'}]}]"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SeedPublicCatalog(t.Context(), SeedDeps{Images: f.images, Blobs: f.blobs, Clock: f.clk}, cat, testRegions); err != nil {
		t.Fatal(err)
	}
	img, err := f.svc.GetLatestByFamily(t.Context(), "os", "")
	if err != nil {
		t.Fatal(err)
	}
	id := img.GetMetadata().GetId()
	if !f.svc.IsPublicImage(t.Context(), id) || !f.svc.IsPublicProject(img.GetMetadata().GetParentId()) {
		t.Fatalf("seeded image %s not recognized as public", id)
	}
	if _, err := f.svc.Delete(t.Context(), id, nil); !errs.IsKind(err, errs.KindPermissionDenied) {
		t.Fatalf("Delete(public) = %v", err)
	}
	if _, err := f.svc.Update(t.Context(), UpdateImageCmd{Metadata: &commonpb.ResourceMetadata{Id: id}}); !errs.IsKind(err, errs.KindPermissionDenied) {
		t.Fatalf("Update(public) = %v", err)
	}
}

func TestListPublicUnknownRegion(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.svc.ListPublic(t.Context(), "eu-west9", 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("ListPublic(unknown) = %v", err)
	}
	if _, err := f.svc.List(t.Context(), ListImagesQuery{ParentID: testProject, PageSize: -1}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("List(page_size -1) = %v", err)
	}
	if _, err := f.svc.List(t.Context(), ListImagesQuery{ParentID: testProject, Filter: "name=x"}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("List(filter) = %v, want BadRequest naming the unsupported field", err)
	}
}
