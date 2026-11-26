package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const federatedCredentialsService = "nebius.iam.v1.FederatedCredentialsService" //nolint:gosec // G101: a gRPC service name, not a credential

// RegisterFederatedCredentialsRules declares the permissions of the federated
// credentials RPCs (iam.federatedCredentials.<verb>, on the project).
func RegisterFederatedCredentialsRules(r *authz.Rules) {
	r.CRUD(federatedCredentialsService, "iam", "federatedCredentials")
}

// FederatedCredentialsHandler implements nebius.iam.v1.FederatedCredentialsService.
type FederatedCredentialsHandler struct {
	iampb.UnimplementedFederatedCredentialsServiceServer
	svc *domain.FederatedCredentialsService
}

var _ iampb.FederatedCredentialsServiceServer = (*FederatedCredentialsHandler)(nil)

// NewFederatedCredentialsHandler returns a handler backed by svc.
func NewFederatedCredentialsHandler(svc *domain.FederatedCredentialsService) *FederatedCredentialsHandler {
	return &FederatedCredentialsHandler{svc: svc}
}

// Get returns federated credentials by ID.
func (h *FederatedCredentialsHandler) Get(ctx context.Context, req *iampb.GetFederatedCredentialsRequest) (*iampb.FederatedCredentials, error) {
	c, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// GetByName returns federated credentials by parent and name.
func (h *FederatedCredentialsHandler) GetByName(ctx context.Context, req *iampb.GetByNameFederatedCredentialsRequest) (*iampb.FederatedCredentials, error) {
	c, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// List returns one page of a project's federated credentials.
func (h *FederatedCredentialsHandler) List(ctx context.Context, req *iampb.ListFederatedCredentialsRequest) (*iampb.ListFederatedCredentialsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListFederatedCredentialsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a creation operation.
func (h *FederatedCredentialsHandler) Create(ctx context.Context, req *iampb.CreateFederatedCredentialsRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateFederatedCredentialsCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts an update; X-ResetMask selects fields to clear.
func (h *FederatedCredentialsHandler) Update(ctx context.Context, req *iampb.UpdateFederatedCredentialsRequest) (*commonpb.Operation, error) {
	resetMask, err := resetMaskOf(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateFederatedCredentialsCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a deletion operation.
func (h *FederatedCredentialsHandler) Delete(ctx context.Context, req *iampb.DeleteFederatedCredentialsRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}
