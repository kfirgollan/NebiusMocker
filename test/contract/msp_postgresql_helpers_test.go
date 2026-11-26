package contract

import (
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	resourcepb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1/resource"
	pgsvc "github.com/nebius/gosdk/services/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const pgPassword = "Sup3r-secret-pw"

// pgRealClock times the waits of the tests themselves.
var pgRealClock clock.Clock = clock.Real{}

// pgEnv is a mocker with IAM, VPC and Managed PostgreSQL (mock backend).
type pgEnv struct {
	inst     *testkit.Instance
	project  string
	clusters pgsvc.ClusterService
	backups  pgsvc.BackupService
	// clk is set by startPGWithClock.
	clk *clock.Manual
}

func startPG(t *testing.T, opts ...testkit.Option) *pgEnv {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(10 * time.Millisecond)}, opts...)
	return newPGEnv(t, nil, opts)
}

// startPGWithClock runs the mocker on a manual clock, so that the backup
// schedule and retention come due when the test advances it. Operations
// complete inline (a delay would wait for the clock).
func startPGWithClock(t *testing.T, opts ...testkit.Option) *pgEnv {
	t.Helper()
	clk := clock.NewManual(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	opts = append([]testkit.Option{testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithClock(clk)}, opts...)
	return newPGEnv(t, clk, opts)
}

func newPGEnv(t *testing.T, clk *clock.Manual, opts []testkit.Option) *pgEnv {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), msppostgresql.New()}, opts...)
	sdk := inst.SDK(t)
	return &pgEnv{inst: inst, project: inst.ProjectID, clusters: pgsvc.NewClusterService(sdk), backups: pgsvc.NewBackupService(sdk), clk: clk}
}

// as returns the cluster and backup clients authenticated with token.
func (e *pgEnv) as(t *testing.T, token string) (pgsvc.ClusterService, pgsvc.BackupService) {
	t.Helper()
	sdk := e.inst.SDKAs(t, token)
	return pgsvc.NewClusterService(sdk), pgsvc.NewBackupService(sdk)
}

// pgSpec is the spec Terraform sends for a minimal cluster.
func pgSpec() *pgpb.ClusterSpec {
	return &pgpb.ClusterSpec{
		Config: &pgpb.ConfigSpec{
			Version: "16",
			Template: &pgpb.TemplateSpec{
				Resources: &resourcepb.ResourcesSpec{Platform: "cpu-e2", Preset: "2vcpu-8gb"},
				Hosts:     &resourcepb.HostSpec{Count: 1},
				Disk:      &resourcepb.DiskSpec{Type: "network-ssd", SizeGibibytes: 20},
			},
		},
		Bootstrap: &pgpb.BootstrapSpec{UserName: "app", UserPassword: pgPassword, DbName: "appdb"},
	}
}

func (e *pgEnv) createReq(name string, mod func(*pgpb.ClusterSpec)) *pgpb.CreateClusterRequest {
	spec := pgSpec()
	if mod != nil {
		mod(spec)
	}
	return &pgpb.CreateClusterRequest{Metadata: &commonpb.ResourceMetadata{ParentId: e.project, Name: name}, Spec: spec}
}

func (e *pgEnv) newCluster(t *testing.T, name string, mod func(*pgpb.ClusterSpec)) string {
	t.Helper()
	return alphaWait(t)(e.clusters.Create(t.Context(), e.createReq(name, mod)))
}

func (e *pgEnv) getCluster(t *testing.T, id string) *pgpb.Cluster {
	t.Helper()
	c, err := e.clusters.Get(t.Context(), &pgpb.GetClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("get cluster %s: %v", id, err)
	}
	return c
}

func (e *pgEnv) listBackups(t *testing.T, clusterID string) []*pgpb.Backup {
	t.Helper()
	r, err := e.backups.ListByCluster(t.Context(), &pgpb.ListBackupsByClusterRequest{ClusterId: clusterID})
	if err != nil {
		t.Fatalf("list backups of %s: %v", clusterID, err)
	}
	return r.GetBackups()
}

// onDemandBackup takes an on-demand backup and returns it.
func (e *pgEnv) onDemandBackup(t *testing.T, clusterID string) *pgpb.Backup {
	t.Helper()
	seen := map[string]bool{}
	for _, b := range e.listBackups(t, clusterID) {
		seen[b.GetId()] = true
	}
	if err := alphaOK(t.Context())(e.backups.Create(t.Context(), &pgpb.CreateBackupRequest{ClusterId: clusterID})); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	for _, b := range e.listBackups(t, clusterID) {
		if !seen[b.GetId()] {
			return b
		}
	}
	t.Fatal("no new backup after Create")
	return nil
}

// advance moves the manual clock and waits until the module's sweeper has
// run for it and parked again on the clock.
func (e *pgEnv) advance(t *testing.T, d time.Duration) {
	t.Helper()
	e.clk.Advance(d)
	deadline := pgRealClock.Now().Add(5 * time.Second)
	for !e.clk.HasWaiter(e.clk.Now().Add(time.Minute)) {
		if pgRealClock.Now().After(deadline) {
			t.Fatal("the backup sweeper did not run")
		}
		<-pgRealClock.After(time.Millisecond)
	}
}

// waitSweeperParked waits until the sweeper first waits on the clock.
func (e *pgEnv) waitSweeperParked(t *testing.T) {
	t.Helper()
	deadline := pgRealClock.Now().Add(5 * time.Second)
	for !e.clk.HasWaiter(e.clk.Now().Add(time.Minute)) {
		if pgRealClock.Now().After(deadline) {
			t.Fatal("the backup sweeper did not start")
		}
		<-pgRealClock.After(time.Millisecond)
	}
}

func pgPhase(c *pgpb.Cluster) (mspv1alpha1pb.ClusterStatus_Phase, mspv1alpha1pb.ClusterStatus_State) {
	return c.GetStatus().GetPhase(), c.GetStatus().GetState()
}
