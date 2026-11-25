package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// blockPayloadSize is the size of the payload written for images made from a
// disk or snapshot. Disks keep no data in the mocker, so the image is the
// qcow2 form of an empty disk: a small, sparse file whose header declares the
// disk's size.
const blockPayloadSize = 64 << 10

// imageSource is the validated spec.source of a Create request. Exactly one of
// storage, diskID or snapshotID is set. resolveSource fills the block-device
// fields that the operation's Work step needs.
type imageSource struct {
	storage    *computepb.ImageSpec_SourceStorage
	diskID     string
	snapshotID string

	// Set by resolveSource for disk and snapshot sources.
	blockSize int64  // bytes of the device the image describes
	blockRef  string // "computedisk-…" or "computedisksnapshot-…"
	locked    bool   // the source disk carries this image in lock_state.images
}

// sourceOf validates the source oneof's shape (presence and required fields).
func sourceOf(spec *computepb.ImageSpec) (*imageSource, error) {
	switch src := spec.GetSource().(type) {
	case *computepb.ImageSpec_SourceStorage_:
		st := src.SourceStorage
		var v []errs.Violation
		if st.GetBucketName() == "" {
			v = append(v, errs.Violation{Field: "spec.source_storage.bucket_name", Message: "required"})
		}
		if st.GetObjectName() == "" {
			v = append(v, errs.Violation{Field: "spec.source_storage.object_name", Message: "required"})
		}
		if len(v) > 0 {
			return nil, errs.BadRequest(v...)
		}
		return &imageSource{storage: st}, nil
	case *computepb.ImageSpec_SourceDiskId:
		if src.SourceDiskId == "" {
			return nil, errs.BadRequest(errs.Violation{Field: "spec.source_disk_id", Message: "required"})
		}
		return &imageSource{diskID: src.SourceDiskId}, nil
	case *computepb.ImageSpec_SourceDiskSnapshotId:
		if src.SourceDiskSnapshotId == "" {
			return nil, errs.BadRequest(errs.Violation{Field: "spec.source_disk_snapshot_id", Message: "required"})
		}
		return &imageSource{snapshotID: src.SourceDiskSnapshotId}, nil
	default:
		return nil, errs.BadRequest(errs.Violation{Field: "spec.source", Message: "one of source_disk_id, source_storage, source_disk_snapshot_id is required"})
	}
}

// resolveSource checks that the source exists (and, for a disk or snapshot,
// is READY and in the image's project) and applies the cpu_architecture
// default: the source's architecture, else AMD64 ("By default the image uses
// AMD64", CLI docs).
func (s *ImageService) resolveSource(ctx context.Context, img *computepb.Image, src *imageSource) error {
	var arch computepb.ImageSpec_CPUArchitecture
	var err error
	switch {
	case src.storage != nil:
		err = s.checkStorageSource(ctx, src.storage)
	case src.diskID != "":
		arch, err = s.lockDisk(ctx, img, src)
	default:
		arch, err = s.resolveSnapshot(ctx, img.GetMetadata().GetParentId(), src)
	}
	if err != nil {
		return err
	}
	if img.GetSpec().GetCpuArchitecture() == computepb.ImageSpec_UNSPECIFIED {
		img.Spec.CpuArchitecture = arch
		if arch == computepb.ImageSpec_UNSPECIFIED {
			img.Spec.CpuArchitecture = computepb.ImageSpec_AMD64
		}
	}
	return nil
}

func (s *ImageService) checkStorageSource(ctx context.Context, st *computepb.ImageSpec_SourceStorage) error {
	if _, err := s.blobs.StatSource(ctx, st.GetBucketName(), st.GetObjectName()); err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return errs.BadRequest(errs.Violation{
				Field:   "spec.source_storage.object_name",
				Message: fmt.Sprintf("object %q not found in bucket %q", st.GetObjectName(), st.GetBucketName()),
			})
		}
		return fmt.Errorf("stat source object: %w", err)
	}
	return nil
}

// lockDisk checks the source disk (exists, same project, READY) and adds the
// image to its status.lock_state.images in one step. disk.proto: "Disk is
// locked for deletion and for read-write operations while image is being
// created"; the lock is released when the import finishes (unlockSource).
func (s *ImageService) lockDisk(ctx context.Context, img *computepb.Image, src *imageSource) (computepb.ImageSpec_CPUArchitecture, error) {
	d, err := s.locks.LockForImage(ctx, src.diskID, img.GetMetadata().GetParentId(), img.GetMetadata().GetId())
	if err != nil {
		return 0, fmt.Errorf("source disk %s: %w", src.diskID, err)
	}
	src.blockSize, src.blockRef, src.locked = d.GetStatus().GetSizeBytes(), src.diskID, true
	return diskArch(d.GetStatus().GetSourceImageCpuArchitecture()), nil
}

