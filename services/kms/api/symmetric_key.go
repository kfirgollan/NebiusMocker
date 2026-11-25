// Package api is the KMS gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
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

// SymmetricKeyHandler implements nebius.kms.v1.SymmetricKeyService.
type SymmetricKeyHandler struct {
	kmspb.UnimplementedSymmetricKeyServiceServer
	svc *domain.SymmetricKeyService
}

var _ kmspb.SymmetricKeyServiceServer = (*SymmetricKeyHandler)(nil)

// NewSymmetricKeyHandler returns a handler backed by svc.
func NewSymmetricKeyHandler(svc *domain.SymmetricKeyService) *SymmetricKeyHandler {
	return &SymmetricKeyHandler{svc: svc}
}

// Create starts a key creation operation.
func (h *SymmetricKeyHandler) Create(ctx context.Context, req *kmspb.CreateSymmetricKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateSymmetricKeyCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a key update operation; X-ResetMask selects fields to clear.
func (h *SymmetricKeyHandler) Update(ctx context.Context, req *kmspb.UpdateSymmetricKeyRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.Update(ctx, domain.UpdateSymmetricKeyCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Rotate starts a manual rotation operation.
func (h *SymmetricKeyHandler) Rotate(ctx context.Context, req *kmspb.RotateSymmetricKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Rotate(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Get returns a key by ID.
func (h *SymmetricKeyHandler) Get(ctx context.Context, req *kmspb.GetSymmetricKeyRequest) (*kmspb.SymmetricKey, error) {
	k, err := h.svc.Get(ctx, req.GetId(), visibility(req))
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// GetByName returns a key by parent and name.
func (h *SymmetricKeyHandler) GetByName(ctx context.Context, req *kmspb.GetSymmetricKeyByNameRequest) (*kmspb.SymmetricKey, error) {
	k, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// List returns one page of a parent's keys.
func (h *SymmetricKeyHandler) List(ctx context.Context, req *kmspb.ListSymmetricKeysRequest) (*kmspb.ListSymmetricKeysResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), visibility(req))
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &kmspb.ListSymmetricKeysResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Delete schedules a key for deletion.
func (h *SymmetricKeyHandler) Delete(ctx context.Context, req *kmspb.DeleteSymmetricKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// UpdateDeletionDelay changes how long a deleted key is kept before purge.
func (h *SymmetricKeyHandler) UpdateDeletionDelay(ctx context.Context, req *kmspb.UpdateSymmetricKeyDeletionDelayRequest) (*commonpb.Operation, error) {
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
func (h *SymmetricKeyHandler) Undelete(ctx context.Context, req *kmspb.UndeleteSymmetricKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Undelete(ctx, req.GetId(), req.GetName(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
