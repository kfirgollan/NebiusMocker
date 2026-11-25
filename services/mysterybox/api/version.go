package api

import (
	"context"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// VersionHandler implements nebius.mysterybox.v1.SecretVersionService.
type VersionHandler struct {
	mysteryboxpb.UnimplementedSecretVersionServiceServer
	svc *domain.Service
}

var _ mysteryboxpb.SecretVersionServiceServer = (*VersionHandler)(nil)

// NewVersionHandler returns a handler backed by svc.
func NewVersionHandler(svc *domain.Service) *VersionHandler { return &VersionHandler{svc: svc} }

// Get returns a version by ID, without its payload.
func (h *VersionHandler) Get(ctx context.Context, req *mysteryboxpb.GetSecretVersionRequest) (*mysteryboxpb.SecretVersion, error) {
	v, err := h.svc.GetVersion(ctx, req.GetId(), req.GetShowScheduledForDeletion())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return v, nil
}

// List returns one page of a secret's versions.
func (h *VersionHandler) List(ctx context.Context, req *mysteryboxpb.ListSecretVersionsRequest) (*mysteryboxpb.ListSecretVersionsResponse, error) {
	page, err := h.svc.ListVersions(ctx, domain.ListVersionsQuery{
		SecretID: req.GetParentId(), PageSize: req.GetPageSize(), PageToken: req.GetPageToken(),
		ShowDeleted: req.GetShowScheduledForDeletion(),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &mysteryboxpb.ListSecretVersionsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a version creation operation.
func (h *VersionHandler) Create(ctx context.Context, req *mysteryboxpb.CreateSecretVersionRequest) (*commonpb.Operation, error) {
	redacted := proto.CloneOf(req)
	if redacted.GetSpec() != nil {
		redacted.Spec.Payload = nil
	}
	op, err := h.svc.CreateVersion(ctx, domain.CreateVersionCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: redacted,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a soft-deletion operation.
func (h *VersionHandler) Delete(ctx context.Context, req *mysteryboxpb.DeleteSecretVersionRequest) (*commonpb.Operation, error) {
	op, err := h.svc.DeleteVersion(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Undelete starts an operation restoring a version scheduled for deletion.
func (h *VersionHandler) Undelete(ctx context.Context, req *mysteryboxpb.UndeleteSecretVersionRequest) (*commonpb.Operation, error) {
	op, err := h.svc.UndeleteVersion(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
