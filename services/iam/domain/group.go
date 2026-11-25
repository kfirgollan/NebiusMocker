package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// GroupService implements IAM group behavior.
type GroupService struct {
	groups      GroupRepository
	tenants     TenantRepository
	memberships MembershipRepository
	permits     PermitRepository
	ops         Operations
	ids         IDGenerator
	clk         clock.Clock
}

// GroupDeps are the dependencies of a GroupService. All are required.
type GroupDeps struct {
	Groups      GroupRepository
	Tenants     TenantRepository
	Memberships MembershipRepository
	Permits     PermitRepository
	Ops         Operations
	IDs         IDGenerator
	Clock       clock.Clock
}

// NewGroupService wires a GroupService.
func NewGroupService(d GroupDeps) (*GroupService, error) {
	if d.Groups == nil || d.Tenants == nil || d.Memberships == nil || d.Permits == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("iam: group service dependencies must not be nil")
	}
	return &GroupService{
		groups: d.Groups, tenants: d.Tenants, memberships: d.Memberships, permits: d.Permits,
		ops: d.Ops, ids: d.IDs, clk: d.Clock,
	}, nil
}

// CreateGroupCmd is the input of Create.
type CreateGroupCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iampb.GroupSpec
	// Request is echoed in Operation.request.
	Request proto.Message
}

// UpdateGroupCmd is the input of Update (full replace guarded by a reset mask).
type UpdateGroupCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iampb.GroupSpec
	// ResetMask lists fields to clear when absent from the request (X-ResetMask).
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a group by ID, with live member counts.
func (s *GroupService) Get(ctx context.Context, id string) (*iampb.Group, error) {
	g, err := s.groups.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return g, s.fillCounts(ctx, g)
}

// GetByName returns a group by its name within a tenant.
func (s *GroupService) GetByName(ctx context.Context, parentID, name string) (*iampb.Group, error) {
	g, err := s.groups.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return g, s.fillCounts(ctx, g)
}

// fillCounts sets the status member counts from the memberships.
func (s *GroupService) fillCounts(ctx context.Context, g *iampb.Group) error {
	users, sas, err := memberCounts(ctx, s.memberships, g.GetMetadata().GetId())
	if err != nil {
		return fmt.Errorf("member counts of %s: %w", g.GetMetadata().GetId(), err)
	}
	if g.GetStatus() == nil {
		g.Status = &iampb.GroupStatus{State: iampb.GroupStatus_ACTIVE}
	}
	g.Status.MembersCount = users + sas
	g.Status.TenantUserAccountsCount = users
	g.Status.ServiceAccountsCount = sas
	return nil
}

// List returns one page of a tenant's groups in creation order.
func (s *GroupService) List(ctx context.Context, parentID string, pageSize int64, pageToken string) (GroupPage, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return GroupPage{}, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize),
		})
	}
	size := int(pageSize)
	if size == 0 {
		size = defaultPageSize
	}
	page, err := s.groups.List(ctx, parentID, size, pageToken)
	if err != nil {
		return GroupPage{}, err
	}
	for _, g := range page.Items {
		if err := s.fillCounts(ctx, g); err != nil {
			return GroupPage{}, err
		}
	}
	return page, nil
}

// Create validates the request, stores the group and returns its operation.
func (s *GroupService) Create(ctx context.Context, cmd CreateGroupCmd) (*operations.Operation, error) {
	g, err := s.newGroup(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  g.GetMetadata().GetId(),
		Description: "Create group",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.groups.Create(ctx, g) },
	})
}

// Update applies a full-replace update with reset-mask semantics.
func (s *GroupService) Update(ctx context.Context, cmd UpdateGroupCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update group",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

// Delete removes a group when its operation completes.
func (s *GroupService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete group",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.groups.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error { return s.deleteCascade(ctx, id) },
	})
}

// deleteCascade removes the group's memberships and permits, then the group:
// they are its children and would otherwise grant access through a dead group.
// Each pass deletes a page, so the loop ends when a fresh listing is empty.
func (s *GroupService) deleteCascade(ctx context.Context, id string) error {
	for {
		page, err := s.memberships.ListByGroup(ctx, id, maxPageSize, "")
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			break
		}
		for _, m := range page.Items {
			if err := s.memberships.Delete(ctx, m.GetMetadata().GetId()); err != nil {
				return fmt.Errorf("delete membership %s: %w", m.GetMetadata().GetId(), err)
			}
		}
	}
	for {
		page, err := s.permits.ListByGroup(ctx, id, maxPageSize, "")
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			break
		}
		for _, p := range page.Items {
			if err := s.permits.Delete(ctx, p.GetMetadata().GetId()); err != nil {
				return fmt.Errorf("delete permit %s: %w", p.GetMetadata().GetId(), err)
			}
		}
	}
	return s.groups.Delete(ctx, id)
}

func (s *GroupService) newGroup(ctx context.Context, cmd CreateGroupCmd) (*iampb.Group, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if _, err := s.tenants.Get(ctx, parentID); err != nil {
		return nil, fmt.Errorf("group parent %s: %w", parentID, err)
	}
	id, err := s.ids.New(KindGroup)
	if err != nil {
		return nil, fmt.Errorf("new group id: %w", err)
	}
	now := timestamppb.New(s.clk.Now())
	spec := cmd.Spec
	if spec == nil {
		spec = &iampb.GroupSpec{}
	}
	return &iampb.Group{
		Metadata: &commonpb.ResourceMetadata{
			Id:              id,
			ParentId:        parentID,
			Name:            md.GetName(),
			Labels:          maps.Clone(md.GetLabels()),
			ResourceVersion: 1,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		Spec:   proto.CloneOf(spec),
		Status: &iampb.GroupStatus{State: iampb.GroupStatus_ACTIVE},
	}, nil
}

func (s *GroupService) applyUpdate(ctx context.Context, cmd UpdateGroupCmd) error {
	stored, err := s.groups.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkUpdatable(stored, cmd.Metadata); err != nil {
		return err
	}
	updated, err := patchGroup(stored, cmd)
	if err != nil {
		return err
	}
	if proto.Equal(updated, stored) {
		return nil // nothing changed: keep resource_version and updated_at
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	return s.groups.Update(ctx, updated)
}

// checkUpdatable enforces immutable parent_id and optimistic concurrency.
func checkUpdatable(stored *iampb.Group, md *commonpb.ResourceMetadata) error {
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

// patchGroup applies the request to a copy of stored using the reset mask, then
// restores every server-owned field the client cannot change.
func patchGroup(stored *iampb.Group, cmd UpdateGroupCmd) (*iampb.Group, error) {
	patch := &iampb.Group{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
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
		updated.Spec = &iampb.GroupSpec{}
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	return updated, nil
}
