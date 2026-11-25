package domain

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func ssdSpec(sizeGiB int64) *computepb.DiskSpec {
	return &computepb.DiskSpec{
		Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: sizeGiB}, Type: computepb.DiskSpec_NETWORK_SSD,
	}
}

func withImage(spec *computepb.DiskSpec, id string) *computepb.DiskSpec {
	spec.Source = &computepb.DiskSpec_SourceImageId{SourceImageId: id}
	return spec
}

func withSnapshot(spec *computepb.DiskSpec, id string) *computepb.DiskSpec {
	spec.Source = &computepb.DiskSpec_SourceSnapshotId{SourceSnapshotId: id}
	return spec
}

func withFamily(spec *computepb.DiskSpec, family string) *computepb.DiskSpec {
	spec.Source = &computepb.DiskSpec_SourceImageFamily{SourceImageFamily: &computepb.SourceImageFamily{ImageFamily: family}}
	return spec
}

// createDisk creates a disk in testProject. It returns the stored disk next to
// the operation error; a Submit error returns no disk.
func (f *diskFixture) createDisk(t *testing.T, name string, spec *computepb.DiskSpec) (*computepb.Disk, error) {
	t.Helper()
	op, err := f.ds.Create(t.Context(), CreateDiskCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name}, Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	d, gerr := f.disks.Get(t.Context(), op.ResourceID)
	if gerr != nil {
		t.Fatalf("created disk %s missing: %v", op.ResourceID, gerr)
	}
	return d, op.Err
}

func (f *diskFixture) updateDisk(t *testing.T, cmd UpdateDiskCmd) error {
	t.Helper()
	_, err := f.ds.Update(t.Context(), cmd)
	return err
}

func (f *diskFixture) mustGetDisk(t *testing.T, id string) *computepb.Disk {
	t.Helper()
	d, err := f.disks.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return d
}

func TestDiskCreateEmpty(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	spec := ssdSpec(10)
	d, err := f.createDisk(t, "empty", spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	st := d.GetStatus()
	if st.GetState() != computepb.DiskStatus_READY || st.GetReconciling() ||
		st.GetSizeBytes() != 10*gib || st.GetBlockSizeBytes() != 4096 {
		t.Fatalf("status = %v, want READY, 10 GiB, 4096-byte blocks", st)
	}
	if diff := cmp.Diff(ssdSpec(10), d.GetSpec(), protocmp.Transform()); diff != "" {
		t.Fatalf("spec was not echoed unchanged (-want +got):\n%s", diff)
	}
	if d.GetMetadata().GetResourceVersion() != 1 || d.GetMetadata().GetParentId() != testProject {
		t.Fatalf("metadata = %v", d.GetMetadata())
	}
	_ = spec
}

func TestDiskCreateBlockSizeAndUnits(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	spec := &computepb.DiskSpec{
		Size: &computepb.DiskSpec_SizeMebibytes{SizeMebibytes: 2048}, Type: computepb.DiskSpec_NETWORK_SSD, BlockSizeBytes: 8192,
	}
	d, err := f.createDisk(t, "units", spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.GetStatus().GetSizeBytes() != 2*gib || d.GetStatus().GetBlockSizeBytes() != 8192 {
		t.Fatalf("status = %v", d.GetStatus())
	}
	if diff := cmp.Diff(spec, d.GetSpec(), protocmp.Transform()); diff != "" {
		t.Fatalf("spec was not echoed unchanged (-want +got):\n%s", diff)
	}
}

func TestDiskCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		md       *commonpb.ResourceMetadata
		spec     *computepb.DiskSpec
		wantKind errs.Kind
	}{
		{name: "id set", md: &commonpb.ResourceMetadata{Id: "x", ParentId: testProject}, spec: ssdSpec(10), wantKind: errs.KindBadRequest},
		{name: "no parent", md: &commonpb.ResourceMetadata{}, spec: ssdSpec(10), wantKind: errs.KindBadRequest},
		{name: "unknown project", md: &commonpb.ResourceMetadata{ParentId: "project-e00mock00000000unknown"}, spec: ssdSpec(10), wantKind: errs.KindNotFound},
		{name: "no size", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD}, wantKind: errs.KindBadRequest},
		{name: "no type", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 10}}, wantKind: errs.KindBadRequest},
		{name: "hdd", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 10}, Type: computepb.DiskSpec_NETWORK_HDD}, wantKind: errs.KindBadRequest},
		{name: "bad block size", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 10}, Type: computepb.DiskSpec_NETWORK_SSD, BlockSizeBytes: 3000}, wantKind: errs.KindBadRequest},
		{name: "too big", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: ssdSpec(8193), wantKind: errs.KindBadRequest},
		{name: "ok", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: ssdSpec(8192)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newDiskFixture(t)
			op, err := f.ds.Create(t.Context(), CreateDiskCmd{Metadata: tt.md, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil && op != nil {
				t.Fatalf("Create returned an operation next to error %v", err)
			}
			if err != nil && len(f.ops.ops) != 0 {
				t.Fatalf("a failed validation created %d operations", len(f.ops.ops))
			}
		})
	}
}

