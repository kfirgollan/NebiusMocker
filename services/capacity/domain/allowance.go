package domain

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// CreateAllowanceCmd is the input of CreateAllowance.
type CreateAllowanceCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *capacitypb.CapacityAllowanceSpec
	Request  proto.Message
}

// UpdateAllowanceCmd is the input of UpdateAllowance (full replace guarded
// by a reset mask).
type UpdateAllowanceCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *capacitypb.CapacityAllowanceSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// GetAllowance returns an allowance by ID.
func (s *Service) GetAllowance(ctx context.Context, id string) (*capacitypb.CapacityAllowance, error) {
	a, err := s.allowances.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.withStatus(ctx, a)
}

// GetAllowanceByGroup returns the project's allowance for a block group.
// Without one, it returns the default (not created) allowance: no ID and no
// limit, i.e. the project may use the whole block group (docs: project
// limits).
func (s *Service) GetAllowanceByGroup(ctx context.Context, projectID, blockGroupID string) (*capacitypb.CapacityAllowance, error) {
	tenant, err := s.projectTenant(ctx, "parent_id", projectID)
	if err != nil {
		return nil, err
	}
	if _, err := s.group(blockGroupID, tenant); err != nil {
		return nil, err
	}
	all, err := s.allowances.ByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.GetSpec().GetCapacityBlockGroupId() == blockGroupID {
			return s.withStatus(ctx, a)
		}
	}
	return s.withStatus(ctx, defaultAllowance(projectID, blockGroupID))
}

// allowanceLimit returns the allowance's limit, nil when unlimited.
func allowanceLimit(a *capacitypb.CapacityAllowance) *uint64 {
	if a.GetSpec().Limit == nil {
		return nil
	}
	l := a.GetSpec().GetLimit()
	return &l
}

func defaultAllowance(projectID, blockGroupID string) *capacitypb.CapacityAllowance {
	return &capacitypb.CapacityAllowance{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: blockGroupID},
	}
}

// ListAllowances returns one page of the project's allowances: one per block
// group of the project's tenant, the created one or the default one.
func (s *Service) ListAllowances(ctx context.Context, projectID string, size int64, pageToken string) (Page[*capacitypb.CapacityAllowance], error) {
	tenant, err := s.projectTenant(ctx, "parent_id", projectID)
	if err != nil {
		return Page[*capacitypb.CapacityAllowance]{}, err
	}
	created, err := s.allowances.ByProject(ctx, projectID)
	if err != nil {
		return Page[*capacitypb.CapacityAllowance]{}, err
	}
	byGroup := map[string]*capacitypb.CapacityAllowance{}
	for _, a := range created {
		byGroup[a.GetSpec().GetCapacityBlockGroupId()] = a
	}
	var all []*capacitypb.CapacityAllowance
	for _, g := range s.inv.InTenant(tenant) {
		a, ok := byGroup[g.ID]
		if !ok {
			a = defaultAllowance(projectID, g.ID)
		}
		all = append(all, a)
	}
	return s.allowancePage(ctx, all, size, pageToken)
}

// ListAllowancesByGroup returns one page of the allowances created for a
// block group, of every project.
func (s *Service) ListAllowancesByGroup(ctx context.Context, blockGroupID string, size int64, pageToken string) (Page[*capacitypb.CapacityAllowance], error) {
	if _, err := s.group(blockGroupID, ""); err != nil {
		return Page[*capacitypb.CapacityAllowance]{}, err
	}
	all, err := s.allowances.ByBlockGroup(ctx, blockGroupID)
	if err != nil {
		return Page[*capacitypb.CapacityAllowance]{}, err
	}
	return s.allowancePage(ctx, all, size, pageToken)
}

func (s *Service) allowancePage(ctx context.Context, all []*capacitypb.CapacityAllowance, size int64, pageToken string) (Page[*capacitypb.CapacityAllowance], error) {
	page, err := listPage(all, size, maxAllowancePageSize, pageToken)
	if err != nil {
		return Page[*capacitypb.CapacityAllowance]{}, err
	}
	for i, a := range page.Items {
		if page.Items[i], err = s.withStatus(ctx, a); err != nil {
			return Page[*capacitypb.CapacityAllowance]{}, err
		}
	}
	return page, nil
}

