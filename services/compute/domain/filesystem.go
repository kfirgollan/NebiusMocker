package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// FilesystemDeps are the dependencies of FilesystemService. All are required.
type FilesystemDeps struct {
	Filesystems FilesystemRepository
	// OpIndex records filesystem operations per project (ListOperationsByParent).
	OpIndex  OperationIndex
	Data     FilesystemData
	Projects Projects
	Ops      Operations
	IDs      IDGenerator
	Clock    clock.Clock
}

func (d FilesystemDeps) validate() error {
	if d.Filesystems == nil || d.OpIndex == nil || d.Data == nil || d.Projects == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return errors.New("compute: filesystem service dependencies must not be nil")
	}
	return nil
}

// FilesystemService implements nebius.compute.v1.FilesystemService behavior.
// Files live in an object-store bucket per filesystem (FilesystemData).
type FilesystemService struct{ d FilesystemDeps }

// NewFilesystemService wires a FilesystemService.
func NewFilesystemService(d FilesystemDeps) (*FilesystemService, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &FilesystemService{d: d}, nil
}

// CreateFilesystemCmd is the input of FilesystemService.Create.
type CreateFilesystemCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *computepb.FilesystemSpec
	Request  proto.Message // echoed in Operation.request
}

// UpdateFilesystemCmd is the input of FilesystemService.Update (full replace guarded by a reset mask).
type UpdateFilesystemCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *computepb.FilesystemSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a filesystem by ID.
func (s *FilesystemService) Get(ctx context.Context, id string) (*computepb.Filesystem, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return s.d.Filesystems.Get(ctx, id)
}

// GetByName returns a filesystem by its name within a project.
func (s *FilesystemService) GetByName(ctx context.Context, parentID, name string) (*computepb.Filesystem, error) {
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	return s.d.Filesystems.GetByName(ctx, parentID, name)
}

// List returns one page of a project's filesystems in creation order. The
// upstream filter field is deprecated and "not implemented", so a non-empty
// filter is rejected rather than ignored.
func (s *FilesystemService) List(ctx context.Context, parentID string, size int64, token, filter string) (FilesystemPage, error) {
	if parentID == "" {
		return FilesystemPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	if filter != "" {
		return FilesystemPage{}, errs.BadRequest(errs.Violation{
			Field: "filter", Message: "filter is not supported (deprecated upstream); filter on the client side",
		})
	}
	n, err := pageSize(size)
	if err != nil {
		return FilesystemPage{}, err
	}
	return s.d.Filesystems.List(ctx, parentID, n, token)
}

// Create validates the request, stores the filesystem as CREATING and makes
// it READY once its bucket exists. A failed provisioning leaves it in ERROR.
func (s *FilesystemService) Create(ctx context.Context, cmd CreateFilesystemCmd) (*operations.Operation, error) {
	fs, err := s.newFilesystem(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id := fs.GetMetadata().GetId()
	return s.submit(ctx, fs.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Create filesystem",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.d.Filesystems.Create(ctx, fs) },
		Work: func(ctx context.Context) error {
			if err := s.d.Data.Provision(ctx, id); err != nil {
				err = fmt.Errorf("provision filesystem %s: %w", id, err)
				_, merr := s.d.Filesystems.Mutate(ctx, id, func(f *computepb.Filesystem) error {
					setFilesystemState(f, computepb.FilesystemStatus_ERROR)
					f.Status.StateDescription = err.Error()
					return nil
				})
				return errors.Join(err, merr)
			}
			_, err := s.d.Filesystems.Mutate(ctx, id, func(f *computepb.Filesystem) error {
				setFilesystemState(f, computepb.FilesystemStatus_READY)
				return nil
			})
			return err
		},
	})
}

// Update applies a full-replace update with reset-mask semantics. Labels,
// size (grow only) and forbid_deletion may change; the name, type and block
// size are immutable.
func (s *FilesystemService) Update(ctx context.Context, cmd UpdateFilesystemCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	stored, err := s.d.Filesystems.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Update filesystem",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
		Work: func(ctx context.Context) error {
			_, err := s.d.Filesystems.Mutate(ctx, id, func(f *computepb.Filesystem) error {
				if f.GetStatus().GetState() != computepb.FilesystemStatus_UPDATING {
					return nil
				}
				size, err := FilesystemSizeBytes(f.GetSpec())
				if err != nil {
					return err
				}
				f.Status.SizeBytes = size // hot resize: the bucket has no size to change
				setFilesystemState(f, computepb.FilesystemStatus_READY)
				return nil
			})
			return err
		},
	})
}

// Delete marks the filesystem DELETING and purges its files when the
// operation completes. It fails with FAILED_PRECONDITION while deletion is
// forbidden, instances use it or Object Storage filesystem buckets expose it.
func (s *FilesystemService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	stored, err := s.d.Filesystems.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Delete filesystem",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			// The bucket check and the state change share one transaction
			// with Expose's check, so a concurrent bucket create cannot slip in.
			_, err := s.d.Filesystems.MutateExposed(ctx, id, func(f *computepb.Filesystem, buckets []string) error {
				if len(buckets) > 0 {
					return errs.BadState(id, fmt.Sprintf(
						"filesystem is exposed by Object Storage filesystem buckets [%s]: delete them first", strings.Join(buckets, ", ")))
				}
				if err := CheckFilesystemDeletable(f); err != nil {
					return err
				}
				setFilesystemState(f, computepb.FilesystemStatus_DELETING)
				return nil
			})
			return err
		},
		Work: func(ctx context.Context) error {
			if err := s.d.Data.Purge(ctx, id); err != nil {
				return fmt.Errorf("purge filesystem %s: %w", id, err)
			}
			return s.d.Filesystems.Delete(ctx, id)
		},
	})
}

