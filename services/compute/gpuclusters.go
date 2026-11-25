package compute

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/compute/api"
	"github.com/kfirgollan/NebiusMocker/services/compute/dal"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// registerGpuClusters wires GpuClusterService. It returns the membership port
// that InstanceService joins and leaves clusters through (spec.gpu_cluster).
func registerGpuClusters(reg grpc.ServiceRegistrar, deps mocker.Deps) (*domain.GpuClusterMembers, error) {
	fabrics, err := loadFabrics(deps.Config.ComputeFabricsFile)
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	clusters := dal.NewGpuClusterRepo(deps.Store)
	members, err := domain.NewGpuClusterMembers(clusters, fabrics)
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	svc, err := domain.NewGpuClusterService(domain.GpuClusterDeps{
		Clusters: clusters, Fabrics: fabrics, OpIndex: dal.NewGpuClusterOpIndex(deps.Store),
		Projects: deps.Authz, Regions: projectRegions{peers: deps.Peers},
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindGpuCluster, func(ctx context.Context, id string) (string, error) {
		c, err := clusters.Get(ctx, id)
		return c.GetMetadata().GetParentId(), err
	})
	api.RegisterGpuClusterAuthzRules(deps.Rules)
	computepb.RegisterGpuClusterServiceServer(reg, api.NewGpuClusterHandler(svc))
	return members, nil
}

// projectRegions resolves project regions through the iam module's
// peer.ProjectRegions, looked up per call (module order is not fixed).
type projectRegions struct{ peers *peer.Directory }

func (p projectRegions) Region(ctx context.Context, projectID string) (string, error) {
	r, ok := peer.Lookup(p.peers, peer.ProjectRegionsKey)
	if !ok {
		return "", errs.Internal("project regions are unavailable: the iam module is not running")
	}
	return r.Region(ctx, projectID)
}

// loadFabrics returns the built-in fabric catalog, or the one in path when set.
func loadFabrics(path string) (domain.FabricCatalog, error) {
	if path == "" {
		return domain.DefaultFabricCatalog()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.FabricCatalog{}, fmt.Errorf("read fabric catalog: %w", err)
	}
	return domain.ParseFabricCatalog(data)
}