func TestDiskCreateDuplicateName(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	if _, err := f.createDisk(t, "dup", ssdSpec(10)); err != nil {
		t.Fatal(err)
	}
	_, err := f.createDisk(t, "dup", ssdSpec(10))
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("second create err = %v, want already exists", err)
	}
}

func TestDiskCreateFromImage(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedImage("img-ready", testProject, "", computepb.ImageStatus_READY, 5*gib)
	f.seedImage("img-public", PublicProjectID("e00"), "", computepb.ImageStatus_READY, 5*gib)
	f.seedImage("img-creating", testProject, "", computepb.ImageStatus_CREATING, 5*gib)
	f.seedImage("img-foreign", otherProject, "", computepb.ImageStatus_READY, 5*gib)

	tests := []struct {
		name     string
		image    string
		sizeGiB  int64
		wantKind errs.Kind
	}{
		{name: "own image", image: "img-ready", sizeGiB: 10},
		{name: "public image", image: "img-public", sizeGiB: 10},
		{name: "exactly the minimum size", image: "img-ready", sizeGiB: 5},
		{name: "missing", image: "img-none", sizeGiB: 10, wantKind: errs.KindNotFound},
		{name: "not ready", image: "img-creating", sizeGiB: 10, wantKind: errs.KindBadState},
		{name: "foreign project", image: "img-foreign", sizeGiB: 10, wantKind: errs.KindBadRequest},
		{name: "too small", image: "img-ready", sizeGiB: 4, wantKind: errs.KindBadRequest},
		{name: "empty id", image: "", sizeGiB: 10, wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, err := f.createDisk(t, tt.name, withImage(ssdSpec(tt.sizeGiB), tt.image))
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil {
				return
			}
			st := d.GetStatus()
			if st.GetSourceImageId() != tt.image || st.GetSourceImageCpuArchitecture() != computepb.DiskStatus_ARM64 ||
				st.GetState() != computepb.DiskStatus_READY {
				t.Fatalf("status = %v, want source image %s, ARM64, READY", st, tt.image)
			}
		})
	}
}

func TestDiskCreateBootDiskCap(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedImage("img", testProject, "", computepb.ImageStatus_READY, 5*gib)
	spec := &computepb.DiskSpec{
		Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 331 * 93}, Type: computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED,
		BlockSizeBytes: 8192, Source: &computepb.DiskSpec_SourceImageId{SourceImageId: "img"},
	}
	if _, err := f.createDisk(t, "boot", spec); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("boot disk above 30720 GiB: err = %v, want bad request", err)
	}
	spec.Source = nil
	if _, err := f.createDisk(t, "data", spec); err != nil {
		t.Fatalf("same size without an image: %v", err)
	}
}

