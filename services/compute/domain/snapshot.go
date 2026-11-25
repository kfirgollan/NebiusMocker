package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const maxSnapshotDescription = 1024

// SnapshotService implements nebius.compute.v1.DiskSnapshotService behavior.
// Snapshots record their source disk's size and CPU architecture; no data is
// copied.
type SnapshotService struct{ d DiskDeps }

// NewSnapshotService wires a SnapshotService.
func NewSnapshotService(d DiskDeps) (*SnapshotService, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &SnapshotService{d: d}, nil
}

// CreateSnapshotCmd is the input of SnapshotService.Create.
type CreateSnapshotCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *computepb.DiskSnapshotSpec
	Request  proto.Message
}

// UpdateSnapshotCmd is the input of SnapshotService.Update.
type UpdateSnapshotCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *computepb.DiskSnapshotSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a snapshot by ID.
func (s *SnapshotService) Get(ctx context.Context, id string) (*computepb.DiskSnapshot, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return s.d.Snapshots.Get(ctx, id)
}

// GetByName returns a snapshot by its name within a project.
func (s *SnapshotService) GetByName(ctx context.Context, parentID, name string) (*computepb.DiskSnapshot, error) {
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	return s.d.Snapshots.GetByName(ctx, parentID, name)
}

// List returns one page of a project's snapshots in creation order.
func (s *SnapshotService) List(ctx context.Context, parentID string, size int64, token string) (SnapshotPage, error) {
	if parentID == "" {
		return SnapshotPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return SnapshotPage{}, err
	}
	return s.d.Snapshots.List(ctx, parentID, n, token)
}

// ListByDisk returns one page of the snapshots taken of an existing disk.
func (s *SnapshotService) ListByDisk(ctx context.Context, diskID string, size int64, token string) (SnapshotPage, error) {
	if diskID == "" {
		return SnapshotPage{}, errs.BadRequest(errs.Violation{Field: "disk_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return SnapshotPage{}, err
	}
	disk, err := s.d.Disks.Get(ctx, diskID)
	if err != nil {
		return SnapshotPage{}, err
	}
	return s.d.Snapshots.ListByDisk(ctx, disk.GetMetadata().GetParentId(), diskID, n, token)
}

// Create checks that the source disk exists in the snapshot's project and is
// READY, locks the disk against deletion while the snapshot is CREATING, and
// records the disk's size and CPU architecture in the snapshot status.
func (s *SnapshotService) Create(ctx context.Context, cmd CreateSnapshotCmd) (*operations.Operation, error) {
	snap, err := s.newSnapshot(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id, diskID := snap.GetMetadata().GetId(), snap.GetSpec().GetSourceDiskId()
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create disk snapshot",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.prepareCreate(ctx, snap) },
		Work: func(ctx context.Context) error {
			if _, err := s.d.Snapshots.Mutate(ctx, id, func(sn *computepb.DiskSnapshot) error {
				sn.Status.State = computepb.DiskSnapshotStatus_READY
				return nil
			}); err != nil {
				return err
			}
			return s.unlockDisk(ctx, diskID, id)
		},
	})
}

// Update changes name, labels and description; source_disk_id is immutable.
func (s *SnapshotService) Update(ctx context.Context, cmd UpdateSnapshotCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	if _, err := s.d.Snapshots.Get(ctx, id); err != nil {
		return nil, err
	}
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update disk snapshot",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			_, err := s.d.Snapshots.Mutate(ctx, id, func(stored *computepb.DiskSnapshot) error {
				updated, err := s.patchSnapshot(stored, cmd)
				if err != nil {
					return err
				}
				if updated != stored {
					proto.Reset(stored)
					proto.Merge(stored, updated)
				}
				return nil
			})
			return err
		},
	})
}

// Delete marks the snapshot DELETING and removes it when the operation
// completes. It fails with FAILED_PRECONDITION while disks are being created
// from the snapshot.
func (s *SnapshotService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	if _, err := s.d.Snapshots.Get(ctx, id); err != nil {
		return nil, err
	}
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete disk snapshot",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.d.Snapshots.Mutate(ctx, id, func(sn *computepb.DiskSnapshot) error {
				if disks := sn.GetStatus().GetLockState().GetDisks(); len(disks) > 0 {
					return errs.BadState(id, fmt.Sprintf("snapshot is locked while disks are created from it: %v", disks))
				}
				sn.Status.State = computepb.DiskSnapshotStatus_DELETING
				return nil
			})
			return err
		},
		Work: func(ctx context.Context) error { return s.d.Snapshots.Delete(ctx, id) },
	})
}

