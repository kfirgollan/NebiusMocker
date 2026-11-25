// Package api is the IAM gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// GroupHandler implements nebius.iam.v1.GroupService.
type GroupHandler struct {
	iampb.UnimplementedGroupServiceServer
	svc *domain.GroupService
}

var _ iampb.GroupServiceServer = (*GroupHandler)(nil)

// NewGroupHandler returns a handler backed by svc.
func NewGroupHandler(svc *domain.GroupService) *GroupHandler { return &GroupHandler{svc: svc} }

// Get returns a group by ID.
func (h *GroupHandler) Get(ctx context.Context, req *iampb.GetGroupRequest) (*iampb.Group, error) {
	g, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return g, nil
}

// GetByName returns a group by parent and name.
func (h *GroupHandler) GetByName(ctx context.Context, req *iampb.GetGroupByNameRequest) (*iampb.Group, error) {
	g, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return g, nil
}

// List returns one page of a tenant's groups.
func (h *GroupHandler) List(ctx context.Context, req *iampb.ListGroupsRequest) (*iampb.ListGroupsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListGroupsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a group creation operation.
func (h *GroupHandler) Create(ctx context.Context, req *iampb.CreateGroupRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateGroupCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a group update operation; X-ResetMask selects fields to clear.
func (h *GroupHandler) Update(ctx context.Context, req *iampb.UpdateGroupRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.Update(ctx, domain.UpdateGroupCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a group deletion operation.
func (h *GroupHandler) Delete(ctx context.Context, req *iampb.DeleteGroupRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
