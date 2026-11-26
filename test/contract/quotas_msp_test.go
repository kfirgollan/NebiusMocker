package contract

import (
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	mlflowsvc "github.com/nebius/gosdk/services/nebius/msp/mlflow/v1alpha1"
	pgsvc "github.com/nebius/gosdk/services/nebius/msp/postgresql/v1alpha1"
	quotassvc "github.com/nebius/gosdk/services/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const mspGiB = 1 << 30

// startMSPQuotas runs IAM, VPC, Object Storage, quotas and both managed
// services (mock backends) under authz enforce.
func startMSPQuotas(t *testing.T) quotaEnv {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{
		iam.New(), vpc.New(), quotas.New(), storage.New(storage.WithS3Listen("127.0.0.1:0")),
		msppostgresql.New(), mspmlflow.New(),
	}, testkit.WithAuthz(authz.ModeEnforce), testkit.WithObjStore(memory.New(clock.Real{})),
		testkit.WithOperationDelay(10*time.Millisecond))
	return quotaEnv{inst: inst, quotas: quotassvc.NewQuotaAllowanceService(inst.SDK(t))}
}

// wantUsage checks the usage of several project allowances.
func (e quotaEnv) wantUsage(t *testing.T, step string, want map[string]uint64) {
	t.Helper()
	for name, n := range want {
		a := e.byName(t, e.inst.ProjectID, name)
		if got := a.GetStatus().GetUsage(); got != n {
			t.Errorf("%s: %s usage = %d, want %d", step, name, got, n)
		}
		if n > 0 && a.GetStatus().GetUsageState() != quotaspb.QuotaAllowanceStatus_USAGE_STATE_USED {
			t.Errorf("%s: %s usage_state = %v, want USED", step, name, a.GetStatus().GetUsageState())
		}
	}
}

// wantQuotaRefused checks that err is RESOURCE_EXHAUSTED naming name.
func wantQuotaRefused(t *testing.T, what string, err error, name string) {
	t.Helper()
	if v := quotaFailure(t, err).Violations(); len(v) == 0 || v[0].GetQuota() != name {
		t.Fatalf("%s: violations = %v, want %s", what, v, name)
	}
}

// TestQuotaManagedPostgreSQL drives the PostgreSQL quotas through gosdk:
// a create charges the cluster and its hosts' vCPUs, RAM and disk; a lowered
// allowance refuses a create, a restore and a growing update; a shrink and a
// delete release.
func TestQuotaManagedPostgreSQL(t *testing.T) {
	t.Parallel()
	e := startMSPQuotas(t)
	ctx := t.Context()
	pg := &pgEnv{inst: e.inst, project: e.inst.ProjectID, clusters: pgsvc.NewClusterService(e.inst.SDK(t)), backups: pgsvc.NewBackupService(e.inst.SDK(t))}
	zero := map[string]uint64{quota.MSPPostgreSQLClusterCount: 0, quota.MSPPostgreSQLVCPU: 0, quota.MSPPostgreSQLMemory: 0, quota.MSPPostgreSQLDiskSize: 0}
	e.wantUsage(t, "before", zero)

	// 2 hosts of 2vcpu-8gb with 20 GiB disks each.
	id := pg.newCluster(t, "main", func(s *pgpb.ClusterSpec) { s.Config.Template.Hosts.Count = 2 })
	e.wantUsage(t, "after create", map[string]uint64{
		quota.MSPPostgreSQLClusterCount: 1, quota.MSPPostgreSQLVCPU: 4,
		quota.MSPPostgreSQLMemory: 16 * mspGiB, quota.MSPPostgreSQLDiskSize: 40 * mspGiB,
	})

	// The vCPU allowance at its usage: a new cluster and a larger preset are refused.
	e.setLimit(t, e.inst.ProjectID, quota.MSPPostgreSQLVCPU, 4)
	_, err := pg.clusters.Create(ctx, pg.createReq("refused", nil))
	wantQuotaRefused(t, "create over the vCPU allowance", err, quota.MSPPostgreSQLVCPU)
	if _, err := pg.clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: pg.project, Name: "refused"}); err == nil {
		t.Error("a refused create stored the cluster")
	}
	cur := pg.getCluster(t, id)
	grown := cur.GetSpec()
	grown.Config.Template.Resources.Preset = "4vcpu-16gb"
	_, err = pg.clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: grown})
	wantQuotaRefused(t, "update over the vCPU allowance", err, quota.MSPPostgreSQLVCPU)
	if p := pg.getCluster(t, id).GetSpec().GetConfig().GetTemplate().GetResources().GetPreset(); p != "2vcpu-8gb" {
		t.Errorf("preset after a refused update = %s", p)
	}
	e.wantUsage(t, "after refusals", map[string]uint64{quota.MSPPostgreSQLClusterCount: 1, quota.MSPPostgreSQLVCPU: 4})

	// A shrink always passes and releases its share; the disk grows within
	// its allowance.
	cur = pg.getCluster(t, id)
	shrunk := cur.GetSpec()
	shrunk.Config.Template.Hosts.Count = 1
	shrunk.Config.Template.Disk.SizeGibibytes = 30
	alphaWait(t)(pg.clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: shrunk}))
	e.wantUsage(t, "after shrink", map[string]uint64{
		quota.MSPPostgreSQLVCPU: 2, quota.MSPPostgreSQLMemory: 8 * mspGiB, quota.MSPPostgreSQLDiskSize: 30 * mspGiB,
	})

	// A stopped cluster keeps its share.
	alphaWait(t)(pg.clusters.Stop(ctx, &pgpb.StopClusterRequest{Id: id}))
	e.wantUsage(t, "stopped", map[string]uint64{quota.MSPPostgreSQLClusterCount: 1, quota.MSPPostgreSQLVCPU: 2})
	alphaWait(t)(pg.clusters.Start(ctx, &pgpb.StartClusterRequest{Id: id}))

	// A restore creates a cluster: refused at the cluster allowance.
	backup := pg.onDemandBackup(t, id)
	e.setLimit(t, e.inst.ProjectID, quota.MSPPostgreSQLClusterCount, 1)
	restoreSpec := pgSpec()
	restoreSpec.Config.Template.Disk.SizeGibibytes = 30
	_, err = pg.clusters.Restore(ctx, &pgpb.RestoreClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: pg.project, Name: "copy"}, Spec: restoreSpec, SourceClusterId: id, BackupId: backup.GetId(),
	})
	wantQuotaRefused(t, "restore over the cluster allowance", err, quota.MSPPostgreSQLClusterCount)

	// Delete releases everything; the refused create then fits.
	alphaWait(t)(pg.clusters.Delete(ctx, &pgpb.DeleteClusterRequest{Id: id}))
	e.wantUsage(t, "after delete", zero)
	pg.newCluster(t, "refused", nil)
	e.wantUsage(t, "after the retried create", map[string]uint64{quota.MSPPostgreSQLClusterCount: 1, quota.MSPPostgreSQLVCPU: 2})
}