func TestDiskCreateFromFamily(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	public := PublicProjectID("e00")
	f.seedImage("img-old", public, "ubuntu", computepb.ImageStatus_READY, 5*gib)
	f.seedImage("img-new", public, "ubuntu", computepb.ImageStatus_READY, 20*gib)
	f.images.add(&computepb.Image{
		Metadata: &commonpb.ResourceMetadata{Id: "img-dep", ParentId: public},
		Spec:     &computepb.ImageSpec{ImageFamily: "legacy"},
		Status: &computepb.ImageStatus{
			State: computepb.ImageStatus_READY,
			ImageFamilyDeprecation: &computepb.ImageStatus_ImageFamilyDeprecationStatus{
				DeprecatedAt: timestamppb.New(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)),
			},
		},
	})

	d, err := f.createDisk(t, "fam", withFamily(ssdSpec(30), "ubuntu"))
	if err != nil {
		t.Fatalf("create from family: %v", err)
	}
	if d.GetStatus().GetSourceImageId() != "img-new" {
		t.Fatalf("source image = %q, want the latest of the family", d.GetStatus().GetSourceImageId())
	}
	if _, err := f.createDisk(t, "small", withFamily(ssdSpec(10), "ubuntu")); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("below the latest image's minimum: err = %v, want bad request", err)
	}
	if _, err := f.createDisk(t, "dep", withFamily(ssdSpec(30), "legacy")); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("deprecated family: err = %v, want bad state", err)
	}
	if _, err := f.createDisk(t, "none", withFamily(ssdSpec(30), "nope")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("unknown family: err = %v, want not found", err)
	}
	if _, err := f.createDisk(t, "nofam", withFamily(ssdSpec(30), "")); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("empty family: err = %v, want bad request", err)
	}
}

func TestDiskCreateFromSnapshot(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedSnapshot(t, "snap-ok", testProject, "disk-src", func(s *computepb.DiskSnapshot) {
		s.Status.SourceCpuArchitecture = computepb.DiskSnapshotStatus_ARM64
	})
	f.seedSnapshot(t, "snap-other", otherProject, "disk-src", nil)
	f.seedSnapshot(t, "snap-creating", testProject, "disk-src", func(s *computepb.DiskSnapshot) {
		s.Status.State = computepb.DiskSnapshotStatus_CREATING
	})

	tests := []struct {
		name     string
		snapshot string
		sizeGiB  int64
		wantKind errs.Kind
	}{
		{name: "ok", snapshot: "snap-ok", sizeGiB: 10},
		{name: "larger", snapshot: "snap-ok", sizeGiB: 20},
		{name: "missing", snapshot: "snap-none", sizeGiB: 10, wantKind: errs.KindNotFound},
		{name: "other project", snapshot: "snap-other", sizeGiB: 10, wantKind: errs.KindBadRequest},
		{name: "not ready", snapshot: "snap-creating", sizeGiB: 10, wantKind: errs.KindBadState},
		{name: "too small", snapshot: "snap-ok", sizeGiB: 9, wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, err := f.createDisk(t, tt.name, withSnapshot(ssdSpec(tt.sizeGiB), tt.snapshot))
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err == nil && d.GetStatus().GetSourceImageCpuArchitecture() != computepb.DiskStatus_ARM64 {
				t.Fatalf("architecture = %v, want ARM64 from the snapshot", d.GetStatus().GetSourceImageCpuArchitecture())
			}
		})
	}
}

