package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// ClusterHandler implements nebius.mk8s.v1.ClusterService.
type ClusterHandler struct {
	mk8spb.UnimplementedClusterServiceServer
	svc *domain.Service
}

var _ mk8spb.ClusterServiceServer = (*ClusterHandler)(nil)

// NewClusterHandler returns a handler backed by svc.
func NewClusterHandler(svc *domain.Service) *ClusterHandler { return &ClusterHandler{svc: svc} }

// Get returns a cluster by ID (resource_version is deprecated and ignored).
func (h *ClusterHandler) Get(ctx context.Context, req *mk8spb.GetClusterRequest) (*mk8spb.Cluster, error) {
	c, err := h.svc.GetCluster(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// GetByName returns a cluster by project and name.
func (h *ClusterHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*mk8spb.Cluster, error) {
	c, err := h.svc.GetClusterByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// List returns one page of a project's clusters.
func (h *ClusterHandler) List(ctx context.Context, req *mk8spb.ListClustersRequest) (*mk8spb.ListClustersResponse, error) {
	page, err := h.svc.ListClusters(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &mk8spb.ListClustersResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a cluster creation operation.
func (h *ClusterHandler) Create(ctx context.Context, req *mk8spb.CreateClusterRequest) (*commonpb.Operation, error) {
	op, err := h.svc.CreateCluster(ctx, domain.CreateClusterCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a cluster update operation; X-ResetMask selects fields to clear.
func (h *ClusterHandler) Update(ctx context.Context, req *mk8spb.UpdateClusterRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.UpdateCluster(ctx, domain.UpdateClusterCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a cluster deletion operation.
func (h *ClusterHandler) Delete(ctx context.Context, req *mk8spb.DeleteClusterRequest) (*commonpb.Operation, error) {
	op, err := h.svc.DeleteCluster(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// ListControlPlaneVersions lists the Kubernetes versions clusters can run.
func (h *ClusterHandler) ListControlPlaneVersions(context.Context, *mk8spb.ListClusterControlPlaneVersionsRequest) (*mk8spb.ListClusterControlPlaneVersionsResponse, error) {
	return &mk8spb.ListClusterControlPlaneVersionsResponse{Items: h.svc.ControlPlaneVersions()}, nil
}
