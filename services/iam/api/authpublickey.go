package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// AuthPublicKeyHandler implements nebius.iam.v1.AuthPublicKeyService.
type AuthPublicKeyHandler struct {
	iampb.UnimplementedAuthPublicKeyServiceServer
	svc *domain.AuthPublicKeyService
}

var _ iampb.AuthPublicKeyServiceServer = (*AuthPublicKeyHandler)(nil)

// NewAuthPublicKeyHandler returns a handler backed by svc.
func NewAuthPublicKeyHandler(svc *domain.AuthPublicKeyService) *AuthPublicKeyHandler {
	return &AuthPublicKeyHandler{svc: svc}
}

// Get returns a key by ID.
func (h *AuthPublicKeyHandler) Get(ctx context.Context, req *iampb.GetAuthPublicKeyRequest) (*iampb.AuthPublicKey, error) {
	k, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// GetByName returns a key by parent and name.
func (h *AuthPublicKeyHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*iampb.AuthPublicKey, error) {
	k, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// List returns one page of a parent's keys.
func (h *AuthPublicKeyHandler) List(ctx context.Context, req *iampb.ListAuthPublicKeyRequest) (*iampb.ListAuthPublicKeyResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListAuthPublicKeyResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListByAccount returns one page of a service account's keys.
func (h *AuthPublicKeyHandler) ListByAccount(ctx context.Context, req *iampb.ListAuthPublicKeyByAccountRequest) (*iampb.ListAuthPublicKeyResponse, error) {
	page, err := h.svc.ListByAccount(ctx, req.GetAccount(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListAuthPublicKeyResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a key creation operation.
func (h *AuthPublicKeyHandler) Create(ctx context.Context, req *iampb.CreateAuthPublicKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateAuthPublicKeyCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a key update; X-ResetMask selects fields to clear.
func (h *AuthPublicKeyHandler) Update(ctx context.Context, req *iampb.UpdateAuthPublicKeyRequest) (*commonpb.Operation, error) {
	resetMask, err := resetMaskOf(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateAuthPublicKeyCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a key deletion operation.
func (h *AuthPublicKeyHandler) Delete(ctx context.Context, req *iampb.DeleteAuthPublicKeyRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

// Activate re-enables a key.
func (h *AuthPublicKeyHandler) Activate(ctx context.Context, req *iampb.ActivateAuthPublicKeyRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Activate(ctx, req.GetId(), req))
}

// Deactivate disables a key.
func (h *AuthPublicKeyHandler) Deactivate(ctx context.Context, req *iampb.DeactivateAuthPublicKeyRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Deactivate(ctx, req.GetId(), req))
}
