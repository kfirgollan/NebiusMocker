package domain

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// stubDiskReader is a map-backed DiskReader.
type stubDiskReader struct {
	mu        sync.Mutex
	disks     map[string]*computepb.Disk
	snapshots map[string]*computepb.DiskSnapshot
}

func newStubDiskReader() *stubDiskReader {
	return &stubDiskReader{disks: map[string]*computepb.Disk{}, snapshots: map[string]*computepb.DiskSnapshot{}}
}

func (s *stubDiskReader) Disk(_ context.Context, id string) (*computepb.Disk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.disks[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(d), nil
}

func (s *stubDiskReader) Snapshot(_ context.Context, id string) (*computepb.DiskSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.snapshots[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(d), nil
}

// LockForImage implements DiskLocker with the same rules as DiskLocks.
func (s *stubDiskReader) LockForImage(_ context.Context, diskID, parentID, imageID string) (*computepb.Disk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.disks[diskID]
	switch {
	case !ok:
		return nil, errs.NotFound(diskID)
	case d.GetMetadata().GetParentId() != parentID:
		return nil, errs.BadRequest(errs.Violation{Field: "spec.source_disk_id", Message: "other project"})
	case d.GetStatus().GetState() != computepb.DiskStatus_READY:
		return nil, errs.BadState(diskID, "not READY")
	}
	if d.GetStatus().GetLockState() == nil {
		d.Status.LockState = &computepb.DiskStatus_LockState{}
	}
	d.Status.LockState.Images = append(d.Status.LockState.Images, imageID)
	return proto.CloneOf(d), nil
}

// UnlockForImage implements DiskLocker.
func (s *stubDiskReader) UnlockForImage(_ context.Context, diskID, imageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.disks[diskID]; ok && d.GetStatus().GetLockState() != nil {
		d.Status.LockState.Images = slices.DeleteFunc(d.GetStatus().GetLockState().GetImages(), func(x string) bool { return x == imageID })
	}
	return nil
}

func (s *stubDiskReader) lockedImages(diskID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.disks[diskID].GetStatus().GetLockState().GetImages())
}

func (s *stubDiskReader) addDisk(id, parent string, size int64, state computepb.DiskStatus_State, arch computepb.DiskStatus_SourceImageCPUArchitecture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disks[id] = &computepb.Disk{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent},
		Status:   &computepb.DiskStatus{State: state, SizeBytes: size, SourceImageCpuArchitecture: arch},
	}
}

func (s *stubDiskReader) addSnapshot(id, parent, diskID string, content int64, state computepb.DiskSnapshotStatus_State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[id] = &computepb.DiskSnapshot{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent},
		Spec:     &computepb.DiskSnapshotSpec{SourceDiskId: diskID},
		Status: &computepb.DiskSnapshotStatus{
			State: state, ContentSizeBytes: content, SourceCpuArchitecture: computepb.DiskSnapshotStatus_ARM64,
		},
	}
}

func diskSpec(id string) *computepb.ImageSpec {
	return &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskId{SourceDiskId: id}}
}

func snapshotSpec(id string) *computepb.ImageSpec {
	return &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskSnapshotId{SourceDiskSnapshotId: id}}
}