// withStatus fills the allowance's status from the current usage of the
// project in the block group.
func (s *Service) withStatus(ctx context.Context, a *capacitypb.CapacityAllowance) (*capacitypb.CapacityAllowance, error) {
	st := &capacitypb.CapacityAllowanceStatus{
		State:      capacitypb.CapacityAllowanceStatus_STATE_ACTIVE,
		UsageState: capacitypb.CapacityAllowanceStatus_USAGE_STATE_NOT_USED,
		Unit:       Unit,
	}
	g, ok := s.inv.Group(a.GetSpec().GetCapacityBlockGroupId())
	if ok {
		_, byProject, err := s.groupUsage(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		usage := byProject[a.GetMetadata().GetParentId()]
		st.Usage = usage
		st.UsagePercentage = percentage(usage, effectiveLimit(allowanceLimit(a), g.limitAt(s.clk.Now())))
		if usage > 0 {
			st.UsageState = capacitypb.CapacityAllowanceStatus_USAGE_STATE_USED
		}
	}
	a.Status = st
	return a, nil
}

// CreateAllowance sets a project limit for a block group of the project's
// tenant. The limit may exceed the group's size: the effective limit is
// capped at the group's current limit, and a higher one takes effect when
// the group grows (docs: project limits). Without a limit the allowance is
// unlimited.
func (s *Service) CreateAllowance(ctx context.Context, cmd CreateAllowanceCmd) (*operations.Operation, error) {
	md := cmd.Metadata
	tenant, err := s.projectTenant(ctx, "metadata.parent_id", md.GetParentId())
	if err != nil {
		return nil, err
	}
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if md.GetResourceVersion() != 0 {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.resource_version", Message: "must not be set on create"})
	}
	groupID := cmd.Spec.GetCapacityBlockGroupId()
	if _, err := s.group(groupID, tenant); err != nil {
		return nil, fmt.Errorf("spec.capacity_block_group_id: %w", err)
	}
	id, err := s.ids.New(KindAllowance)
	if err != nil {
		return nil, fmt.Errorf("new capacity allowance id: %w", err)
	}
	now := timestamppb.New(s.clk.Now())
	a := &capacitypb.CapacityAllowance{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: md.GetParentId(), Name: md.GetName(), Labels: md.GetLabels(),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec: &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: groupID, Limit: cmd.Spec.Limit},
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create capacity allowance",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.allowances.Create(ctx, a)
		},
	})
}

// UpdateAllowance changes an allowance's limit, block group, name or labels.
func (s *Service) UpdateAllowance(ctx context.Context, cmd UpdateAllowanceCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update capacity allowance",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.updateAllowance(ctx, cmd)
		},
	})
}

func (s *Service) updateAllowance(ctx context.Context, cmd UpdateAllowanceCmd) error {
	stored, err := s.allowances.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	smd := stored.GetMetadata()
	if p := cmd.Metadata.GetParentId(); p != "" && p != smd.GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if v := cmd.Metadata.GetResourceVersion(); v != 0 && v != smd.GetResourceVersion() {
		return errs.BadState(smd.GetId(), fmt.Sprintf("resource_version %d does not match the current version %d", v, smd.GetResourceVersion()))
	}
	resetMask := cmd.ResetMask
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	// A patch may omit the spec (nebius CLI: update --patch --clear-mask
	// spec.limit sends only metadata.id): an empty spec patches nothing, and
	// the reset mask then clears just the fields it names.
	patchMsg := &capacitypb.CapacityAllowance{Metadata: cmd.Metadata, Spec: cmd.Spec}
	if patchMsg.GetSpec() == nil {
		patchMsg.Spec = &capacitypb.CapacityAllowanceSpec{}
	}
	if err := fieldmaskpb.PatchWithResetMask(updated, patchMsg, resetMask); err != nil {
		return errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	umd := updated.GetMetadata()
	if umd == nil || updated.GetSpec() == nil {
		return errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: "metadata and spec must not be reset"})
	}
	// The provider updates capacity_block_group_id in place: the allowance
	// moves to another block group of the tenant (the DAL keeps one
	// allowance per project and block group).
	if groupID := updated.GetSpec().GetCapacityBlockGroupId(); groupID != stored.GetSpec().GetCapacityBlockGroupId() {
		tenant, err := s.projectTenant(ctx, "metadata.parent_id", smd.GetParentId())
		if err != nil {
			return err
		}
		if _, err := s.group(groupID, tenant); err != nil {
			return fmt.Errorf("spec.capacity_block_group_id: %w", err)
		}
	}
	umd.Id, umd.ParentId, umd.ResourceVersion = smd.GetId(), smd.GetParentId(), smd.GetResourceVersion()
	umd.CreatedAt, umd.UpdatedAt = smd.GetCreatedAt(), smd.GetUpdatedAt()
	updated.Status = nil
	if proto.Equal(updated, stored) {
		return nil
	}
	umd.ResourceVersion++
	umd.UpdatedAt = timestamppb.New(s.clk.Now())
	return s.allowances.Update(ctx, updated)
}

// DeleteAllowance removes a project limit: the effective limit becomes the
// block group's size again.
func (s *Service) DeleteAllowance(ctx context.Context, id string, req proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete capacity allowance",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     req,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, err := s.allowances.Get(ctx, id); err != nil {
				return err
			}
			return s.allowances.Delete(ctx, id)
		},
	})
}
