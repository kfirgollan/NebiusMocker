package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// DiskDeps are the dependencies of DiskService and SnapshotService. All are required.
type DiskDeps struct {
	Disks     DiskRepository
	Snapshots SnapshotRepository
	Images    ImageLookup
	// OpIndex records disk operations per project (ListOperationsByParent).
	OpIndex  OperationIndex
	Projects Projects
	Ops      Operations
	IDs      IDGenerator
	Clock    clock.Clock
}

func (d DiskDeps) validate() error {
	if d.Disks == nil || d.Snapshots == nil || d.Images == nil || d.OpIndex == nil || d.Projects == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return errors.New("compute: disk service dependencies must not be nil")
	}
	return nil
}

// DiskService implements nebius.compute.v1.DiskService behavior. It is a
// control plane only: no storage is allocated and nothing is attached.
type DiskService struct{ d DiskDeps }

// NewDiskService wires a DiskService.
func NewDiskService(d DiskDeps) (*DiskService, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &DiskService{d: d}, nil
}

// CreateDiskCmd is the input of DiskService.Create.
type CreateDiskCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *computepb.DiskSpec
	Request  proto.Message // echoed in Operation.request
}

// UpdateDiskCmd is the input of DiskService.Update (full replace guarded by a reset mask).
type UpdateDiskCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *computepb.DiskSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a disk by ID.
func (s *DiskService) Get(ctx context.Context, id string) (*computepb.Disk, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return s.d.Disks.Get(ctx, id)
}

// GetByName returns a disk by its name within a project.
func (s *DiskService) GetByName(ctx context.Context, parentID, name string) (*computepb.Disk, error) {
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	return s.d.Disks.GetByName(ctx, parentID, name)
}

// List returns one page of a project's disks in creation order. The upstream
// filter field is deprecated and "not implemented", so a non-empty filter is
// rejected rather than ignored.
func (s *DiskService) List(ctx context.Context, parentID string, size int64, token, filter string) (DiskPage, error) {
	if parentID == "" {
		return DiskPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	if filter != "" {
		return DiskPage{}, errs.BadRequest(errs.Violation{
			Field: "filter", Message: "filter is not supported (deprecated upstream); filter on the client side",
		})
	}
	n, err := pageSize(size)
	if err != nil {
		return DiskPage{}, err
	}
	return s.d.Disks.List(ctx, parentID, n, token)
}

// diskSource is the resolved source of a new disk.
type diskSource struct {
	imageID    string
	snapshotID string
	minSize    int64
	arch       computepb.DiskStatus_SourceImageCPUArchitecture
	boot       bool // created from an image: boot-disk limits apply
}

// Create validates the request, stores the disk as CREATING and makes it
// READY when the operation completes. A snapshot source is locked against
// deletion while the disk is being created.
func (s *DiskService) Create(ctx context.Context, cmd CreateDiskCmd) (*operations.Operation, error) {
	disk, src, err := s.newDisk(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id := disk.GetMetadata().GetId()
	return s.submit(ctx, disk.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Create disk",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.prepareCreate(ctx, disk, src) },
		Work: func(ctx context.Context) error {
			if _, err := s.d.Disks.Mutate(ctx, id, func(d *computepb.Disk) error {
				setDiskState(d, computepb.DiskStatus_READY)
				return nil
			}); err != nil {
				return err
			}
			return s.unlockSnapshot(ctx, src.snapshotID, id)
		},
	})
}

// Update applies a full-replace update with reset-mask semantics. Name,
// labels, size (grow only) and forbid_deletion may change; type, block size,
// source and encryption are immutable.
func (s *DiskService) Update(ctx context.Context, cmd UpdateDiskCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	stored, err := s.d.Disks.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Update disk",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
		Work: func(ctx context.Context) error {
			_, err := s.d.Disks.Mutate(ctx, id, func(d *computepb.Disk) error {
				if d.GetStatus().GetState() != computepb.DiskStatus_UPDATING {
					return nil
				}
				size, err := SizeBytes(d.GetSpec())
				if err != nil {
					return err
				}
				d.Status.SizeBytes = size
				setDiskState(d, computepb.DiskStatus_READY)
				return nil
			})
			return err
		},
	})
}

