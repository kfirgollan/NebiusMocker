package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// ServiceAccountHandler implements nebius.iam.v1.ServiceAccountService.
type ServiceAccountHandler struct {
	iampb.UnimplementedServiceAccountServiceServer
	svc *domain.ServiceAccountService
}

var _ iampb.ServiceAccountServiceServer = (*ServiceAccountHandler)(nil)

// NewServiceAccountHandler returns a handler backed by svc.
func NewServiceAccountHandler(svc *domain.ServiceAccountService) *ServiceAccountHandler {
	return &ServiceAccountHandler{svc: svc}
}

// Get returns a service account by ID.
func (h *ServiceAccountHandler) Get(ctx context.Context, req *iampb.GetServiceAccountRequest) (*iampb.ServiceAccount, error) {
	sa, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return sa, nil
}

// GetByName returns a service account by project and name.
func (h *ServiceAccountHandler) GetByName(ctx context.Context, req *iampb.GetServiceAccountByNameRequest) (*iampb.ServiceAccount, error) {
	sa, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return sa, nil
}

// List returns one page of a project's service accounts.
func (h *ServiceAccountHandler) List(ctx context.Context, req *iampb.ListServiceAccountRequest) (*iampb.ListServiceAccountResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListServiceAccountResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a service account creation operation.
func (h *ServiceAccountHandler) Create(ctx context.Context, req *iampb.CreateServiceAccountRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateServiceAccountCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a service account update; X-ResetMask selects fields to clear.
func (h *ServiceAccountHandler) Update(ctx context.Context, req *iampb.UpdateServiceAccountRequest) (*commonpb.Operation, error) {
	resetMask, err := resetMaskOf(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateServiceAccountCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a service account deletion operation.
func (h *ServiceAccountHandler) Delete(ctx context.Context, req *iampb.DeleteServiceAccountRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// resetMaskOf reads X-ResetMask from the request metadata.
func resetMaskOf(ctx context.Context) (*mask.Mask, error) {
	m, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()})
	}
	return m, nil
}