func TestDiskCreateLocksSnapshotWhileCreating(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedSnapshot(t, "snap", testProject, "disk-src", nil)
	f.ops.deferred = true

	op, err := f.ds.Create(t.Context(), CreateDiskCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "from-snap"}, Spec: withSnapshot(ssdSpec(10), "snap"),
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := f.snaps.Get(t.Context(), "snap")
	if got := snap.GetStatus().GetLockState().GetDisks(); !slices.Equal(got, []string{op.ResourceID}) {
		t.Fatalf("snapshot lock_state.disks = %v, want [%s]", got, op.ResourceID)
	}
	if st := f.mustGetDisk(t, op.ResourceID).GetStatus(); st.GetState() != computepb.DiskStatus_CREATING || !st.GetReconciling() {
		t.Fatalf("disk status while creating = %v", st)
	}
	if _, err := f.ss.Delete(t.Context(), "snap", nil); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("snapshot delete while locked: err = %v, want bad state", err)
	}

	f.ops.finish()
	snap, _ = f.snaps.Get(t.Context(), "snap")
	if got := snap.GetStatus().GetLockState().GetDisks(); len(got) != 0 {
		t.Fatalf("snapshot lock_state.disks after create = %v, want empty", got)
	}
	if st := f.mustGetDisk(t, op.ResourceID).GetStatus(); st.GetState() != computepb.DiskStatus_READY || st.GetReconciling() {
		t.Fatalf("disk status after create = %v", st)
	}
}

func TestDiskUpdateLabels(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	f.clk.Advance(time.Hour)

	md := &commonpb.ResourceMetadata{Id: "disk-1", Labels: map[string]string{"env": "dev"}}
	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: md}); err != nil {
		t.Fatalf("update: %v", err)
	}
	d := f.mustGetDisk(t, "disk-1")
	if d.GetMetadata().GetResourceVersion() != 2 || d.GetMetadata().GetLabels()["env"] != "dev" {
		t.Fatalf("metadata after label change = %v", d.GetMetadata())
	}
	if got := d.GetMetadata().GetUpdatedAt().AsTime(); !got.Equal(f.clk.Now()) {
		t.Fatalf("updated_at = %v, want %v", got, f.clk.Now())
	}

	// The same update again changes nothing.
	f.clk.Advance(time.Hour)
	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: md}); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	again := f.mustGetDisk(t, "disk-1")
	if again.GetMetadata().GetResourceVersion() != 2 || !again.GetMetadata().GetUpdatedAt().AsTime().Equal(d.GetMetadata().GetUpdatedAt().AsTime()) {
		t.Fatalf("no-op update changed metadata: %v", again.GetMetadata())
	}
}

func TestDiskUpdateGrow(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	f.ops.deferred = true

	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: &commonpb.ResourceMetadata{Id: "disk-1"}, Spec: ssdSpec(20)}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	d := f.mustGetDisk(t, "disk-1")
	if d.GetStatus().GetState() != computepb.DiskStatus_UPDATING || !d.GetStatus().GetReconciling() {
		t.Fatalf("status during grow = %v, want UPDATING", d.GetStatus())
	}
	if d.GetStatus().GetSizeBytes() != 10*gib {
		t.Fatalf("size_bytes during grow = %d, want the old size", d.GetStatus().GetSizeBytes())
	}
	f.ops.finish()
	d = f.mustGetDisk(t, "disk-1")
	if d.GetStatus().GetState() != computepb.DiskStatus_READY || d.GetStatus().GetReconciling() || d.GetStatus().GetSizeBytes() != 20*gib {
		t.Fatalf("status after grow = %v, want READY with 20 GiB", d.GetStatus())
	}
	if d.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("resource_version = %d, want 2", d.GetMetadata().GetResourceVersion())
	}
}

