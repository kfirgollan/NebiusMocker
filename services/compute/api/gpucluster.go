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

const gpuClusterService = "nebius.compute.v1.GpuClusterService"

// RegisterGpuClusterAuthzRules declares the permission each GpuClusterService
// RPC needs (compute.gpuClusters.* on the project).
func RegisterGpuClusterAuthzRules(r *authz.Rules) {
	r.CRUD(gpuClusterService, "compute", "gpuClusters")
	r.Add("/"+gpuClusterService+"/ListOperationsByParent", authz.OnParent(authz.Perm("compute", "gpuClusters", authz.VerbList)))
}

// GpuClusterHandler implements nebius.compute.v1.GpuClusterService.
type GpuClusterHandler struct {
	computepb.UnimplementedGpuClusterServiceServer
	svc *domain.GpuClusterService
}

var _ computepb.GpuClusterServiceServer = (*GpuClusterHandler)(nil)

// NewGpuClusterHandler returns a handler backed by svc.
func NewGpuClusterHandler(svc *domain.GpuClusterService) *GpuClusterHandler {
	return &GpuClusterHandler{svc: svc}
}

// Get returns a GPU cluster by ID.
func (h *GpuClusterHandler) Get(ctx context.Context, req *computepb.GetGpuClusterRequest) (*computepb.GpuCluster, error) {
	c, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// GetByName returns a GPU cluster by parent and name.
func (h *GpuClusterHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*computepb.GpuCluster, error) {
	c, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// List returns one page of a project's GPU clusters.
func (h *GpuClusterHandler) List(ctx context.Context, req *computepb.ListGpuClustersRequest) (*computepb.ListGpuClustersResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter()) //nolint:staticcheck // the deprecated filter is read only to reject it
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListGpuClustersResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a GPU cluster creation operation.
func (h *GpuClusterHandler) Create(ctx context.Context, req *computepb.CreateGpuClusterRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateGpuClusterCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a GPU cluster update operation; X-ResetMask selects fields to clear.
func (h *GpuClusterHandler) Update(ctx context.Context, req *computepb.UpdateGpuClusterRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateGpuClusterCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a GPU cluster deletion operation.
func (h *GpuClusterHandler) Delete(ctx context.Context, req *computepb.DeleteGpuClusterRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// ListOperationsByParent lists the operations on a project's GPU clusters.
func (h *GpuClusterHandler) ListOperationsByParent(ctx context.Context, req *computepb.ListOperationsByParentRequest) (*commonpb.ListOperationsResponse, error) {
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
