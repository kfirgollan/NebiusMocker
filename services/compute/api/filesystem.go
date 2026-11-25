package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

const filesystemService = "nebius.compute.v1.FilesystemService"

// RegisterFilesystemAuthzRules declares the permission each FilesystemService
// RPC needs (compute.filesystems.* on the project).
func RegisterFilesystemAuthzRules(r *authz.Rules) {
	r.CRUD(filesystemService, "compute", "filesystems")
	r.Add("/"+filesystemService+"/ListOperationsByParent", authz.OnParent(authz.Perm("compute", "filesystems", authz.VerbList)))
}

// FilesystemHandler implements nebius.compute.v1.FilesystemService.
type FilesystemHandler struct {
	computepb.UnimplementedFilesystemServiceServer
	svc *domain.FilesystemService
}

var _ computepb.FilesystemServiceServer = (*FilesystemHandler)(nil)

// NewFilesystemHandler returns a handler backed by svc.
func NewFilesystemHandler(svc *domain.FilesystemService) *FilesystemHandler {
	return &FilesystemHandler{svc: svc}
}

// Get returns a filesystem by ID.
func (h *FilesystemHandler) Get(ctx context.Context, req *computepb.GetFilesystemRequest) (*computepb.Filesystem, error) {
	f, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return f, nil
}

// GetByName returns a filesystem by parent and name.
func (h *FilesystemHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*computepb.Filesystem, error) {
	f, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return f, nil
}

// List returns one page of a project's filesystems.
func (h *FilesystemHandler) List(ctx context.Context, req *computepb.ListFilesystemsRequest) (*computepb.ListFilesystemsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter()) //nolint:staticcheck // the deprecated filter is read only to reject it
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListFilesystemsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a filesystem creation operation.
func (h *FilesystemHandler) Create(ctx context.Context, req *computepb.CreateFilesystemRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateFilesystemCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a filesystem update operation; X-ResetMask selects fields to clear.
func (h *FilesystemHandler) Update(ctx context.Context, req *computepb.UpdateFilesystemRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateFilesystemCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a filesystem deletion operation.
func (h *FilesystemHandler) Delete(ctx context.Context, req *computepb.DeleteFilesystemRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// ListOperationsByParent lists the operations on a project's filesystems.
func (h *FilesystemHandler) ListOperationsByParent(ctx context.Context, req *computepb.ListOperationsByParentRequest) (*commonpb.ListOperationsResponse, error) {
	page, err := h.svc.ListOperationsByParent(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	resp := &commonpb.ListOperationsResponse{NextPageToken: page.NextPageToken}
	for _, op := range page.Items {
		resp.Operations = append(resp.Operations, opsapi.ToProto(ctx, op))
	}
	return resp, nil
}
