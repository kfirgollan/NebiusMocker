package api

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// ClusterHandler implements nebius.msp.postgresql.v1alpha1.ClusterService.
type ClusterHandler struct {
	pgpb.UnimplementedClusterServiceServer
	svc *domain.Service
}

var _ pgpb.ClusterServiceServer = (*ClusterHandler)(nil)

// NewClusterHandler returns a handler backed by svc.
func NewClusterHandler(svc *domain.Service) *ClusterHandler { return &ClusterHandler{svc: svc} }

// clusterView is the cluster as the API shows it: bootstrap.user_password is
// INPUT_ONLY and never leaves the service.
func clusterView(c *pgpb.Cluster) *pgpb.Cluster {
	if c.GetSpec().GetBootstrap().GetUserPassword() == "" {
		return c
	}
	out := proto.CloneOf(c)
	out.Spec.Bootstrap.UserPassword = ""
	return out
}

// op maps a started operation to its v1alpha1 proto.
//
//nolint:staticcheck // SA1019: nebius.common.v1alpha1 is deprecated upstream, still what this API returns
func op(ctx context.Context, o *operations.Operation, err error) (*commonv1alpha1pb.Operation, error) {
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.AlphaToProto(ctx, o), nil
}

// Get returns a cluster by ID.
func (h *ClusterHandler) Get(ctx context.Context, req *pgpb.GetClusterRequest) (*pgpb.Cluster, error) {
	c, err := h.svc.GetCluster(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return clusterView(c), nil
}

// GetByName returns a cluster by project and name.
func (h *ClusterHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*pgpb.Cluster, error) {
	c, err := h.svc.GetClusterByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return clusterView(c), nil
}

// GetForBackup returns the cluster as it was when the backup was taken.
func (h *ClusterHandler) GetForBackup(ctx context.Context, req *pgpb.GetClusterForBackupRequest) (*pgpb.Cluster, error) {
	c, err := h.svc.GetClusterForBackup(ctx, req.GetId(), req.GetBackupId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return clusterView(c), nil
}

// List returns one page of a project's clusters.
func (h *ClusterHandler) List(ctx context.Context, req *pgpb.ListClustersRequest) (*pgpb.ListClustersResponse, error) {
	page, err := h.svc.ListClusters(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	out := &pgpb.ListClustersResponse{NextPageToken: page.NextPageToken}
	for _, c := range page.Items {
		out.Clusters = append(out.Clusters, clusterView(c))
	}
	return out, nil
}

// Create starts a cluster creation operation.
func (h *ClusterHandler) Create(ctx context.Context, req *pgpb.CreateClusterRequest) (*commonv1alpha1pb.Operation, error) { //nolint:staticcheck // SA1019: see op
	o, err := h.svc.CreateCluster(ctx, domain.CreateClusterCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	return op(ctx, o, err)
}

// Update starts a cluster update operation; X-ResetMask selects fields to clear.
func (h *ClusterHandler) Update(ctx context.Context, req *pgpb.UpdateClusterRequest) (*commonv1alpha1pb.Operation, error) { //nolint:staticcheck // SA1019: see op
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	o, err := h.svc.UpdateCluster(ctx, domain.UpdateClusterCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	return op(ctx, o, err)
}

// Delete starts a cluster deletion operation.
func (h *ClusterHandler) Delete(ctx context.Context, req *pgpb.DeleteClusterRequest) (*commonv1alpha1pb.Operation, error) { //nolint:staticcheck // SA1019: see op
	o, err := h.svc.DeleteCluster(ctx, req.GetId(), req)
	return op(ctx, o, err)
}

// Restore starts the creation of a cluster from a backup.
func (h *ClusterHandler) Restore(ctx context.Context, req *pgpb.RestoreClusterRequest) (*commonv1alpha1pb.Operation, error) { //nolint:staticcheck // SA1019: see op
	cmd := domain.RestoreClusterCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), SourceClusterID: req.GetSourceClusterId(), BackupID: req.GetBackupId(), Request: req,
	}
	if req.GetRecoveryTime() != nil {
		t := req.GetRecoveryTime().AsTime()
		cmd.RecoveryTime = &t
	}
	o, err := h.svc.RestoreCluster(ctx, cmd)
	return op(ctx, o, err)
}

// Stop starts an operation that stops a running cluster.
func (h *ClusterHandler) Stop(ctx context.Context, req *pgpb.StopClusterRequest) (*commonv1alpha1pb.Operation, error) { //nolint:staticcheck // SA1019: see op
	o, err := h.svc.StopCluster(ctx, req.GetId(), req)
	return op(ctx, o, err)
}

// Start starts an operation that starts a stopped cluster.
func (h *ClusterHandler) Start(ctx context.Context, req *pgpb.StartClusterRequest) (*commonv1alpha1pb.Operation, error) { //nolint:staticcheck // SA1019: see op
	o, err := h.svc.StartCluster(ctx, req.GetId(), req)
	return op(ctx, o, err)
}
