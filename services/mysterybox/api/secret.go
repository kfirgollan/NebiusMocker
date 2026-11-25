// Package api is the MysteryBox gRPC view layer: it converts between protos
// and domain inputs, redacts payloads from echoed requests and maps domain
// errors. It makes no business decisions.
package api

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// SecretHandler implements nebius.mysterybox.v1.SecretService.
type SecretHandler struct {
	mysteryboxpb.UnimplementedSecretServiceServer
	svc *domain.Service
}

var _ mysteryboxpb.SecretServiceServer = (*SecretHandler)(nil)

// NewSecretHandler returns a handler backed by svc.
func NewSecretHandler(svc *domain.Service) *SecretHandler { return &SecretHandler{svc: svc} }

// Get returns a secret by ID, without its payload.
func (h *SecretHandler) Get(ctx context.Context, req *mysteryboxpb.GetSecretRequest) (*mysteryboxpb.Secret, error) {
	s, err := h.svc.GetSecret(ctx, req.GetId(), req.GetShowScheduledForDeletion())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return s, nil
}

// GetByName returns an active secret by project and name.
func (h *SecretHandler) GetByName(ctx context.Context, req *mysteryboxpb.GetSecretByNameRequest) (*mysteryboxpb.Secret, error) {
	s, err := h.svc.GetSecretByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return s, nil
}

// List returns one page of a project's secrets.
func (h *SecretHandler) List(ctx context.Context, req *mysteryboxpb.ListSecretsRequest) (*mysteryboxpb.ListSecretsResponse, error) {
	page, err := h.svc.ListSecrets(ctx, domain.ListSecretsQuery{
		ParentID: req.GetParentId(), PageSize: req.GetPageSize(), PageToken: req.GetPageToken(),
		ShowDeleted: req.GetShowScheduledForDeletion(),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &mysteryboxpb.ListSecretsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a secret creation operation.
func (h *SecretHandler) Create(ctx context.Context, req *mysteryboxpb.CreateSecretRequest) (*commonpb.Operation, error) {
	op, err := h.svc.CreateSecret(ctx, domain.CreateSecretCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: redactCreateSecret(req),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a secret update operation; X-ResetMask selects fields to clear.
func (h *SecretHandler) Update(ctx context.Context, req *mysteryboxpb.UpdateSecretRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.UpdateSecret(ctx, domain.UpdateSecretCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: redactUpdateSecret(req),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a soft-deletion operation.
func (h *SecretHandler) Delete(ctx context.Context, req *mysteryboxpb.DeleteSecretRequest) (*commonpb.Operation, error) {
	op, err := h.svc.DeleteSecret(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Undelete starts an operation restoring a secret scheduled for deletion.
func (h *SecretHandler) Undelete(ctx context.Context, req *mysteryboxpb.UndeleteSecretRequest) (*commonpb.Operation, error) {
	op, err := h.svc.UndeleteSecret(ctx, req.GetId(), req.GetName(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// redactCreateSecret returns a copy of req without payload values: requests
// are echoed in operations, which are not protected like payloads are.
func redactCreateSecret(req *mysteryboxpb.CreateSecretRequest) *mysteryboxpb.CreateSecretRequest {
	c := proto.CloneOf(req)
	if v := c.GetSpec().GetSecretVersion(); v != nil {
		v.Payload = nil
	}
	return c
}

func redactUpdateSecret(req *mysteryboxpb.UpdateSecretRequest) *mysteryboxpb.UpdateSecretRequest {
	c := proto.CloneOf(req)
	if v := c.GetSpec().GetSecretVersion(); v != nil {
		v.Payload = nil
	}
	return c
}
