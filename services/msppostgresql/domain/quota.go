package domain

import (
	"context"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// QuotaAmounts is what a cluster with spec consumes: one cluster, and the
// vCPUs, RAM and disk of each of its hosts (docs: the quotas "apply to the
// vCPU, RAM, and storage limits users set when they create clusters"). A
// stopped cluster keeps its share: its hosts and disks are reserved.
func QuotaAmounts(spec *pgpb.ClusterSpec) map[string]int64 {
	t := spec.GetConfig().GetTemplate()
	hosts := t.GetHosts().GetCount()
	cpus, memGiB, _ := config.ParsePreset(t.GetResources().GetPreset())
	return map[string]int64{
		quota.MSPPostgreSQLClusterCount: 1,
		quota.MSPPostgreSQLVCPU:         hosts * cpus,
		quota.MSPPostgreSQLMemory:       hosts * memGiB * gib,
		quota.MSPPostgreSQLDiskSize:     hosts * t.GetDisk().GetSizeGibibytes() * gib,
	}
}

// resizeAmounts is what an update from prev to next adds (negative entries
// are a shrink); the cluster count does not change.
func resizeAmounts(prev, next *pgpb.ClusterSpec) map[string]int64 {
	out := QuotaAmounts(next)
	for k, v := range QuotaAmounts(prev) {
		out[k] -= v
	}
	return out
}

// admitQuota runs apply under the quota gate when amounts add consumption
// to the project, and directly otherwise.
func (s *Service) admitQuota(ctx context.Context, projectID string, amounts map[string]int64, apply func(ctx context.Context) error) error {
	for _, n := range amounts {
		if n > 0 {
			return s.d.Quota.Admit(ctx, peer.QuotaClaim{ContainerID: projectID, Amounts: amounts}, apply)
		}
	}
	return apply(ctx)
}
