package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Namespace returns the backend namespace (Harbor project) and the first path
// segment of image names for a registry: its NID without the "registry-"
// prefix, as in Nebius image names "<fqdn>/<registry-id-suffix>/<image>:<tag>".
func Namespace(registryID string) string {
	return strings.TrimPrefix(registryID, KindRegistry+"-")
}

// RegistryIDOf is the inverse of Namespace.
func RegistryIDOf(namespace string) string { return KindRegistry + "-" + namespace }

// RegistryService implements Container Registry registry behavior.
type RegistryService struct {
	repo      RegistryRepository
	index     ArtifactIndex
	backend   Backend
	hierarchy Hierarchy
	endpoint  Endpoint
	ops       Operations
	ids       IDGenerator
	clk       clock.Clock
}

// RegistryDeps are the dependencies of a RegistryService. All are required.
type RegistryDeps struct {
	Registries RegistryRepository
	Artifacts  ArtifactIndex
	Backend    Backend
	Hierarchy  Hierarchy
	Endpoint   Endpoint
	Ops        Operations
	IDs        IDGenerator
	Clock      clock.Clock
}

// NewRegistryService wires a RegistryService.
func NewRegistryService(d RegistryDeps) (*RegistryService, error) {
	if d.Registries == nil || d.Artifacts == nil || d.Backend == nil || d.Hierarchy == nil || d.Endpoint == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("registry: registry service dependencies must not be nil")
	}
	return &RegistryService{
		repo: d.Registries, index: d.Artifacts, backend: d.Backend, hierarchy: d.Hierarchy, endpoint: d.Endpoint,
		ops: d.Ops, ids: d.IDs, clk: d.Clock,
	}, nil
}

// CreateRegistryCmd is the input of Create.
type CreateRegistryCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *registrypb.RegistrySpec
	// Request is echoed in Operation.request.
	Request proto.Message
}

// UpdateRegistryCmd is the input of Update (full replace guarded by a reset mask).
type UpdateRegistryCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *registrypb.RegistrySpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// DeleteRegistryCmd is the input of Delete.
type DeleteRegistryCmd struct {
	ID string
	// Force deletes a registry that still holds images.
	Force   bool
	Request proto.Message
}

// Get returns a registry with live status (image count, FQDN).
func (s *RegistryService) Get(ctx context.Context, id string) (*registrypb.Registry, error) {
	r, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return r, s.fillStatus(ctx, r)
}

// GetByName returns a registry by its name within a project.
func (s *RegistryService) GetByName(ctx context.Context, parentID, name string) (*registrypb.Registry, error) {
	r, err := s.repo.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return r, s.fillStatus(ctx, r)
}

// List returns one page of a project's registries in creation order.
func (s *RegistryService) List(ctx context.Context, parentID string, pageSize int64, pageToken string) (RegistryPage, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return RegistryPage{}, err
	}
	page, err := s.repo.List(ctx, parentID, size, pageToken)
	if err != nil {
		return RegistryPage{}, err
	}
	for _, r := range page.Items {
		if err := s.fillStatus(ctx, r); err != nil {
			return RegistryPage{}, err
		}
	}
	return page, nil
}

// fillStatus sets the server-derived fields: the FQDN images are pushed to
// and the number of images, read live from the backend.
func (s *RegistryService) fillStatus(ctx context.Context, r *registrypb.Registry) error {
	if r.GetStatus() == nil {
		r.Status = &registrypb.RegistryStatus{}
	}
	r.Status.RegistryFqdn = s.endpoint.FQDN()
	if r.GetStatus().GetState() != registrypb.RegistryStatus_ACTIVE {
		return nil // no namespace yet
	}
	images, err := s.backend.ListImages(ctx, Namespace(r.GetMetadata().GetId()))
	if err != nil {
		return fmt.Errorf("images of registry %s: %w", r.GetMetadata().GetId(), err)
	}
	r.Status.ImagesCount = int32(min(len(images), maxInt32))
	if r.GetSpec() == nil {
		r.Spec = &registrypb.RegistrySpec{}
	}
	r.Spec.ImagesCount = r.GetStatus().GetImagesCount() // deprecated OUTPUT_ONLY mirror of the status field
	return nil
}

const maxInt32 = 1<<31 - 1

// Create stores the registry (CREATING) and, in the operation's work step,
// creates its backend namespace and marks it ACTIVE.
func (s *RegistryService) Create(ctx context.Context, cmd CreateRegistryCmd) (*operations.Operation, error) {
	r, err := s.newRegistry(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id := r.GetMetadata().GetId()
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create registry",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.repo.Create(ctx, r) },
		Work:        func(ctx context.Context) error { return s.activate(ctx, id) },
	})
}

// activate creates the backend namespace (idempotent) and marks the registry ACTIVE.
func (s *RegistryService) activate(ctx context.Context, id string) error {
	if err := s.backend.EnsureNamespace(ctx, Namespace(id)); err != nil {
		return fmt.Errorf("create namespace for registry %s: %w", id, err)
	}
	r, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	r.Status = &registrypb.RegistryStatus{State: registrypb.RegistryStatus_ACTIVE}
	return s.repo.Update(ctx, r)
}

