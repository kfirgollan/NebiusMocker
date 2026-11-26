// Package api is the Managed MLflow gRPC view layer: it converts between
// protos and domain inputs and maps domain errors. It makes no business
// decisions.
//
//nolint:staticcheck // SA1019: nebius.common.v1alpha1 operations are deprecated upstream; this v1alpha1 service returns them
package api

import (
	"context"

	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

// clusterService is the gRPC service this module serves.
const clusterService = "nebius.msp.mlflow.v1alpha1.ClusterService"

// RegisterAuthzRules declares the permission each RPC needs:
// mlflow.clusters.* on the cluster (or its project). Stop and Start are
// lifecycle verbs, which editors hold.
func RegisterAuthzRules(r *authz.Rules) {
	m := func(method string) string { return "/" + clusterService + "/" + method }
	perm := func(verb string) authz.Permission { return authz.Perm("mlflow", "clusters", verb) }
	r.Add(m("Get"), authz.OnID(perm(authz.VerbGet)))
	r.Add(m("GetByName"), authz.OnParent(perm(authz.VerbGet)))
	r.Add(m("List"), authz.OnParent(perm(authz.VerbList)))
	r.Add(m("Create"), authz.OnMetadataParent(perm(authz.VerbCreate)))
	r.Add(m("Delete"), authz.OnID(perm(authz.VerbDelete)))
	r.Add(m("Stop"), authz.OnID(perm(authz.VerbStop)))
	r.Add(m("Start"), authz.OnID(perm(authz.VerbStart)))
}

// ClusterHandler implements nebius.msp.mlflow.v1alpha1.ClusterService.
type ClusterHandler struct {
	mlflowpb.UnimplementedClusterServiceServer
	svc *domain.Service
}

var _ mlflowpb.ClusterServiceServer = (*ClusterHandler)(nil)

// NewClusterHandler returns a handler backed by svc.
func NewClusterHandler(svc *domain.Service) *ClusterHandler { return &ClusterHandler{svc: svc} }

// Get returns a cluster by ID.
func (h *ClusterHandler) Get(ctx context.Context, req *mlflowpb.GetClusterRequest) (*mlflowpb.Cluster, error) {
	c, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// GetByName returns a cluster by project and name.
func (h *ClusterHandler) GetByName(ctx context.Context, req *mlflowpb.GetClusterByNameRequest) (*mlflowpb.Cluster, error) {
	c, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// List returns one page of a project's clusters.
func (h *ClusterHandler) List(ctx context.Context, req *mlflowpb.ListClustersRequest) (*mlflowpb.ListClustersResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &mlflowpb.ListClustersResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a cluster creation operation.
func (h *ClusterHandler) Create(ctx context.Context, req *mlflowpb.CreateClusterRequest) (*commonv1alpha1pb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateClusterCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	return alphaOp(ctx, op, err)
}

// Delete starts a cluster deletion operation.
func (h *ClusterHandler) Delete(ctx context.Context, req *mlflowpb.DeleteClusterRequest) (*commonv1alpha1pb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	return alphaOp(ctx, op, err)
}

// Stop starts an operation that stops a running cluster.
func (h *ClusterHandler) Stop(ctx context.Context, req *mlflowpb.StopClusterRequest) (*commonv1alpha1pb.Operation, error) {
	op, err := h.svc.Stop(ctx, req.GetId(), req)
	return alphaOp(ctx, op, err)
}

// Start starts an operation that starts a stopped cluster.
func (h *ClusterHandler) Start(ctx context.Context, req *mlflowpb.StartClusterRequest) (*commonv1alpha1pb.Operation, error) {
	op, err := h.svc.Start(ctx, req.GetId(), req)
	return alphaOp(ctx, op, err)
}

// alphaOp maps a started operation to its common.v1alpha1 proto.
func alphaOp(ctx context.Context, op *operations.Operation, err error) (*commonv1alpha1pb.Operation, error) {
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.AlphaToProto(ctx, op), nil
}