func TestCreateFromDisk(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.disks.addDisk("computedisk-e00a", testProject, 40<<30, computepb.DiskStatus_READY, computepb.DiskStatus_ARM64)
	var lockedDuringImport []string
	f.blobs.putHook = func(string) { lockedDuringImport = f.disks.lockedImages("computedisk-e00a") }
	img, err := f.create(t, "from-disk", diskSpec("computedisk-e00a"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// disk.proto: locked while the image is being created, released after.
	if len(lockedDuringImport) != 1 || lockedDuringImport[0] != img.GetMetadata().GetId() {
		t.Fatalf("lock_state.images during import = %v, want [%s]", lockedDuringImport, img.GetMetadata().GetId())
	}
	if got := f.disks.lockedImages("computedisk-e00a"); len(got) != 0 {
		t.Fatalf("lock_state.images after READY = %v, want empty", got)
	}
	st := img.GetStatus()
	if st.GetState() != computepb.ImageStatus_READY || st.GetMinDiskSizeBytes() != 40<<30 ||
		st.GetStorageSizeBytes() != blockPayloadSize || img.GetSpec().GetCpuArchitecture() != computepb.ImageSpec_ARM64 ||
		img.GetSpec().GetSourceDiskId() != "computedisk-e00a" {
		t.Fatalf("image from disk = %v", img)
	}
	blob := f.blobs.images[img.GetMetadata().GetId()]
	if blob.meta[MetaSource] != "computedisk-e00a" || blob.meta[MetaFormat] != "qcow2" || blob.meta[MetaVirtualSize] != "42949672960" {
		t.Fatalf("payload metadata = %v", blob.meta)
	}
}

func TestCreateFromSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.disks.addDisk("computedisk-e00a", testProject, 30<<30, computepb.DiskStatus_READY, 0)
	f.disks.addSnapshot("computedisksnapshot-e00a", testProject, "computedisk-e00a", 2<<30, computepb.DiskSnapshotStatus_READY)
	f.disks.addSnapshot("computedisksnapshot-e00b", testProject, "computedisk-e00gone", 5<<30+1, computepb.DiskSnapshotStatus_READY)

	img, err := f.create(t, "from-snap", snapshotSpec("computedisksnapshot-e00a"))
	if err != nil {
		t.Fatal(err)
	}
	// A disk made from the image must be at least as big as the snapshot's disk.
	if img.GetStatus().GetMinDiskSizeBytes() != 30<<30 || img.GetSpec().GetCpuArchitecture() != computepb.ImageSpec_ARM64 {
		t.Fatalf("image from snapshot = %v", img)
	}
	// Source disk deleted since: size after the snapshot's content, rounded up to a GiB.
	img, err = f.create(t, "from-orphan-snap", snapshotSpec("computedisksnapshot-e00b"))
	if err != nil {
		t.Fatal(err)
	}
	if img.GetStatus().GetMinDiskSizeBytes() != 6<<30 {
		t.Fatalf("image from orphan snapshot min disk = %d", img.GetStatus().GetMinDiskSizeBytes())
	}
}

// failingBlobs fails every PutImage.
type failingBlobs struct{ *fakeBlobs }

func (failingBlobs) PutImage(context.Context, string, io.Reader, int64, map[string]string) (BlobInfo, error) {
	return BlobInfo{}, errors.New("object store down")
}

func TestFailedImportFromDiskReleasesLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.disks.addDisk("computedisk-e00a", testProject, 10<<30, computepb.DiskStatus_READY, 0)
	svc, err := NewImageService(ImageDeps{
		Images: f.images, OpIndex: &fakeOpIndex{}, Blobs: failingBlobs{f.blobs}, Projects: fakeProjects{testProject: true},
		Disks: f.disks, DiskLocks: f.disks, Ops: f.ops, IDs: &fakeIDs{}, Clock: f.clk, Regions: testRegions,
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Create(t.Context(), CreateImageCmd{Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "x"}, Spec: diskSpec("computedisk-e00a")})
	if err != nil || op.Err == nil {
		t.Fatalf("Create = %v (op %+v); want a failed operation", err, op)
	}
	img, _ := f.images.Get(t.Context(), op.ResourceID)
	if img.GetStatus().GetState() != computepb.ImageStatus_ERROR {
		t.Fatalf("image state = %v, want ERROR", img.GetStatus().GetState())
	}
	if got := f.disks.lockedImages("computedisk-e00a"); len(got) != 0 {
		t.Fatalf("lock_state.images after a failed import = %v, want empty", got)
	}
}

func TestCreateFromBlockSourceErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		spec     *computepb.ImageSpec
		wantKind errs.Kind
	}{
		{name: "missing disk", spec: diskSpec("computedisk-e00none"), wantKind: errs.KindNotFound},
		{name: "disk not ready", spec: diskSpec("computedisk-e00creating"), wantKind: errs.KindBadState},
		{name: "disk in another project", spec: diskSpec("computedisk-e00other"), wantKind: errs.KindBadRequest},
		{name: "missing snapshot", spec: snapshotSpec("computedisksnapshot-e00none"), wantKind: errs.KindNotFound},
		{name: "snapshot not ready", spec: snapshotSpec("computedisksnapshot-e00creating"), wantKind: errs.KindBadState},
		{name: "empty disk id", spec: diskSpec(""), wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.disks.addDisk("computedisk-e00creating", testProject, 1<<30, computepb.DiskStatus_CREATING, 0)
			f.disks.addDisk("computedisk-e00other", "project-e00mock00000000another", 1<<30, computepb.DiskStatus_READY, 0)
			f.disks.addSnapshot("computedisksnapshot-e00creating", testProject, "", 1<<30, computepb.DiskSnapshotStatus_CREATING)
			_, err := f.svc.Create(t.Context(), CreateImageCmd{Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "n"}, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if len(f.images.order) != 0 {
				t.Fatal("a failed Create stored an image")
			}
		})
	}
}
