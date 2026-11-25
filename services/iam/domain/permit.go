package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// PermitService implements IAM access permits: a group (the parent, i.e. the
// subject) is granted a role on a resource and, through inheritance, on
// everything below it.
type PermitService struct {
	permits   PermitRepository
	groups    GroupRepository
	hierarchy Hierarchy
	roles     RoleCatalog
	ops       Operations
	ids       IDGenerator
	clk       clock.Clock
}

// PermitDeps are the dependencies of a PermitService. All are required.
type PermitDeps struct {
	Permits   PermitRepository
	Groups    GroupRepository
	Hierarchy Hierarchy
	Roles     RoleCatalog
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
}

// NewPermitService wires a PermitService.
func NewPermitService(d PermitDeps) (*PermitService, error) {
	if d.Permits == nil || d.Groups == nil || d.Hierarchy == nil || d.Roles == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("iam: permit service dependencies must not be nil")
	}
	return &PermitService{
		permits: d.Permits, groups: d.Groups, hierarchy: d.Hierarchy, roles: d.Roles,
		ops: d.Ops, ids: d.IDs, clk: d.Clock,
	}, nil
}

// CreatePermitCmd is the input of Create.
type CreatePermitCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iampb.AccessPermitSpec
	Request  proto.Message
}

// Get returns a permit by ID.
func (s *PermitService) Get(ctx context.Context, id string) (*iampb.AccessPermit, error) {
	return s.permits.Get(ctx, id)
}

// List returns one page of a group's permits.
func (s *PermitService) List(ctx context.Context, groupID string, pageSize int64, pageToken string) (PermitPage, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return PermitPage{}, err
	}
	return s.permits.ListByGroup(ctx, groupID, size, pageToken)
}

// Create grants the role.
func (s *PermitService) Create(ctx context.Context, cmd CreatePermitCmd) (*operations.Operation, error) {
	p, err := s.newPermit(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  p.GetMetadata().GetId(),
		Description: "Create access permit",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.permits.Create(ctx, p) },
	})
}

// Delete revokes the permit when the operation completes.
func (s *PermitService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete access permit",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.permits.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error { return s.permits.Delete(ctx, id) },
	})
}

func (s *PermitService) newPermit(ctx context.Context, cmd CreatePermitCmd) (*iampb.AccessPermit, error) {
	md, spec := cmd.Metadata, cmd.Spec
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	group, err := s.groups.Get(ctx, md.GetParentId())
	if err != nil {
		return nil, fmt.Errorf("permit subject group %s: %w", md.GetParentId(), err)
	}
	if spec.GetRole() == "" || !s.roles.Has(spec.GetRole()) {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.role", Message: fmt.Sprintf("unknown role %q", spec.GetRole())})
	}
	if spec.GetResourceId() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.resource_id", Message: "required"})
	}
	// Upstream: "If parent of subject is not from resource's hierarchy - NOT_FOUND will be thrown."
	root, err := s.hierarchy.Root(ctx, spec.GetResourceId())
	if err != nil {
		return nil, fmt.Errorf("permit resource %s: %w", spec.GetResourceId(), err)
	}
	if root != group.GetMetadata().GetParentId() {
		return nil, errs.NotFound(spec.GetResourceId())
	}
	id, err := s.ids.New(KindAccessPermit)
	if err != nil {
		return nil, fmt.Errorf("new permit id: %w", err)
	}
	now := timestamppb.New(s.clk.Now())
	return &iampb.AccessPermit{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: group.GetMetadata().GetId(), Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   &iampb.AccessPermitSpec{ResourceId: spec.GetResourceId(), Role: spec.GetRole()},
		Status: &iampb.AccessPermitStatus{},
	}, nil
}
