package domain

import (
	"context"
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
	img, err := f.create(t, "from-disk", diskSpec("computedisk-e00a"))
	if err != nil {
		t.Fatalf("create: %v", err)
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
