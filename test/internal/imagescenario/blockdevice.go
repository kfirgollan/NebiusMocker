package imagescenario

import (
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
)

// CreateDisk creates a READY network SSD disk of sizeGiB through DiskService.
func CreateDisk(t *testing.T, env *Env, name string, sizeGiB int64) *computepb.Disk {
	t.Helper()
	disks := computesvc.NewDiskService(env.Inst.SDK(t))
	op, err := disks.Create(t.Context(), &computepb.CreateDiskRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.Inst.ProjectID, Name: name},
		Spec: &computepb.DiskSpec{
			Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: sizeGiB}, Type: computepb.DiskSpec_NETWORK_SSD,
		},
	})
	if err != nil {
		t.Fatalf("create disk %s: %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("create disk %s wait: %v", name, err)
	}
	d, err := disks.Get(t.Context(), &computepb.GetDiskRequest{Id: op.ResourceID()})
	if err != nil || d.GetStatus().GetState() != computepb.DiskStatus_READY {
		t.Fatalf("disk %s = %v, %v; want READY", name, d, err)
	}
	return d
}

// CreateSnapshot snapshots a disk through DiskSnapshotService.
func CreateSnapshot(t *testing.T, env *Env, name, diskID string) *computepb.DiskSnapshot {
	t.Helper()
	snaps := computesvc.NewDiskSnapshotService(env.Inst.SDK(t))
	op, err := snaps.Create(t.Context(), &computepb.CreateDiskSnapshotRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.Inst.ProjectID, Name: name},
		Spec:     &computepb.DiskSnapshotSpec{SourceDiskId: diskID},
	})
	if err != nil {
		t.Fatalf("create snapshot %s: %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("create snapshot %s wait: %v", name, err)
	}
	s, err := snaps.Get(t.Context(), &computepb.GetDiskSnapshotRequest{Id: op.ResourceID()})
	if err != nil || s.GetStatus().GetState() != computepb.DiskSnapshotStatus_READY {
		t.Fatalf("snapshot %s = %v, %v; want READY", name, s, err)
	}
	return s
}

// createFrom creates an image with the given source and waits for it.
func createFrom(t *testing.T, env *Env, name string, spec *computepb.ImageSpec) *computepb.Image {
	t.Helper()
	op, err := env.Images.Create(t.Context(), &computepb.CreateImageRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.Inst.ProjectID, Name: name}, Spec: spec,
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create(%s) wait: %v", name, err)
	}
	img, err := env.Images.Get(t.Context(), &computepb.GetImageRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// FromDiskAndSnapshot creates a disk and a snapshot through the compute
// services, then images from each, and checks size, status and payload, and
// the disk lock: disk.proto, "Disk is locked for deletion and for read-write
// operations while image is being created". The env's operation delay must be
// long enough (≥ 500ms) to observe the CREATING window. A disk that does not
// exist is NOT_FOUND.
func FromDiskAndSnapshot(t *testing.T, env *Env) {
	t.Helper()
	ctx := t.Context()
	disk := CreateDisk(t, env, "data-disk", 40)
	diskID := disk.GetMetadata().GetId()
	disks := computesvc.NewDiskService(env.Inst.SDK(t))

	op, err := env.Images.Create(ctx, &computepb.CreateImageRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.Inst.ProjectID, Name: "from-disk"},
		Spec:     &computepb.ImageSpec{Description: "from disk", Source: &computepb.ImageSpec_SourceDiskId{SourceDiskId: diskID}},
	})
	if err != nil {
		t.Fatalf("Create(from-disk): %v", err)
	}
	imageID := op.ResourceID()
	locked, err := disks.Get(ctx, &computepb.GetDiskRequest{Id: diskID})
	if err != nil {
		t.Fatal(err)
	}
	if op.Done() || !slices.Contains(locked.GetStatus().GetLockState().GetImages(), imageID) {
		t.Fatalf("while the image is CREATING (done=%v): disk lock_state = %v, want images [%s]", op.Done(), locked.GetStatus().GetLockState(), imageID)
	}
	_, err = disks.Delete(ctx, &computepb.DeleteDiskRequest{Id: diskID})
	if code := status.Code(err); code == codes.OK {
		t.Fatal("a disk locked by an image being created was deleted")
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Create(from-disk) wait: %v", err)
	}
	released, err := disks.Get(ctx, &computepb.GetDiskRequest{Id: diskID})
	if err != nil || len(released.GetStatus().GetLockState().GetImages()) != 0 {
		t.Fatalf("after the image is READY: disk lock_state = %v (%v), want no images", released.GetStatus().GetLockState(), err)
	}
	fromDisk, err := env.Images.Get(ctx, &computepb.GetImageRequest{Id: imageID})
	if err != nil {
		t.Fatal(err)
	}
	if fromDisk.GetStatus().GetState() != computepb.ImageStatus_READY || fromDisk.GetStatus().GetMinDiskSizeBytes() != 40<<30 ||
		fromDisk.GetSpec().GetSourceDiskId() != disk.GetMetadata().GetId() || fromDisk.GetSpec().GetCpuArchitecture() != computepb.ImageSpec_AMD64 {
		t.Fatalf("image from disk = %v", fromDisk)
	}
	AssertBlob(t, env.Store, fromDisk)

	snap := CreateSnapshot(t, env, "data-snap", disk.GetMetadata().GetId())
	fromSnap := createFrom(t, env, "from-snapshot",
		&computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskSnapshotId{SourceDiskSnapshotId: snap.GetMetadata().GetId()}})
	if fromSnap.GetStatus().GetState() != computepb.ImageStatus_READY || fromSnap.GetStatus().GetMinDiskSizeBytes() != 40<<30 ||
		fromSnap.GetSpec().GetSourceDiskSnapshotId() != snap.GetMetadata().GetId() {
		t.Fatalf("image from snapshot = %v", fromSnap)
	}
	AssertBlob(t, env.Store, fromSnap)

	_, err = env.Images.Create(t.Context(), &computepb.CreateImageRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.Inst.ProjectID, Name: "ghost"},
		Spec:     &computepb.ImageSpec{Source: &computepb.ImageSpec_SourceDiskId{SourceDiskId: "computedisk-e00mock0000000000999"}},
	})
	AssertCode(t, err, codes.NotFound)
}