func TestDiskUpdateRejections(t *testing.T) {
	t.Parallel()
	managed := &computepb.DiskEncryption{Type: computepb.DiskEncryption_DISK_ENCRYPTION_MANAGED}
	tests := []struct {
		name     string
		seed     func(*computepb.Disk)
		md       *commonpb.ResourceMetadata
		spec     *computepb.DiskSpec
		wantKind errs.Kind
	}{
		{name: "shrink", spec: ssdSpec(5), wantKind: errs.KindBadRequest},
		{name: "grow beyond the type maximum", spec: ssdSpec(8193), wantKind: errs.KindBadRequest},
		{
			name: "type change",
			spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED}, wantKind: errs.KindBadRequest,
		},
		{name: "block size change", spec: &computepb.DiskSpec{BlockSizeBytes: 8192}, wantKind: errs.KindBadRequest},
		{
			name: "source change",
			spec: &computepb.DiskSpec{Source: &computepb.DiskSpec_SourceImageId{SourceImageId: "img"}}, wantKind: errs.KindBadRequest,
		},
		{name: "encryption change", spec: &computepb.DiskSpec{DiskEncryption: managed}, wantKind: errs.KindBadRequest},
		{
			name: "resource version mismatch",
			md:   &commonpb.ResourceMetadata{Id: "disk-1", ResourceVersion: 7, Labels: map[string]string{"a": "b"}}, wantKind: errs.KindConflict,
		},
		{
			name: "parent change",
			md:   &commonpb.ResourceMetadata{Id: "disk-1", ParentId: otherProject, Labels: map[string]string{"a": "b"}}, wantKind: errs.KindBadRequest,
		},
		{
			name: "not ready",
			seed: func(d *computepb.Disk) { d.Status.State = computepb.DiskStatus_UPDATING },
			md:   &commonpb.ResourceMetadata{Id: "disk-1", Labels: map[string]string{"a": "b"}}, wantKind: errs.KindBadState,
		},
		{
			name: "forbid_deletion on a managed disk",
			seed: func(d *computepb.Disk) { d.Status.ManagedBy = "instance-1" },
			spec: &computepb.DiskSpec{ForbidDeletion: true}, wantKind: errs.KindBadState,
		},
		{
			name: "matching resource version",
			md:   &commonpb.ResourceMetadata{Id: "disk-1", ResourceVersion: 1, Labels: map[string]string{"a": "b"}},
		},
		{name: "same parent", md: &commonpb.ResourceMetadata{Id: "disk-1", ParentId: testProject, Labels: map[string]string{"a": "b"}}},
		{name: "forbid_deletion on a standalone disk", spec: &computepb.DiskSpec{ForbidDeletion: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newDiskFixture(t)
			f.seedDisk(t, "disk-1", tt.seed)
			before := f.mustGetDisk(t, "disk-1")
			md := tt.md
			if md == nil {
				md = &commonpb.ResourceMetadata{Id: "disk-1"}
			}
			err := f.updateDisk(t, UpdateDiskCmd{Metadata: md, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Update kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil {
				if diff := cmp.Diff(before, f.mustGetDisk(t, "disk-1"), protocmp.Transform()); diff != "" {
					t.Fatalf("a rejected update changed the disk (-before +after):\n%s", diff)
				}
			}
		})
	}
}

func TestDiskUpdateKeepsUnsetImmutableFields(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", func(d *computepb.Disk) {
		d.Spec = &computepb.DiskSpec{
			Size:           &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 93},
			Type:           computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED,
			BlockSizeBytes: 8192,
			Source:         &computepb.DiskSpec_SourceImageId{SourceImageId: "img"},
			DiskEncryption: &computepb.DiskEncryption{Type: computepb.DiskEncryption_DISK_ENCRYPTION_MANAGED},
		}
		d.Status.SizeBytes, d.Status.BlockSizeBytes, d.Status.SourceImageId = 93*gib, 8192, "img"
	})
	// Only the size is sent: type, block size, source and encryption stay.
	spec := &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 186}}
	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: &commonpb.ResourceMetadata{Id: "disk-1"}, Spec: spec}); err != nil {
		t.Fatalf("update: %v", err)
	}
	d := f.mustGetDisk(t, "disk-1")
	want := &computepb.DiskSpec{
		Size:           &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 186},
		Type:           computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED,
		BlockSizeBytes: 8192,
		Source:         &computepb.DiskSpec_SourceImageId{SourceImageId: "img"},
		DiskEncryption: &computepb.DiskEncryption{Type: computepb.DiskEncryption_DISK_ENCRYPTION_MANAGED},
	}
	if diff := cmp.Diff(want, d.GetSpec(), protocmp.Transform()); diff != "" {
		t.Fatalf("spec after update (-want +got):\n%s", diff)
	}
	if d.GetStatus().GetSizeBytes() != 186*gib || d.GetStatus().GetState() != computepb.DiskStatus_READY {
		t.Fatalf("status = %v", d.GetStatus())
	}

	// Repeating the stored immutable values is accepted too.
	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: &commonpb.ResourceMetadata{Id: "disk-1"}, Spec: want}); err != nil {
		t.Fatalf("update with the stored immutable values: %v", err)
	}
}