// Delete marks the disk DELETING and removes it when the operation completes.
// It fails with FAILED_PRECONDITION while the disk is protected, attached,
// managed by an instance or locked by an image or snapshot being created.
// Snapshots of the disk are kept: their source_disk_id "may become stale if
// the disk is deleted" (DiskSnapshotSpec).
func (s *DiskService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	stored, err := s.d.Disks.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Delete disk",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.d.Disks.Mutate(ctx, id, func(d *computepb.Disk) error {
				if err := CheckDiskDeletable(d); err != nil {
					return err
				}
				setDiskState(d, computepb.DiskStatus_DELETING)
				return nil
			})
			return err
		},
		Work: func(ctx context.Context) error { return s.d.Disks.Delete(ctx, id) },
	})
}

// CheckDiskDeletable reports why a disk cannot be deleted, or nil.
func CheckDiskDeletable(d *computepb.Disk) error {
	id, st := d.GetMetadata().GetId(), d.GetStatus()
	switch {
	case d.GetSpec().GetForbidDeletion():
		return errs.BadState(id, "disk cannot be deleted because forbid_deletion is set")
	case st.GetReadWriteAttachment() != "" || len(st.GetReadOnlyAttachments()) > 0:
		return errs.BadState(id, "disk is attached to an instance: detach it before deleting")
	case st.GetManagedBy() != "":
		return errs.BadState(id, fmt.Sprintf("disk is managed by instance %s and is deleted with it", st.GetManagedBy()))
	case len(st.GetLockState().GetImages()) > 0:
		return errs.BadState(id, fmt.Sprintf("disk is locked while images are created from it: %v", st.GetLockState().GetImages()))
	case len(st.GetLockState().GetSnapshots()) > 0:
		return errs.BadState(id, fmt.Sprintf("disk is locked while snapshots are created from it: %v", st.GetLockState().GetSnapshots()))
	}
	return nil
}

// ListOperationsByParent returns the operations on disks of a project,
// newest first, including operations on disks deleted since.
func (s *DiskService) ListOperationsByParent(ctx context.Context, parentID string, size int64, token string) (OperationPage, error) {
	if parentID == "" {
		return OperationPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return OperationPage{}, err
	}
	offset, err := decodeOffset(token)
	if err != nil {
		return OperationPage{}, err
	}
	ids, err := s.d.OpIndex.IDs(ctx, parentID)
	if err != nil {
		return OperationPage{}, err
	}
	slices.Reverse(ids)
	var page OperationPage
	for i := offset; i < len(ids) && len(page.Items) < n; i++ {
		op, err := s.d.Ops.Get(ctx, ids[i])
		if err != nil {
			return OperationPage{}, err
		}
		page.Items = append(page.Items, op)
	}
	if next := offset + n; next < len(ids) {
		page.NextPageToken = encodeOffset(next)
	}
	return page, nil
}