// ListOperationsByParent returns the operations on filesystems of a project,
// newest first, including operations on filesystems deleted since.
func (s *FilesystemService) ListOperationsByParent(ctx context.Context, parentID string, size int64, token string) (OperationPage, error) {
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

func (s *FilesystemService) submit(ctx context.Context, parentID string, spec operations.Spec) (*operations.Operation, error) {
	op, err := s.d.Ops.Submit(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := s.d.OpIndex.Add(ctx, parentID, op.ID); err != nil {
		return nil, fmt.Errorf("index operation %s: %w", op.ID, err)
	}
	return op, nil
}

// newFilesystem validates a Create request and builds the CREATING filesystem.
func (s *FilesystemService) newFilesystem(ctx context.Context, cmd CreateFilesystemCmd) (*computepb.Filesystem, error) {
	md, spec := cmd.Metadata, cmd.Spec
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if _, err := s.d.Projects.Parent(ctx, parentID); err != nil {
		return nil, fmt.Errorf("filesystem parent %s: %w", parentID, err)
	}
	if err := checkFilesystemType(spec.GetType()); err != nil {
		return nil, err
	}
	blockSize, err := EffectiveBlockSize(spec.GetBlockSizeBytes())
	if err != nil {
		return nil, err
	}
	size, err := FilesystemSizeBytes(spec)
	if err != nil {
		return nil, err
	}
	if err := checkFilesystemSize(size); err != nil {
		return nil, err
	}
	id, err := s.d.IDs.New(KindFilesystem)
	if err != nil {
		return nil, fmt.Errorf("new filesystem id: %w", err)
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &computepb.Filesystem{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec: proto.CloneOf(spec), // echoed as sent: defaults live in status
		Status: &computepb.FilesystemStatus{
			State: computepb.FilesystemStatus_CREATING, Reconciling: true, SizeBytes: size, BlockSizeBytes: blockSize,
		},
	}, nil
}

func (s *FilesystemService) applyUpdate(ctx context.Context, cmd UpdateFilesystemCmd) error {
	_, err := s.d.Filesystems.Mutate(ctx, cmd.Metadata.GetId(), func(stored *computepb.Filesystem) error {
		updated, err := s.patchFilesystem(stored, cmd)
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

// patchFilesystem returns stored with the update applied, or stored itself when nothing changed.
func (s *FilesystemService) patchFilesystem(stored *computepb.Filesystem, cmd UpdateFilesystemCmd) (*computepb.Filesystem, error) {
	if err := checkMetadataUpdatable(stored.GetMetadata(), cmd.Metadata); err != nil {
		return nil, err
	}
	if n := cmd.Metadata.GetName(); n != "" && n != stored.GetMetadata().GetName() {
		return nil, errs.BadRequest(errs.Violation{
			Field: "metadata.name", Message: "field is immutable: a filesystem cannot be renamed",
		})
	}
	id := stored.GetMetadata().GetId()
	if st := stored.GetStatus().GetState(); st != computepb.FilesystemStatus_READY {
		return nil, errs.BadState(id, fmt.Sprintf("filesystem is %s: only READY filesystems can be updated", st))
	}
	updated, err := patchResource(stored, &computepb.Filesystem{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return nil, err
	}
	updated.Metadata.Name = stored.GetMetadata().GetName() // a reset mask cannot clear the name
	if updated.GetSpec() == nil {
		updated.Spec = &computepb.FilesystemSpec{}
	}
	if err := keepFilesystemImmutable(stored.GetSpec(), updated.GetSpec()); err != nil {
		return nil, err
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return stored, nil // nothing changed: keep resource_version and updated_at
	}
	size, err := FilesystemSizeBytes(updated.GetSpec())
	if err != nil {
		return nil, err
	}
	current := stored.GetStatus().GetSizeBytes()
	if size < current {
		return nil, errs.BadRequest(errs.Violation{
			Field: "spec.size", Message: fmt.Sprintf("filesystem size cannot be reduced: %d bytes is less than the current %d bytes", size, current),
		})
	}
	if err := checkFilesystemSize(size); err != nil {
		return nil, err
	}
	if size != current {
		setFilesystemState(updated, computepb.FilesystemStatus_UPDATING)
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.d.Clock.Now())
	return updated, nil
}

// keepFilesystemImmutable restores unset immutable fields of next from prev
// and fails if one was changed. The size oneof is mutable, but an update that
// leaves it unset keeps the current size.
func keepFilesystemImmutable(prev, next *computepb.FilesystemSpec) error {
	var v []errs.Violation
	if next.GetSize() == nil {
		next.Size = prev.GetSize()
	}
	switch {
	case next.GetType() == computepb.FilesystemSpec_UNSPECIFIED:
		next.Type = prev.GetType()
	case next.GetType() != prev.GetType():
		v = append(v, errs.Violation{Field: "spec.type", Message: "field is immutable"})
	}
	switch {
	case next.GetBlockSizeBytes() == 0:
		next.BlockSizeBytes = prev.GetBlockSizeBytes()
	case next.GetBlockSizeBytes() != prev.GetBlockSizeBytes():
		v = append(v, errs.Violation{Field: "spec.block_size_bytes", Message: "field is immutable"})
	}
	if len(v) > 0 {
		return errs.BadRequest(v...)
	}
	return nil
}

func setFilesystemState(f *computepb.Filesystem, state computepb.FilesystemStatus_State) {
	if f.GetStatus() == nil {
		f.Status = &computepb.FilesystemStatus{}
	}
	f.Status.State = state
	f.Status.StateDescription = ""
	f.Status.Reconciling = state == computepb.FilesystemStatus_CREATING || state == computepb.FilesystemStatus_UPDATING ||
		state == computepb.FilesystemStatus_DELETING
}
