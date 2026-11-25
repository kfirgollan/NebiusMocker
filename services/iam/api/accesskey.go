package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// AccessKeyHandler implements nebius.iam.v2.AccessKeyService. The *ByAwsId
// mutations are not implemented yet (UNIMPLEMENTED).
type AccessKeyHandler struct {
	iamv2pb.UnimplementedAccessKeyServiceServer
	svc *domain.AccessKeyService
}

var _ iamv2pb.AccessKeyServiceServer = (*AccessKeyHandler)(nil)

// NewAccessKeyHandler returns a handler backed by svc.
func NewAccessKeyHandler(svc *domain.AccessKeyService) *AccessKeyHandler {
	return &AccessKeyHandler{svc: svc}
}

// Get returns a key by ID.
func (h *AccessKeyHandler) Get(ctx context.Context, req *iamv2pb.GetAccessKeyRequest) (*iamv2pb.AccessKey, error) {
	k, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// GetByName returns a key by parent and name.
func (h *AccessKeyHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*iamv2pb.AccessKey, error) {
	k, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// GetByAwsId returns a key by its AWS access key ID.
func (h *AccessKeyHandler) GetByAwsId(ctx context.Context, req *iamv2pb.GetAccessKeyByAwsIdRequest) (*iamv2pb.AccessKey, error) {
	k, err := h.svc.GetByAwsID(ctx, req.GetAwsAccessKeyId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return k, nil
}

// GetSecret reveals a key's secret.
func (h *AccessKeyHandler) GetSecret(ctx context.Context, req *iamv2pb.GetAccessKeySecretRequest) (*iamv2pb.GetAccessKeySecretResponse, error) {
	s, err := h.svc.GetSecret(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iamv2pb.GetAccessKeySecretResponse{AwsAccessKeyId: s.AwsAccessKeyID, Secret: s.Secret}, nil
}

// List returns one page of a parent's keys.
func (h *AccessKeyHandler) List(ctx context.Context, req *iamv2pb.ListAccessKeysRequest) (*iamv2pb.ListAccessKeysResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iamv2pb.ListAccessKeysResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListByAccount returns every key of an account (not paged).
func (h *AccessKeyHandler) ListByAccount(ctx context.Context, req *iamv2pb.ListAccessKeysByAccountRequest) (*iamv2pb.ListAccessKeysResponse, error) {
	keys, err := h.svc.ListByAccount(ctx, req.GetAccount())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iamv2pb.ListAccessKeysResponse{Items: keys}, nil
}

// Create starts an access key creation operation.
func (h *AccessKeyHandler) Create(ctx context.Context, req *iamv2pb.CreateAccessKeyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateAccessKeyCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts an access key update; X-ResetMask selects fields to clear.
func (h *AccessKeyHandler) Update(ctx context.Context, req *iamv2pb.UpdateAccessKeyRequest) (*commonpb.Operation, error) {
	resetMask, err := resetMaskOf(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateAccessKeyCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts an access key deletion operation.
func (h *AccessKeyHandler) Delete(ctx context.Context, req *iamv2pb.DeleteAccessKeyRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

// Activate re-enables a key.
func (h *AccessKeyHandler) Activate(ctx context.Context, req *iamv2pb.ActivateAccessKeyRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Activate(ctx, req.GetId(), req))
}

// Deactivate disables a key.
func (h *AccessKeyHandler) Deactivate(ctx context.Context, req *iamv2pb.DeactivateAccessKeyRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Deactivate(ctx, req.GetId(), req))
}

// opResult converts a domain (operation, error) pair to the RPC result.
func opResult(ctx context.Context) func(*operations.Operation, error) (*commonpb.Operation, error) {
	return func(op *operations.Operation, err error) (*commonpb.Operation, error) {
		if err != nil {
			return nil, serviceerror.FromDomain(ctx, err)
		}
		return opsapi.ToProto(ctx, op), nil
	}
}