// unlockSource releases the disk lock taken by lockDisk, if any.
func (s *ImageService) unlockSource(ctx context.Context, imageID string, src *imageSource) error {
	if !src.locked {
		return nil
	}
	if err := s.locks.UnlockForImage(ctx, src.diskID, imageID); err != nil {
		return fmt.Errorf("unlock disk %s: %w", src.diskID, err)
	}
	return nil
}

// resolveSnapshot sizes the image after the snapshot's source disk when it
// still exists (a disk made from the image must be at least that big), else
// after the snapshot's content.
func (s *ImageService) resolveSnapshot(ctx context.Context, parentID string, src *imageSource) (computepb.ImageSpec_CPUArchitecture, error) {
	snap, err := s.disks.Snapshot(ctx, src.snapshotID)
	if err != nil {
		return 0, fmt.Errorf("source disk snapshot %s: %w", src.snapshotID, err)
	}
	if err := checkBlockSource("spec.source_disk_snapshot_id", src.snapshotID, snap.GetMetadata().GetParentId(), parentID); err != nil {
		return 0, err
	}
	if st := snap.GetStatus().GetState(); st != computepb.DiskSnapshotStatus_READY {
		return 0, errs.BadState(src.snapshotID, fmt.Sprintf("source disk snapshot is %s, not READY", st))
	}
	src.blockSize, src.blockRef = snap.GetStatus().GetContentSizeBytes(), src.snapshotID
	if diskID := snap.GetSpec().GetSourceDiskId(); diskID != "" {
		if d, derr := s.disks.Disk(ctx, diskID); derr == nil {
			src.blockSize = max(src.blockSize, d.GetStatus().GetSizeBytes())
		} else if !errs.IsKind(derr, errs.KindNotFound) {
			return 0, fmt.Errorf("source disk %s of snapshot %s: %w", diskID, src.snapshotID, derr)
		}
	}
	return snapshotArch(snap.GetStatus().GetSourceCpuArchitecture()), nil
}

// checkBlockSource requires the disk or snapshot to be in the image's project.
func checkBlockSource(field, id, sourceParent, imageParent string) error {
	if sourceParent != imageParent {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("%s is not in project %s", id, imageParent)})
	}
	return nil
}

// diskArch maps a disk's source-image CPU architecture to the image enum.
func diskArch(a computepb.DiskStatus_SourceImageCPUArchitecture) computepb.ImageSpec_CPUArchitecture {
	switch a {
	case computepb.DiskStatus_AMD64:
		return computepb.ImageSpec_AMD64
	case computepb.DiskStatus_ARM64:
		return computepb.ImageSpec_ARM64
	case computepb.DiskStatus_SOURCE_IMAGE_CPU_UNSPECIFIED:
		return computepb.ImageSpec_UNSPECIFIED
	}
	return computepb.ImageSpec_UNSPECIFIED
}

// snapshotArch maps a snapshot's CPU architecture to the image enum.
func snapshotArch(a computepb.DiskSnapshotStatus_CPUArchitecture) computepb.ImageSpec_CPUArchitecture {
	switch a {
	case computepb.DiskSnapshotStatus_AMD64:
		return computepb.ImageSpec_AMD64
	case computepb.DiskSnapshotStatus_ARM64:
		return computepb.ImageSpec_ARM64
	case computepb.DiskSnapshotStatus_UNDEFINED:
		return computepb.ImageSpec_UNSPECIFIED
	}
	return computepb.ImageSpec_UNSPECIFIED
}

// importSource runs the operation's Work step for any source.
// The disk lock is released when the image leaves CREATING, READY or ERROR.
func (s *ImageService) importSource(ctx context.Context, id string, src *imageSource) error {
	if src.storage != nil {
		return s.importPayload(ctx, id, src.storage)
	}
	err := s.importBlock(ctx, id, src)
	return errors.Join(err, s.unlockSource(ctx, id, src))
}

// importBlock writes the image of a disk or snapshot into the image store: a
// synthetic qcow2 declaring the device size, so min_disk_size_bytes is the
// device size (rounded up to a GiB) and storage_size_bytes the bytes stored.
func (s *ImageService) importBlock(ctx context.Context, id string, src *imageSource) error {
	payload := SyntheticQCOW2(max(src.blockSize, 1), blockPayloadSize, src.blockRef)
	insp := NewInspector()
	_, _ = insp.Write(payload) // never fails
	in := insp.Result()
	meta := blobMetadata(id, in)
	meta[MetaSource] = src.blockRef
	if _, err := s.blobs.PutImage(ctx, id, bytes.NewReader(payload), in.Size, meta); err != nil {
		err = fmt.Errorf("store payload of %s: %w", id, err)
		if serr := s.setState(ctx, id, computepb.ImageStatus_ERROR, err.Error()); serr != nil {
			return fmt.Errorf("%w (and mark ERROR: %w)", err, serr)
		}
		return err
	}
	return s.markReady(ctx, id, in)
}