func TestDiskUpdateResetMaskClearsLabels(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", func(d *computepb.Disk) { d.Metadata.Labels = map[string]string{"a": "b"} })
	rm, err := mask.Parse("metadata.labels")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: &commonpb.ResourceMetadata{Id: "disk-1"}, ResetMask: rm}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := f.mustGetDisk(t, "disk-1").GetMetadata().GetLabels(); len(got) != 0 {
		t.Fatalf("labels = %v, want cleared by the reset mask", got)
	}
}

func TestDiskUpdateMissing(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: &commonpb.ResourceMetadata{Id: "nope"}}); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update of a missing disk: err = %v, want not found", err)
	}
	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: &commonpb.ResourceMetadata{}}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("update without an id: err = %v, want bad request", err)
	}
}

func TestDiskDelete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		seed     func(*computepb.Disk)
		wantKind errs.Kind
	}{
		{name: "plain disk"},
		{name: "forbid_deletion", seed: func(d *computepb.Disk) { d.Spec.ForbidDeletion = true }, wantKind: errs.KindBadState},
		{name: "read-write attachment", seed: func(d *computepb.Disk) { d.Status.ReadWriteAttachment = "instance-1" }, wantKind: errs.KindBadState},
		{name: "read-only attachments", seed: func(d *computepb.Disk) { d.Status.ReadOnlyAttachments = []string{"instance-1"} }, wantKind: errs.KindBadState},
		{name: "managed_by", seed: func(d *computepb.Disk) { d.Status.ManagedBy = "instance-1" }, wantKind: errs.KindBadState},
		{
			name: "image lock",
			seed: func(d *computepb.Disk) {
				d.Status.LockState = &computepb.DiskStatus_LockState{Images: []string{"img-1"}}
			},
			wantKind: errs.KindBadState,
		},
		{
			name: "snapshot lock",
			seed: func(d *computepb.Disk) {
				d.Status.LockState = &computepb.DiskStatus_LockState{Snapshots: []string{"snap-1"}}
			},
			wantKind: errs.KindBadState,
		},
		{
			name: "empty lock state",
			seed: func(d *computepb.Disk) { d.Status.LockState = &computepb.DiskStatus_LockState{} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newDiskFixture(t)
			f.seedDisk(t, "disk-1", tt.seed)
			op, err := f.ds.Delete(t.Context(), "disk-1", nil)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Delete kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			_, gerr := f.disks.Get(t.Context(), "disk-1")
			if tt.wantKind == errs.KindUnknown {
				if op.Err != nil || !errs.IsKind(gerr, errs.KindNotFound) {
					t.Fatalf("after delete: op.Err = %v, Get = %v, want removed", op.Err, gerr)
				}
				return
			}
			if gerr != nil {
				t.Fatalf("a refused delete removed the disk: %v", gerr)
			}
			if st := f.mustGetDisk(t, "disk-1").GetStatus().GetState(); st != computepb.DiskStatus_READY {
				t.Fatalf("state after a refused delete = %v, want READY", st)
			}
		})
	}
}