func (s *RegistryService) newRegistry(ctx context.Context, cmd CreateRegistryCmd) (*registrypb.Registry, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if err := s.checkParent(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindRegistry)
	if err != nil {
		return nil, fmt.Errorf("new registry id: %w", err)
	}
	now := timestamppb.New(s.clk.Now())
	return &registrypb.Registry{
		Metadata: &commonpb.ResourceMetadata{
			Id:              id,
			ParentId:        md.GetParentId(),
			Name:            md.GetName(),
			Labels:          maps.Clone(md.GetLabels()),
			ResourceVersion: 1,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		Spec:   &registrypb.RegistrySpec{Description: cmd.Spec.GetDescription()},
		Status: &registrypb.RegistryStatus{State: registrypb.RegistryStatus_CREATING},
	}, nil
}

// checkParent requires an existing project.
func (s *RegistryService) checkParent(ctx context.Context, parentID string) error {
	if parentID == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if !strings.HasPrefix(parentID, KindProject+"-") {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a project ID"})
	}
	if _, err := s.hierarchy.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("registry parent %s: %w", parentID, err)
	}
	return nil
}

// Update applies a full-replace update with reset-mask semantics. Only name,
// labels and spec.description are client-owned.
func (s *RegistryService) Update(ctx context.Context, cmd UpdateRegistryCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update registry",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

func (s *RegistryService) applyUpdate(ctx context.Context, cmd UpdateRegistryCmd) error {
	stored, err := s.repo.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkUpdatable(stored, cmd.Metadata); err != nil {
		return err
	}
	updated, err := patchRegistry(stored, cmd)
	if err != nil {
		return err
	}
	if proto.Equal(updated, stored) {
		return nil // nothing changed: keep resource_version and updated_at
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	return s.repo.Update(ctx, updated)
}

// checkUpdatable enforces immutable parent_id and optimistic concurrency.
func checkUpdatable(stored *registrypb.Registry, md *commonpb.ResourceMetadata) error {
	if p := md.GetParentId(); p != "" && p != stored.GetMetadata().GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != stored.GetMetadata().GetResourceVersion() {
		return errs.Conflict(stored.GetMetadata().GetId(), fmt.Sprintf(
			"resource_version %d does not match current version %d", v, stored.GetMetadata().GetResourceVersion(),
		))
	}
	return nil
}

// patchRegistry applies the request to a copy of stored using the reset mask,
// then restores every server-owned field.
func patchRegistry(stored *registrypb.Registry, cmd UpdateRegistryCmd) (*registrypb.Registry, error) {
	patch := &registrypb.Registry{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     &registrypb.RegistrySpec{Description: cmd.Spec.GetDescription()},
	}
	resetMask := cmd.ResetMask
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMask); err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	if updated.GetMetadata() == nil {
		updated.Metadata = &commonpb.ResourceMetadata{}
	}
	md := stored.GetMetadata()
	updated.Metadata.Id = md.GetId()
	updated.Metadata.ParentId = md.GetParentId()
	updated.Metadata.ResourceVersion = md.GetResourceVersion()
	updated.Metadata.CreatedAt = md.GetCreatedAt()
	updated.Metadata.UpdatedAt = md.GetUpdatedAt()
	if updated.GetSpec() == nil {
		updated.Spec = &registrypb.RegistrySpec{}
	}
	updated.Spec.ImagesCount = stored.GetSpec().GetImagesCount()
	updated.Status = proto.CloneOf(stored.GetStatus())
	return updated, nil
}

// Delete removes a registry. A registry that still holds images is refused
// with FAILED_PRECONDITION unless Force is set.
func (s *RegistryService) Delete(ctx context.Context, cmd DeleteRegistryCmd) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  cmd.ID,
		Description: "Delete registry",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.checkDeletable(ctx, cmd) },
		Work:        func(ctx context.Context) error { return s.deleteRegistry(ctx, cmd.ID) },
	})
}

func (s *RegistryService) checkDeletable(ctx context.Context, cmd DeleteRegistryCmd) error {
	r, err := s.repo.Get(ctx, cmd.ID)
	if err != nil {
		return err
	}
	if cmd.Force || r.GetStatus().GetState() != registrypb.RegistryStatus_ACTIVE {
		return nil
	}
	images, err := s.backend.ListImages(ctx, Namespace(cmd.ID))
	if err != nil {
		return fmt.Errorf("images of registry %s: %w", cmd.ID, err)
	}
	if len(images) > 0 {
		return errs.BadState(cmd.ID, fmt.Sprintf("registry holds %d images; delete them first or set force", len(images)))
	}
	return nil
}

func (s *RegistryService) deleteRegistry(ctx context.Context, id string) error {
	if err := s.backend.DeleteNamespace(ctx, Namespace(id)); err != nil {
		return fmt.Errorf("delete namespace of registry %s: %w", id, err)
	}
	if err := s.index.DeleteRegistry(ctx, id); err != nil {
		return fmt.Errorf("forget artifacts of registry %s: %w", id, err)
	}
	return s.repo.Delete(ctx, id)
}

func pageSizeOf(pageSize int64) (int, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize),
		})
	}
	if pageSize == 0 {
		return defaultPageSize, nil
	}
	return int(pageSize), nil
}