func (s *DiskService) submit(ctx context.Context, parentID string, spec operations.Spec) (*operations.Operation, error) {
	op, err := s.d.Ops.Submit(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := s.d.OpIndex.Add(ctx, parentID, op.ID); err != nil {
		return nil, fmt.Errorf("index operation %s: %w", op.ID, err)
	}
	return op, nil
}

// newDisk validates a Create request and builds the CREATING disk.
func (s *DiskService) newDisk(ctx context.Context, cmd CreateDiskCmd) (*computepb.Disk, diskSource, error) {
	md, spec := cmd.Metadata, cmd.Spec
	if md.GetId() != "" {
		return nil, diskSource{}, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, diskSource{}, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if _, err := s.d.Projects.Parent(ctx, parentID); err != nil {
		return nil, diskSource{}, fmt.Errorf("disk parent %s: %w", parentID, err)
	}
	if err := checkType(spec); err != nil {
		return nil, diskSource{}, err
	}
	blockSize, err := EffectiveBlockSize(spec.GetBlockSizeBytes())
	if err != nil {
		return nil, diskSource{}, err
	}
	size, err := SizeBytes(spec)
	if err != nil {
		return nil, diskSource{}, err
	}
	src, err := s.resolveSource(ctx, parentID, spec)
	if err != nil {
		return nil, diskSource{}, err
	}
	if err := checkSize(spec.GetType(), blockSize, size, src.boot); err != nil {
		return nil, diskSource{}, err
	}
	if size < src.minSize {
		return nil, diskSource{}, errs.BadRequest(errs.Violation{
			Field: "spec.size", Message: fmt.Sprintf("disk size %d bytes is smaller than its source (%d bytes)", size, src.minSize),
		})
	}
	id, err := s.d.IDs.New(KindDisk)
	if err != nil {
		return nil, diskSource{}, fmt.Errorf("new disk id: %w", err)
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &computepb.Disk{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec: proto.CloneOf(spec), // echoed as sent: defaults live in status
		Status: &computepb.DiskStatus{
			State: computepb.DiskStatus_CREATING, Reconciling: true,
			SizeBytes: size, BlockSizeBytes: blockSize,
			SourceImageId: src.imageID, SourceImageCpuArchitecture: src.arch,
		},
	}, src, nil
}

// resolveSource checks that the image, image family or snapshot exists and
// can back a disk in parentID.
func (s *DiskService) resolveSource(ctx context.Context, parentID string, spec *computepb.DiskSpec) (diskSource, error) {
	switch src := spec.GetSource().(type) {
	case nil:
		return diskSource{}, nil // an empty additional disk
	case *computepb.DiskSpec_SourceImageId:
		if src.SourceImageId == "" {
			return diskSource{}, errs.BadRequest(errs.Violation{Field: "spec.source_image_id", Message: "must not be empty"})
		}
		img, err := s.d.Images.Get(ctx, src.SourceImageId)
		if err != nil {
			return diskSource{}, fmt.Errorf("source image: %w", err)
		}
		return s.imageSource(img, parentID, "spec.source_image_id")
	case *computepb.DiskSpec_SourceImageFamily:
		family := src.SourceImageFamily
		if family.GetImageFamily() == "" {
			return diskSource{}, errs.BadRequest(errs.Violation{Field: "spec.source_image_family.image_family", Message: "required"})
		}
		img, err := s.d.Images.GetLatestByFamily(ctx, family.GetImageFamily(), family.GetParentId())
		if err != nil {
			return diskSource{}, fmt.Errorf("source image family: %w", err)
		}
		if dep := img.GetStatus().GetImageFamilyDeprecation(); dep != nil && dep.GetDeprecatedAt() != nil &&
			!s.d.Clock.Now().Before(dep.GetDeprecatedAt().AsTime()) {
			return diskSource{}, errs.BadState(img.GetMetadata().GetId(), fmt.Sprintf(
				"image family %q is deprecated: disks cannot be created from it", family.GetImageFamily()))
		}
		return s.imageSource(img, parentID, "spec.source_image_family")
	case *computepb.DiskSpec_SourceSnapshotId:
		snap, err := s.d.Snapshots.Get(ctx, src.SourceSnapshotId)
		if err != nil {
			return diskSource{}, fmt.Errorf("source snapshot: %w", err)
		}
		if err := checkSnapshotUsable(snap, parentID); err != nil {
			return diskSource{}, err
		}
		return diskSource{
			snapshotID: snap.GetMetadata().GetId(),
			minSize:    snap.GetStatus().GetContentSizeBytes(),
			arch:       diskArchFromSnapshot(snap.GetStatus().GetSourceCpuArchitecture()),
		}, nil
	default:
		return diskSource{}, errs.BadRequest(errs.Violation{Field: "spec.source", Message: "unknown source"})
	}
}

func (s *DiskService) imageSource(img *computepb.Image, parentID, field string) (diskSource, error) {
	id, imgParent := img.GetMetadata().GetId(), img.GetMetadata().GetParentId()
	if imgParent != parentID && !s.d.Images.IsPublicProject(imgParent) {
		return diskSource{}, errs.BadRequest(errs.Violation{
			Field: field, Message: fmt.Sprintf("image %s is neither public nor in project %s", id, parentID),
		})
	}
	if st := img.GetStatus().GetState(); st != computepb.ImageStatus_READY {
		return diskSource{}, errs.BadState(id, fmt.Sprintf("image %s is %s, not READY", id, st))
	}
	return diskSource{
		imageID: id,
		minSize: img.GetStatus().GetMinDiskSizeBytes(),
		arch:    diskArchFromImage(img.GetSpec().GetCpuArchitecture()),
		boot:    true,
	}, nil
}

// checkSnapshotUsable checks that a disk in parentID can be created from snap:
// snapshots are usable only within their project, and only when READY.
func checkSnapshotUsable(snap *computepb.DiskSnapshot, parentID string) error {
	id := snap.GetMetadata().GetId()
	if snap.GetMetadata().GetParentId() != parentID {
		return errs.BadRequest(errs.Violation{
			Field: "spec.source_snapshot_id", Message: fmt.Sprintf("snapshot %s is not in project %s", id, parentID),
		})
	}
	if st := snap.GetStatus().GetState(); st != computepb.DiskSnapshotStatus_READY {
		return errs.BadState(id, fmt.Sprintf("snapshot %s is %s, not READY", id, st))
	}
	return nil
}

// prepareCreate runs under the disk's operation lock: it locks a snapshot
// source (re-checking it atomically), then stores the CREATING disk.
func (s *DiskService) prepareCreate(ctx context.Context, disk *computepb.Disk, src diskSource) error {
	id := disk.GetMetadata().GetId()
	if src.snapshotID != "" {
		if _, err := s.d.Snapshots.Mutate(ctx, src.snapshotID, func(snap *computepb.DiskSnapshot) error {
			if err := checkSnapshotUsable(snap, disk.GetMetadata().GetParentId()); err != nil {
				return err
			}
			if snap.GetStatus().GetLockState() == nil {
				snap.Status.LockState = &computepb.DiskSnapshotStatus_LockState{}
			}
			snap.Status.LockState.Disks = append(snap.Status.LockState.Disks, id)
			return nil
		}); err != nil {
			return fmt.Errorf("source snapshot: %w", err)
		}
	}
	if err := s.d.Disks.Create(ctx, disk); err != nil {
		return errors.Join(err, s.unlockSnapshot(ctx, src.snapshotID, id))
	}
	return nil
}

// unlockSnapshot removes diskID from the snapshot's lock. A missing snapshot
// is not an error.
func (s *DiskService) unlockSnapshot(ctx context.Context, snapshotID, diskID string) error {
	if snapshotID == "" {
		return nil
	}
	_, err := s.d.Snapshots.Mutate(ctx, snapshotID, func(snap *computepb.DiskSnapshot) error {
		if ls := snap.GetStatus().GetLockState(); ls != nil {
			ls.Disks = slices.DeleteFunc(ls.GetDisks(), func(d string) bool { return d == diskID })
		}
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}

func (s *DiskService) applyUpdate(ctx context.Context, cmd UpdateDiskCmd) error {
	_, err := s.d.Disks.Mutate(ctx, cmd.Metadata.GetId(), func(stored *computepb.Disk) error {
		updated, err := s.patchDisk(stored, cmd)
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
}

// patchDisk returns stored with the update applied, or stored itself when nothing changed.
func (s *DiskService) patchDisk(stored *computepb.Disk, cmd UpdateDiskCmd) (*computepb.Disk, error) {
	if err := checkMetadataUpdatable(stored.GetMetadata(), cmd.Metadata); err != nil {
		return nil, err
	}
	id := stored.GetMetadata().GetId()
	if st := stored.GetStatus().GetState(); st != computepb.DiskStatus_READY {
		return nil, errs.BadState(id, fmt.Sprintf("disk is %s: only READY disks can be updated", st))
	}
	if by := stored.GetStatus().GetManagedBy(); by != "" {
		// instance.proto, ManagedDisk: "Managed disks can be updated only via
		// instance spec updates. Updates via DiskService are not allowed."
		return nil, errs.BadState(id, fmt.Sprintf("disk is managed by instance %s: update it through the instance spec", by))
	}
	updated, err := patchResource(stored, &computepb.Disk{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return nil, err
	}
	if updated.GetSpec() == nil {
		updated.Spec = &computepb.DiskSpec{}
	}
	if err := keepDiskImmutable(stored.GetSpec(), updated.GetSpec()); err != nil {
		return nil, err
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return stored, nil // nothing changed: keep resource_version and updated_at
	}
	if updated.GetSpec().GetForbidDeletion() != stored.GetSpec().GetForbidDeletion() && stored.GetStatus().GetManagedBy() != "" {
		return nil, errs.BadState(id, "deletion protection can only be changed on a standalone disk")
	}
	size, err := SizeBytes(updated.GetSpec())
	if err != nil {
		return nil, err
	}
	current := stored.GetStatus().GetSizeBytes()
	if size < current {
		return nil, errs.BadRequest(errs.Violation{
			Field: "spec.size", Message: fmt.Sprintf("disk size cannot be reduced: %d bytes is less than the current %d bytes", size, current),
		})
	}
	boot := stored.GetStatus().GetSourceImageId() != ""
	if err := checkSize(updated.GetSpec().GetType(), stored.GetStatus().GetBlockSizeBytes(), size, boot); err != nil {
		return nil, err
	}
	if size != current {
		setDiskState(updated, computepb.DiskStatus_UPDATING)
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.d.Clock.Now())
	return updated, nil
}

// keepDiskImmutable restores unset immutable fields of next from prev and
// fails if one was changed. The size oneof is mutable, but an update that
// leaves it unset keeps the current size.
func keepDiskImmutable(prev, next *computepb.DiskSpec) error {
	var v []errs.Violation
	immutable := func(field string) {
		v = append(v, errs.Violation{Field: "spec." + field, Message: "field is immutable"})
	}
	if next.GetSize() == nil {
		next.Size = prev.GetSize()
	}
	switch {
	case next.GetType() == computepb.DiskSpec_UNSPECIFIED:
		next.Type = prev.GetType()
	case next.GetType() != prev.GetType():
		immutable("type")
	}
	switch {
	case next.GetBlockSizeBytes() == 0:
		next.BlockSizeBytes = prev.GetBlockSizeBytes()
	case next.GetBlockSizeBytes() != prev.GetBlockSizeBytes():
		immutable("block_size_bytes")
	}
	switch {
	case next.GetSource() == nil:
		next.Source = prev.GetSource()
	case !proto.Equal(&computepb.DiskSpec{Source: prev.GetSource()}, &computepb.DiskSpec{Source: next.GetSource()}):
		immutable("source")
	}
	switch {
	case next.GetDiskEncryption() == nil:
		next.DiskEncryption = prev.GetDiskEncryption()
	case next.GetDiskEncryption().GetType() != prev.GetDiskEncryption().GetType():
		immutable("disk_encryption")
	default:
		// An explicit but equal encryption setting keeps the stored shape.
		next.DiskEncryption = prev.GetDiskEncryption()
	}
	if len(v) > 0 {
		return errs.BadRequest(v...)
	}
	return nil
}

func setDiskState(d *computepb.Disk, state computepb.DiskStatus_State) {
	if d.GetStatus() == nil {
		d.Status = &computepb.DiskStatus{}
	}
	d.Status.State = state
	d.Status.Reconciling = state == computepb.DiskStatus_CREATING || state == computepb.DiskStatus_UPDATING ||
		state == computepb.DiskStatus_DELETING
}

func diskArchFromImage(a computepb.ImageSpec_CPUArchitecture) computepb.DiskStatus_SourceImageCPUArchitecture {
	switch a {
	case computepb.ImageSpec_AMD64:
		return computepb.DiskStatus_AMD64
	case computepb.ImageSpec_ARM64:
		return computepb.DiskStatus_ARM64
	case computepb.ImageSpec_UNSPECIFIED:
		return computepb.DiskStatus_SOURCE_IMAGE_CPU_UNSPECIFIED
	default:
		return computepb.DiskStatus_SOURCE_IMAGE_CPU_UNSPECIFIED
	}
}

func diskArchFromSnapshot(a computepb.DiskSnapshotStatus_CPUArchitecture) computepb.DiskStatus_SourceImageCPUArchitecture {
	switch a {
	case computepb.DiskSnapshotStatus_AMD64:
		return computepb.DiskStatus_AMD64
	case computepb.DiskSnapshotStatus_ARM64:
		return computepb.DiskStatus_ARM64
	case computepb.DiskSnapshotStatus_UNDEFINED: // an alias for AMD64 (proto), but the architecture is unknown
		return computepb.DiskStatus_SOURCE_IMAGE_CPU_UNSPECIFIED
	default:
		return computepb.DiskStatus_SOURCE_IMAGE_CPU_UNSPECIFIED
	}
}

// checkMetadataUpdatable enforces immutable parent_id and optimistic concurrency.
func checkMetadataUpdatable(stored, md *commonpb.ResourceMetadata) error {
	if p := md.GetParentId(); p != "" && p != stored.GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != stored.GetResourceVersion() {
		return errs.Conflict(stored.GetId(), fmt.Sprintf(
			"resource_version %d does not match current version %d", v, stored.GetResourceVersion(),
		))
	}
	return nil
}

// patchResource applies patch to a copy of stored with reset-mask semantics
// and restores the server-owned metadata fields.
func patchResource[T resourceMessage](stored, patch T, resetMask *mask.Mask) (T, error) {
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMask); err != nil {
		var zero T
		return zero, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	md, out := stored.GetMetadata(), updated.GetMetadata()
	if out == nil {
		var zero T
		return zero, errs.BadRequest(errs.Violation{Field: "metadata", Message: "cannot be reset"})
	}
	out.Id = md.GetId()
	out.ParentId = md.GetParentId()
	out.ResourceVersion = md.GetResourceVersion()
	out.CreatedAt = md.GetCreatedAt()
	out.UpdatedAt = md.GetUpdatedAt()
	return updated, nil
}

// resourceMessage is a resource proto whose metadata is always set.
type resourceMessage interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}