func (s *SnapshotService) newSnapshot(ctx context.Context, cmd CreateSnapshotCmd) (*computepb.DiskSnapshot, error) {
	md, spec := cmd.Metadata, cmd.Spec
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	var v []errs.Violation
	if parentID == "" {
		v = append(v, errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if spec.GetSourceDiskId() == "" {
		v = append(v, errs.Violation{Field: "spec.source_disk_id", Message: "required"})
	}
	if err := checkDescription(spec.GetDescription()); err != nil {
		v = append(v, *err)
	}
	if len(v) > 0 {
		return nil, errs.BadRequest(v...)
	}
	if _, err := s.d.Projects.Parent(ctx, parentID); err != nil {
		return nil, fmt.Errorf("snapshot parent %s: %w", parentID, err)
	}
	disk, err := s.d.Disks.Get(ctx, spec.GetSourceDiskId())
	if err != nil {
		return nil, fmt.Errorf("source disk: %w", err)
	}
	if err := checkSnapshotSource(disk, parentID); err != nil {
		return nil, err
	}
	id, err := s.d.IDs.New(KindDiskSnapshot)
	if err != nil {
		return nil, fmt.Errorf("new snapshot id: %w", err)
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &computepb.DiskSnapshot{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   proto.CloneOf(spec),
		Status: snapshotStatusOf(disk),
	}, nil
}

// checkSnapshotSource checks that disk can be snapshotted into parentID: the
// snapshot lives in the disk's project, and the disk must be READY.
func checkSnapshotSource(disk *computepb.Disk, parentID string) error {
	id := disk.GetMetadata().GetId()
	if disk.GetMetadata().GetParentId() != parentID {
		return errs.BadRequest(errs.Violation{
			Field:   "metadata.parent_id",
			Message: fmt.Sprintf("must be the project of the source disk (%s)", disk.GetMetadata().GetParentId()),
		})
	}
	if st := disk.GetStatus().GetState(); st != computepb.DiskStatus_READY {
		return errs.BadState(id, fmt.Sprintf("disk %s is %s, not READY", id, st))
	}
	return nil
}

func snapshotStatusOf(disk *computepb.Disk) *computepb.DiskSnapshotStatus {
	size := disk.GetStatus().GetSizeBytes()
	arch := computepb.DiskSnapshotStatus_UNDEFINED
	switch disk.GetStatus().GetSourceImageCpuArchitecture() {
	case computepb.DiskStatus_AMD64:
		arch = computepb.DiskSnapshotStatus_AMD64
	case computepb.DiskStatus_ARM64:
		arch = computepb.DiskSnapshotStatus_ARM64
	case computepb.DiskStatus_SOURCE_IMAGE_CPU_UNSPECIFIED:
	}
	return &computepb.DiskSnapshotStatus{
		State:            computepb.DiskSnapshotStatus_CREATING,
		ContentSizeBytes: size,
		// Equal to the content size: incremental snapshots are not supported (proto).
		StorageSizeBytes:      size,
		SourceCpuArchitecture: arch,
	}
}

func checkDescription(d string) *errs.Violation {
	if n := utf8.RuneCountInString(d); n > maxSnapshotDescription {
		return &errs.Violation{Field: "spec.description", Message: fmt.Sprintf("must be at most %d characters, got %d", maxSnapshotDescription, n)}
	}
	return nil
}

// prepareCreate runs under the snapshot's operation lock: it re-checks the
// disk and locks it against deletion in one transaction, then stores the
// CREATING snapshot.
func (s *SnapshotService) prepareCreate(ctx context.Context, snap *computepb.DiskSnapshot) error {
	id, diskID := snap.GetMetadata().GetId(), snap.GetSpec().GetSourceDiskId()
	disk, err := s.d.Disks.Mutate(ctx, diskID, func(d *computepb.Disk) error {
		if err := checkSnapshotSource(d, snap.GetMetadata().GetParentId()); err != nil {
			return err
		}
		if d.GetStatus().GetLockState() == nil {
			d.Status.LockState = &computepb.DiskStatus_LockState{}
		}
		d.Status.LockState.Snapshots = append(d.Status.LockState.Snapshots, id)
		return nil
	})
	if err != nil {
		return fmt.Errorf("source disk: %w", err)
	}
	// The disk may have grown since the request was validated.
	snap.Status = snapshotStatusOf(disk)
	if err := s.d.Snapshots.Create(ctx, snap); err != nil {
		return errors.Join(err, s.unlockDisk(ctx, diskID, id))
	}
	return nil
}

// unlockDisk removes snapshotID from the disk's lock. A missing disk is not an error.
func (s *SnapshotService) unlockDisk(ctx context.Context, diskID, snapshotID string) error {
	_, err := s.d.Disks.Mutate(ctx, diskID, func(d *computepb.Disk) error {
		if ls := d.GetStatus().GetLockState(); ls != nil {
			ls.Snapshots = slices.DeleteFunc(ls.GetSnapshots(), func(x string) bool { return x == snapshotID })
		}
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}

func (s *SnapshotService) patchSnapshot(stored *computepb.DiskSnapshot, cmd UpdateSnapshotCmd) (*computepb.DiskSnapshot, error) {
	if err := checkMetadataUpdatable(stored.GetMetadata(), cmd.Metadata); err != nil {
		return nil, err
	}
	id := stored.GetMetadata().GetId()
	if st := stored.GetStatus().GetState(); st != computepb.DiskSnapshotStatus_READY {
		return nil, errs.BadState(id, fmt.Sprintf("snapshot is %s: only READY snapshots can be updated", st))
	}
	updated, err := patchResource(stored, &computepb.DiskSnapshot{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return nil, err
	}
	if updated.GetSpec() == nil {
		updated.Spec = &computepb.DiskSnapshotSpec{}
	}
	switch src := updated.GetSpec().GetSourceDiskId(); {
	case src == "":
		updated.Spec.SourceDiskId = stored.GetSpec().GetSourceDiskId()
	case src != stored.GetSpec().GetSourceDiskId():
		return nil, errs.BadRequest(errs.Violation{Field: "spec.source_disk_id", Message: "field is immutable"})
	}
	if err := checkDescription(updated.GetSpec().GetDescription()); err != nil {
		return nil, errs.BadRequest(*err)
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return stored, nil
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.d.Clock.Now())
	return updated, nil
}