// TestQuotaManagedMLflow drives the MLflow quotas through gosdk: a lowered
// cluster allowance refuses a create, raising it admits it and the usage
// shows the cluster's size; a delete releases it.
func TestQuotaManagedMLflow(t *testing.T) {
	t.Parallel()
	e := startMSPQuotas(t)
	ctx := t.Context()
	sdk := e.inst.SDK(t)
	clusters := mlflowsvc.NewClusterService(sdk)
	sa := createServiceAccount(t, iamsvc.NewServiceAccountService(sdk), e.inst.ProjectID, "mlflow-sa").GetMetadata().GetId()
	req := &mlflowpb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: "tracking"},
		Spec:     &mlflowpb.ClusterSpec{AdminUsername: "admin", AdminPassword: mlflowPassword, ServiceAccountId: sa},
	}
	zero := map[string]uint64{quota.MSPMLflowClusterCount: 0, quota.MSPMLflowVCPU: 0, quota.MSPMLflowMemory: 0}
	e.wantUsage(t, "before", zero)

	e.setLimit(t, e.inst.ProjectID, quota.MSPMLflowClusterCount, 0)
	_, err := clusters.Create(ctx, req)
	wantQuotaRefused(t, "create over the cluster allowance", err, quota.MSPMLflowClusterCount)
	e.wantUsage(t, "after the refused create", zero)

	e.setLimit(t, e.inst.ProjectID, quota.MSPMLflowClusterCount, 1)
	id := alphaWait(t)(clusters.Create(ctx, req))
	// "medium", the default size: 8 vCPUs, 32 GiB.
	e.wantUsage(t, "after create", map[string]uint64{quota.MSPMLflowClusterCount: 1, quota.MSPMLflowVCPU: 8, quota.MSPMLflowMemory: 32 * mspGiB})

	// The vCPU allowance below one more cluster's size refuses it too.
	e.setLimit(t, e.inst.ProjectID, quota.MSPMLflowClusterCount, 2)
	e.setLimit(t, e.inst.ProjectID, quota.MSPMLflowVCPU, 15)
	req.Metadata.Name = "second"
	_, err = clusters.Create(ctx, req)
	wantQuotaRefused(t, "create over the vCPU allowance", err, quota.MSPMLflowVCPU)

	alphaWait(t)(clusters.Delete(ctx, &mlflowpb.DeleteClusterRequest{Id: id}))
	e.wantUsage(t, "after delete", zero)
}