func TestDiskDeleteForbidDeletionWording(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", func(d *computepb.Disk) { d.Spec.ForbidDeletion = true })
	_, err := f.ds.Delete(t.Context(), "disk-1", nil)
	if !errs.IsKind(err, errs.KindBadState) || !strings.Contains(err.Error(), "disk cannot be deleted because forbid_deletion is set") {
		t.Fatalf("Delete err = %v, want the documented forbid_deletion wording", err)
	}
}

func TestDiskDeleteMissingAndMarksDeleting(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	if _, err := f.ds.Delete(t.Context(), "nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete of a missing disk: err = %v, want not found", err)
	}
	if _, err := f.ds.Delete(t.Context(), "", nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("delete without an id: err = %v, want bad request", err)
	}
	f.seedDisk(t, "disk-1", nil)
	f.ops.deferred = true
	if _, err := f.ds.Delete(t.Context(), "disk-1", nil); err != nil {
		t.Fatal(err)
	}
	if st := f.mustGetDisk(t, "disk-1").GetStatus(); st.GetState() != computepb.DiskStatus_DELETING || !st.GetReconciling() {
		t.Fatalf("status during delete = %v, want DELETING", st)
	}
	f.ops.finish()
	if _, err := f.ds.Get(t.Context(), "disk-1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete: err = %v, want not found", err)
	}
}

func TestDiskDeleteKeepsSnapshots(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	f.seedSnapshot(t, "snap-1", testProject, "disk-1", nil)
	if _, err := f.ds.Delete(t.Context(), "disk-1", nil); err != nil {
		t.Fatalf("delete with an existing snapshot: %v", err)
	}
	snap, err := f.snaps.Get(t.Context(), "snap-1")
	if err != nil {
		t.Fatalf("snapshot after its disk was deleted: %v", err)
	}
	if snap.GetSpec().GetSourceDiskId() != "disk-1" || snap.GetStatus().GetState() != computepb.DiskSnapshotStatus_READY {
		t.Fatalf("snapshot = %v", snap)
	}
}

func TestCheckDiskDeletable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		mutate   func(*computepb.Disk)
		wantKind errs.Kind
	}{
		{name: "free disk", mutate: func(*computepb.Disk) {}},
		{name: "forbid_deletion", mutate: func(d *computepb.Disk) { d.Spec.ForbidDeletion = true }, wantKind: errs.KindBadState},
		{name: "read-write attachment", mutate: func(d *computepb.Disk) { d.Status.ReadWriteAttachment = "i" }, wantKind: errs.KindBadState},
		{name: "read-only attachment", mutate: func(d *computepb.Disk) { d.Status.ReadOnlyAttachments = []string{"i"} }, wantKind: errs.KindBadState},
		{name: "managed_by", mutate: func(d *computepb.Disk) { d.Status.ManagedBy = "i" }, wantKind: errs.KindBadState},
		{
			name:     "locked by an image",
			mutate:   func(d *computepb.Disk) { d.Status.LockState = &computepb.DiskStatus_LockState{Images: []string{"x"}} },
			wantKind: errs.KindBadState,
		},
		{
			name: "locked by a snapshot",
			mutate: func(d *computepb.Disk) {
				d.Status.LockState = &computepb.DiskStatus_LockState{Snapshots: []string{"x"}}
			},
			wantKind: errs.KindBadState,
		},
		{name: "nil status and spec", mutate: func(d *computepb.Disk) { d.Status, d.Spec = nil, nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &computepb.Disk{
				Metadata: &commonpb.ResourceMetadata{Id: "disk-1"}, Spec: &computepb.DiskSpec{}, Status: &computepb.DiskStatus{},
			}
			tt.mutate(d)
			if got := errs.KindOf(CheckDiskDeletable(d)); got != tt.wantKind {
				t.Fatalf("CheckDiskDeletable kind = %v, want %v", got, tt.wantKind)
			}
		})
	}
}

