// Package api is the quotas gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/quotas/domain"
)

// ServiceName is the gRPC service served by this module.
const ServiceName = "nebius.quotas.v1.QuotaAllowanceService"

// RegisterAuthzRules declares the permission of each RPC
// (quotas.quotaAllowances.<verb>; only admins may create, update or delete,
// see pkg/authz roles).
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(ServiceName, "quotas", "quotaAllowances")
}

// Handler implements nebius.quotas.v1.QuotaAllowanceService.
type Handler struct {
	quotaspb.UnimplementedQuotaAllowanceServiceServer
	svc *domain.Service
}

var _ quotaspb.QuotaAllowanceServiceServer = (*Handler)(nil)

// NewHandler returns a handler backed by svc.
func NewHandler(svc *domain.Service) *Handler { return &Handler{svc: svc} }

// List returns one page of a tenant's or project's allowances.
func (h *Handler) List(ctx context.Context, req *quotaspb.ListQuotaAllowancesRequest) (*quotaspb.ListQuotaAllowancesResponse, error) {
	items, next, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &quotaspb.ListQuotaAllowancesResponse{Items: items, NextPageToken: next}, nil
}

// Get returns an allowance by ID.
func (h *Handler) Get(ctx context.Context, req *quotaspb.GetQuotaAllowanceRequest) (*quotaspb.QuotaAllowance, error) {
	a, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return a, nil
}

// GetByName returns a container's allowance of a quota in a region.
func (h *Handler) GetByName(ctx context.Context, req *quotaspb.GetByNameRequest) (*quotaspb.QuotaAllowance, error) {
	a, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName(), req.GetRegion())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return a, nil
}

// Create sets an allowance (an upsert, as in Nebius).
func (h *Handler) Create(ctx context.Context, req *quotaspb.CreateQuotaAllowanceRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update changes an allowance; X-ResetMask selects fields to clear.
func (h *Handler) Update(ctx context.Context, req *quotaspb.UpdateQuotaAllowanceRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.Update(ctx, domain.UpdateCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete resets an allowance to its default.
func (h *Handler) Delete(ctx context.Context, req *quotaspb.DeleteQuotaAllowanceRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
