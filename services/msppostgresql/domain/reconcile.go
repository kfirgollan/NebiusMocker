package domain

import (
	"context"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
)

const gib = 1 << 30

// ensureCluster reconciles the backend with the stored cluster and records
// the result in its status. A cluster that has just become RUNNING for the
// first time gets its automatic base backup (the base for point-in-time
// recovery).
func (s *Service) ensureCluster(ctx context.Context, id string, r run) error {
	unlock := s.clusterLock(id)
	defer unlock()
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	bc := s.backendCluster(c)
	bc.Password, bc.Restore = r.password, r.restore
	ep, err := s.d.Backend.EnsureCluster(ctx, bc)
	if err != nil {
		return s.fail(ctx, c, err)
	}
	setStatus(c, mspv1alpha1pb.ClusterStatus_PHASE_RUNNING, s.diskState(ctx, c))
	c.Status.ConnectionEndpoints = &pgpb.Endpoints{
		PrivateReadWrite: ep.PrivateReadWrite, PrivateReadOnly: ep.PrivateReadOnly,
		PublicReadWrite: ep.PublicReadWrite, PublicReadOnly: ep.PublicReadOnly,
	}
	if err := s.d.Clusters.Update(ctx, c); err != nil {
		return err
	}
	if r.firstBackup {
		s.firstBackup(ctx, c)
	}
	return nil
}

// backendCluster is the desired state of c for the backend (no password).
func (s *Service) backendCluster(c *pgpb.Cluster) BackendCluster {
	spec := c.GetSpec()
	cfg, t := spec.GetConfig(), spec.GetConfig().GetTemplate()
	cpus, memGiB, _ := config.ParsePreset(t.GetResources().GetPreset())
	var params map[string]string
	if pc := cfg.GetPostgresqlConfig_16(); pc != nil {
		params, _ = postgresParameters(pc)
	}
	return BackendCluster{
		ID: c.GetMetadata().GetId(), ProjectID: c.GetMetadata().GetParentId(), Name: c.GetMetadata().GetName(),
		Version: cfg.GetVersion(), Hosts: int(t.GetHosts().GetCount()),
		CPUs: cpus, MemoryBytes: memGiB * gib, DiskGiB: t.GetDisk().GetSizeGibibytes(),
		Parameters: params, Pooler: pooler(cfg.GetPoolerConfig()), PublicAccess: cfg.GetPublicAccess(),
		UserName: spec.GetBootstrap().GetUserName(), DBName: spec.GetBootstrap().GetDbName(),
	}
}
