package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// PermitHandler implements nebius.iam.v1.AccessPermitService.
type PermitHandler struct {
	iampb.UnimplementedAccessPermitServiceServer
	svc *domain.PermitService
}

var _ iampb.AccessPermitServiceServer = (*PermitHandler)(nil)

// NewPermitHandler returns a handler backed by svc.
func NewPermitHandler(svc *domain.PermitService) *PermitHandler { return &PermitHandler{svc: svc} }

// Create grants a role.
func (h *PermitHandler) Create(ctx context.Context, req *iampb.CreateAccessPermitRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreatePermitCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Get returns a permit.
func (h *PermitHandler) Get(ctx context.Context, req *iampb.GetAccessPermitRequest) (*iampb.AccessPermit, error) {
	p, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return p, nil
}

// Delete revokes a permit.
func (h *PermitHandler) Delete(ctx context.Context, req *iampb.DeleteAccessPermitRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// List returns one page of a group's permits.
func (h *PermitHandler) List(ctx context.Context, req *iampb.ListAccessPermitRequest) (*iampb.ListAccessPermitResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListAccessPermitResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}
