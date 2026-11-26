package domain

import (
	"context"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// nodeClaim is what n new nodes of the template would consume in the
// cluster's project: nodes are compute instances, so they follow the Compute
// quotas (an instance, its vCPUs or GPUs, and a boot disk each). It is nil
// when no node is added.
func nodeClaim(c *mk8spb.Cluster, t *mk8spb.NodeTemplate, res peer.NodeResources, n int64) *peer.QuotaClaim {
	if n <= 0 || t == nil {
		return nil
	}
	instances := quota.ComputeInstanceCount
	if t.GetPreemptible() != nil {
		instances = quota.ComputeInstancePreemptibleCount
	}
	amounts := map[string]int64{
		instances:              n,
		quota.ComputeDiskCount: n,
		quota.ComputeDiskSize(t.GetBootDisk().GetType().String()): n * diskBytes(t.GetBootDisk()),
	}
	if gpuQuota := quota.ComputeInstanceGPU(t.GetResources().GetPlatform()); gpuQuota != "" {
		amounts[gpuQuota] = n * int64(res.GPUs)
	} else {
		amounts[quota.ComputeInstanceNonGPUVCPU] = n * int64(res.VCPUs)
	}
	return &peer.QuotaClaim{ContainerID: c.GetMetadata().GetParentId(), Amounts: amounts}
}

// checkNodeQuota refuses a node group create or scale-up the project's
// Compute quotas cannot hold. It only checks: the nodes are charged when
// compute creates their instances, which is also what enforces the quotas
// against concurrent requests.
func (s *Service) checkNodeQuota(ctx context.Context, p *ngPlan) error {
	if p.claim == nil {
		return nil
	}
	return s.d.Quota.Admit(ctx, *p.claim, nil)
}
