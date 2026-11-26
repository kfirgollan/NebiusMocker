package domain

import (
	"context"

	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// gib is the quota unit of RAM (docs: "Nebius uses binary units").
const gib = 1 << 30

// QuotaAmounts is what a cluster consumes: one cluster and the vCPUs and RAM
// of its size. A stopped cluster keeps its share. A size the catalog no
// longer offers counts only the cluster.
func (s *Service) QuotaAmounts(c *mlflowpb.Cluster) map[string]int64 {
	out := map[string]int64{quota.MSPMLflowClusterCount: 1}
	if cpus, memGiB, ok := parseSize(s.d.Config.Sizes[c.GetSpec().GetSize()]); ok {
		out[quota.MSPMLflowVCPU] = cpus
		out[quota.MSPMLflowMemory] = memGiB * gib
	}
	return out
}

// createWithQuota stores a new cluster under the quota gate of its project.
func (s *Service) createWithQuota(ctx context.Context, c *mlflowpb.Cluster) error {
	claim := peer.QuotaClaim{ContainerID: c.GetMetadata().GetParentId(), Amounts: s.QuotaAmounts(c)}
	return s.d.Quota.Admit(ctx, claim, func(ctx context.Context) error { return s.d.Clusters.Create(ctx, c) })
}
