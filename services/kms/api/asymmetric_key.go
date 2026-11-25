package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/kms/domain"
)

// AsymmetricKeyHandler implements nebius.kms.v1.AsymmetricKeyService.
type AsymmetricKeyHandler struct {
	kmspb.UnimplementedAsymmetricKeyServiceServer
	svc *domain.AsymmetricKeyService
}

var _ kmspb.AsymmetricKeyServiceServer = (*AsymmetricKeyHandler)(nil)

// NewAsymmetricKeyHandler returns a handler backed by svc.
func NewAsymmetricKeyHandler(svc *domain.AsymmetricKeyService) *AsymmetricKeyHandler {
	return &AsymmetricKeyHandler{svc: svc}
}

// Create starts a key creation operation.
func (h *AsymmetricKeyHandler) Create(ctx context.Context, req *kmspb.CreateAsymmetricKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateAsymmetricKeyCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a key update operation; X-ResetMask selects fields to clear.
func (h *AsymmetricKeyHandler) Update(ctx context.Context, req *kmspb.UpdateAsymmetricKeyRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.Update(ctx, domain.UpdateAsymmetricKeyCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Get returns a key by ID.
func (h *AsymmetricKeyHandler) Get(ctx context.Context, req *kmspb.GetAsymmetricKeyRequest) (*kmspb.AsymmetricKey, error) {
	k, err := h.svc.Get(ctx, req.GetId(), visibility(req))
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// GetByName returns a key by parent and name.
func (h *AsymmetricKeyHandler) GetByName(ctx context.Context, req *kmspb.GetAsymmetricKeyByNameRequest) (*kmspb.AsymmetricKey, error) {
	k, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// List returns one page of a parent's keys.
func (h *AsymmetricKeyHandler) List(ctx context.Context, req *kmspb.ListAsymmetricKeysRequest) (*kmspb.ListAsymmetricKeysResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), visibility(req))
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &kmspb.ListAsymmetricKeysResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Delete schedules a key for deletion.
func (h *AsymmetricKeyHandler) Delete(ctx context.Context, req *kmspb.DeleteAsymmetricKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// UpdateDeletionDelay changes how long a deleted key is kept before purge.
func (h *AsymmetricKeyHandler) UpdateDeletionDelay(ctx context.Context, req *kmspb.UpdateAsymmetricKeyDeletionDelayRequest) (*commonpb.Operation, error) {
	if req.GetDeletionDelay() == nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: "deletion_delay", Message: "required"}))
	}
	op, err := h.svc.UpdateDeletionDelay(ctx, req.GetId(), req.GetDeletionDelay().AsDuration(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Undelete cancels a scheduled deletion, optionally renaming the key.
func (h *AsymmetricKeyHandler) Undelete(ctx context.Context, req *kmspb.UndeleteAsymmetricKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Undelete(ctx, req.GetId(), req.GetName(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