func TestDiskListOperationsByParent(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	var ids []string
	for _, name := range []string{"a", "b", "c"} {
		d, err := f.createDisk(t, name, ssdSpec(10))
		if err != nil {
			t.Fatal(err)
		}
		_ = d
	}
	// The newest operation is the delete of a disk that no longer exists.
	dels, err := f.ds.Delete(t.Context(), f.firstDiskID(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.idx.mu.Lock()
	ids = slices.Clone(f.idx.ids[testProject])
	f.idx.mu.Unlock()
	slices.Reverse(ids)
	if len(ids) != 4 || ids[0] != dels.ID {
		t.Fatalf("indexed operation ids = %v", ids)
	}
	// An operation in another project must not show up.
	if _, err := f.ds.Create(t.Context(), CreateDiskCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: otherProject, Name: "x"}, Spec: ssdSpec(10),
	}); err != nil {
		t.Fatal(err)
	}

	var got []string
	token := ""
	for page := range 10 {
		p, err := f.ds.ListOperationsByParent(t.Context(), testProject, 3, token)
		if err != nil {
			t.Fatal(err)
		}
		if page == 0 && len(p.Items) != 3 {
			t.Fatalf("first page has %d items, want 3", len(p.Items))
		}
		for _, op := range p.Items {
			got = append(got, op.ID)
		}
		if token = p.NextPageToken; token == "" {
			break
		}
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("operations = %v, want newest first %v", got, ids)
	}

	if _, err := f.ds.ListOperationsByParent(t.Context(), "", 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("empty parent: err = %v, want bad request", err)
	}
	if _, err := f.ds.ListOperationsByParent(t.Context(), testProject, 0, "garbage!"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad page token: err = %v, want bad request", err)
	}
	if _, err := f.ds.ListOperationsByParent(t.Context(), testProject, 5000, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("page size too large: err = %v, want bad request", err)
	}
}

// firstDiskID returns the ID of the first disk created in testProject.
func (f *diskFixture) firstDiskID(t *testing.T) string {
	t.Helper()
	p, err := f.disks.List(t.Context(), testProject, 1, "")
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("List = %v, %v", p, err)
	}
	return p.Items[0].GetMetadata().GetId()
}

func TestDiskListAndGetByName(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := f.createDisk(t, name, ssdSpec(10)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := f.ds.List(t.Context(), testProject, 2, "", "")
	if err != nil || len(p.Items) != 2 || p.NextPageToken == "" {
		t.Fatalf("first page = %v, %v", p, err)
	}
	p2, err := f.ds.List(t.Context(), testProject, 2, p.NextPageToken, "")
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" {
		t.Fatalf("second page = %v, %v", p2, err)
	}
	if p2.Items[0].GetMetadata().GetName() != "c" {
		t.Fatalf("second page holds %q, want creation order", p2.Items[0].GetMetadata().GetName())
	}
	if _, err := f.ds.List(t.Context(), testProject, 0, "", "name=a"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("List with a filter: err = %v, want bad request", err)
	}
	d, err := f.ds.GetByName(t.Context(), testProject, "b")
	if err != nil || d.GetMetadata().GetName() != "b" {
		t.Fatalf("GetByName = %v, %v", d, err)
	}
	if _, err := f.ds.GetByName(t.Context(), "", "b"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("GetByName without parent: err = %v", err)
	}
	if _, err := f.ds.Get(t.Context(), ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("Get without id: err = %v", err)
	}
}

func TestNewDiskServiceRequiresDependencies(t *testing.T) {
	t.Parallel()
	if _, err := NewDiskService(DiskDeps{}); err == nil {
		t.Fatal("NewDiskService accepted empty dependencies")
	}
	if _, err := NewSnapshotService(DiskDeps{}); err == nil {
		t.Fatal("NewSnapshotService accepted empty dependencies")
	}
}
