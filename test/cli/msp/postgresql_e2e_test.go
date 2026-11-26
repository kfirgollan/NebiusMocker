//go:build e2e

// Package msp runs the official nebius CLI against NebiusMocker through the
// Managed PostgreSQL flows the Nebius docs show (create a cluster, update it,
// stop and start it, take and delete backups, restore from a backup), and
// verifies the results with the CLI's own JSON output and the Go SDK.
package msp

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	pgsvc "github.com/nebius/gosdk/services/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

const password = "Sup3r-secret-pw"

type cluster struct {
	Metadata struct {
		ID       string            `json:"id"`
		ParentID string            `json:"parent_id"`
		Name     string            `json:"name"`
		Labels   map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Description string `json:"description"`
		NetworkID   string `json:"network_id"`
		Config      struct {
			Version      string `json:"version"`
			PublicAccess bool   `json:"public_access"`
			Template     struct {
				Resources struct{ Platform, Preset string } `json:"resources"`
				Hosts     struct{ Count string }            `json:"hosts"`
				Disk      struct {
					Type          string
					SizeGibibytes string `json:"size_gibibytes"`
				} `json:"disk"`
			} `json:"template"`
		} `json:"config"`
		Bootstrap map[string]any `json:"bootstrap"`
	} `json:"spec"`
	Status struct {
		Phase               string `json:"phase"`
		State               string `json:"state"`
		ConnectionEndpoints struct {
			PrivateReadWrite string `json:"private_read_write"`
		} `json:"connection_endpoints"`
	} `json:"status"`
}

type backup struct {
	ID               string `json:"id"`
	SourceClusterID  string `json:"source_cluster_id"`
	SourceName       string `json:"source_cluster_name"`
	SourceVisible    bool   `json:"source_cluster_visible"`
	OnDemand         bool   `json:"on_demand"`
	CreationFinished any    `json:"creation_finish"`
}

type backupList struct {
	Backups []backup `json:"backups"`
}

type env struct {
	cli      *nebiuscli.CLI
	clusters pgsvc.ClusterService
	backups  pgsvc.BackupService
	project  string
	network  string
}

func start(t *testing.T) *env {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), msppostgresql.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(100*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	sdk := inst.SDK(t)
	e := &env{cli: cli, clusters: pgsvc.NewClusterService(sdk), backups: pgsvc.NewBackupService(sdk), project: inst.ProjectID}
	var n struct {
		Metadata struct{ ID string } `json:"metadata"`
	}
	cli.JSON(t, &n, "vpc", "network", "get-by-name", "--name", "default-network")
	e.network = n.Metadata.ID
	return e
}

func (e *env) fails(t *testing.T, want string, args ...string) {
	t.Helper()
	_, err := e.cli.Run(t.Context(), args...)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("nebius %s: err = %v, want one mentioning %q", strings.Join(args, " "), err, want)
	}
}

func (e *env) sdkCluster(t *testing.T, id string) *pgpb.Cluster {
	t.Helper()
	c, err := e.clusters.Get(t.Context(), &pgpb.GetClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("sdk get cluster %s: %v", id, err)
	}
	if c.GetSpec().GetBootstrap().GetUserPassword() != "" {
		t.Errorf("cluster %s returns the bootstrap password", id)
	}
	return c
}

// checkRunning compares the CLI's view with the SDK's for a RUNNING cluster.
func (e *env) checkRunning(t *testing.T, step string, cli cluster, preset string, hosts, disk int64) {
	t.Helper()
	id := cli.Metadata.ID
	if cli.Status.Phase != "PHASE_RUNNING" || cli.Status.State != "STATE_FINISHED" || !strings.HasPrefix(cli.Status.ConnectionEndpoints.PrivateReadWrite, id+".") {
		t.Errorf("%s: cli status = %+v, want PHASE_RUNNING/STATE_FINISHED with a private endpoint", step, cli.Status)
	}
	if _, leaked := cli.Spec.Bootstrap["user_password"]; leaked {
		t.Errorf("%s: cli output carries the bootstrap password: %v", step, cli.Spec.Bootstrap)
	}
	if cli.Spec.Config.Template.Resources.Preset != preset || cli.Spec.NetworkID != e.network {
		t.Errorf("%s: cli spec = %+v", step, cli.Spec)
	}
	sdk := e.sdkCluster(t, id)
	tpl := sdk.GetSpec().GetConfig().GetTemplate()
	if sdk.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || tpl.GetResources().GetPreset() != preset ||
		tpl.GetHosts().GetCount() != hosts || tpl.GetDisk().GetSizeGibibytes() != disk || sdk.GetSpec().GetNetworkId() != e.network {
		t.Errorf("%s: sdk cluster = %v", step, sdk)
	}
}

func TestClusterAndBackupFlows(t *testing.T) {
	e := start(t)
	ctx := t.Context()

	// nebius msp postgresql v1alpha1 cluster create …
	var c cluster
	e.cli.JSON(t, &c, "msp", "postgresql", "v1alpha1", "cluster", "create",
		"--parent-id", e.project, "--name", "cli-pg", "--description", "from the cli", "--labels", "env=dev",
		"--network-id", e.network,
		"--bootstrap-user-name", "app", "--bootstrap-db-name", "appdb", "--bootstrap-user-password", password,
		"--config-version", "16", "--config-template-resources-platform", "cpu-e2", "--config-template-resources-preset", "2vcpu-8gb",
		"--config-template-hosts-count", "1", "--config-template-disk-type", "network-ssd", "--config-template-disk-size-gibibytes", "20",
		"--config-pooler-config-pooling-mode", "transaction", "--config-postgresql-config-16-max-connections", "150")
	id := c.Metadata.ID
	if id == "" || c.Metadata.Name != "cli-pg" || c.Metadata.ParentID != e.project || c.Spec.Description != "from the cli" || c.Metadata.Labels["env"] != "dev" {
		t.Fatalf("create = %+v", c)
	}
	var got cluster
	e.cli.JSON(t, &got, "msp", "postgresql", "v1alpha1", "cluster", "get", id)
	e.checkRunning(t, "get", got, "2vcpu-8gb", 1, 20)
	sdk := e.sdkCluster(t, id)
	if sdk.GetSpec().GetConfig().GetPoolerConfig().GetPoolingMode() != pgpb.ConnectionPoolerConfig_TRANSACTION ||
		sdk.GetSpec().GetConfig().GetPostgresqlConfig_16().GetMaxConnections() != 150 {
		t.Errorf("sdk config = %v", sdk.GetSpec().GetConfig())
	}

	// get-by-name and list
	var byName cluster
	e.cli.JSON(t, &byName, "msp", "postgresql", "v1alpha1", "cluster", "get-by-name", "--parent-id", e.project, "--name", "cli-pg")
	if byName.Metadata.ID != id {
		t.Fatalf("get-by-name = %+v, want %s", byName.Metadata, id)
	}
	var list struct {
		Clusters []cluster `json:"clusters"`
	}
	e.cli.JSON(t, &list, "msp", "postgresql", "v1alpha1", "cluster", "list", "--parent-id", e.project)
	if len(list.Clusters) != 1 || list.Clusters[0].Metadata.ID != id {
		t.Fatalf("list = %+v", list)
	}
	// A duplicate name is refused.
	e.fails(t, "AlreadyExists", "msp", "postgresql", "v1alpha1", "cluster", "create",
		"--parent-id", e.project, "--name", "cli-pg", "--network-id", e.network,
		"--bootstrap-user-name", "app", "--bootstrap-db-name", "appdb", "--bootstrap-user-password", password,
		"--config-version", "16", "--config-template-resources-platform", "cpu-e2", "--config-template-resources-preset", "2vcpu-8gb",
		"--config-template-hosts-count", "1", "--config-template-disk-type", "network-ssd", "--config-template-disk-size-gibibytes", "20")

	// nebius msp postgresql v1alpha1 cluster update <id> --config-template-resources-preset …
	var up cluster
	e.cli.JSON(t, &up, "msp", "postgresql", "v1alpha1", "cluster", "update", id,
		"--config-template-resources-preset", "4vcpu-16gb", "--config-template-hosts-count", "2", "--config-template-disk-size-gibibytes", "30")
	e.cli.JSON(t, &got, "msp", "postgresql", "v1alpha1", "cluster", "get", id)
	e.checkRunning(t, "update", got, "4vcpu-16gb", 2, 30)
	if sdk := e.sdkCluster(t, id); sdk.GetStatus().GetPresetDetails().GetCpuCount() != 4 ||
		sdk.GetSpec().GetConfig().GetPoolerConfig().GetPoolingMode() != pgpb.ConnectionPoolerConfig_TRANSACTION {
		t.Errorf("update lost or missed a field: %v", sdk)
	}
	// The password and public access change in place; the password is never returned.
	e.cli.JSON(t, &up, "msp", "postgresql", "v1alpha1", "cluster", "update", id,
		"--bootstrap-user-password", "An0ther-secret-pw", "--config-public-access", "--labels-add", "team=data")
	e.cli.JSON(t, &got, "msp", "postgresql", "v1alpha1", "cluster", "get", id)
	e.checkRunning(t, "update2", got, "4vcpu-16gb", 2, 30)
	if sdk := e.sdkCluster(t, id); !sdk.GetSpec().GetConfig().GetPublicAccess() || sdk.GetStatus().GetConnectionEndpoints().GetPublicReadWrite() == "" ||
		sdk.GetMetadata().GetLabels()["team"] != "data" || sdk.GetMetadata().GetLabels()["env"] != "dev" {
		t.Errorf("after the password/public-access update: %v", sdk)
	}

	// stop / start
	e.cli.JSON(t, &got, "msp", "postgresql", "v1alpha1", "cluster", "stop", id)
	e.cli.JSON(t, &got, "msp", "postgresql", "v1alpha1", "cluster", "get", id)
	if got.Status.Phase != "PHASE_STOPPING" || got.Status.State != "STATE_FINISHED" {
		t.Errorf("after stop: status = %+v, want STOPPING/FINISHED", got.Status)
	}
	if p := e.sdkCluster(t, id).GetStatus().GetPhase(); p != mspv1alpha1pb.ClusterStatus_PHASE_STOPPING {
		t.Errorf("sdk phase after stop = %v", p)
	}
	e.fails(t, id, "msp", "postgresql", "v1alpha1", "backup", "create", "--cluster-id", id) // a stopped cluster cannot be backed up
	e.cli.JSON(t, &got, "msp", "postgresql", "v1alpha1", "cluster", "start", id)
	e.cli.JSON(t, &got, "msp", "postgresql", "v1alpha1", "cluster", "get", id)
	e.checkRunning(t, "start", got, "4vcpu-16gb", 2, 30)

	// backup create / list / list-by-cluster
	// BackupService.Create returns the operation; its resource_id names the
	// new backup ("the resource that this operation creates").
	var op struct {
		ID         string `json:"id"`
		ResourceID string `json:"resource_id"`
		Status     any    `json:"status"`
	}
	e.cli.JSON(t, &op, "msp", "postgresql", "v1alpha1", "backup", "create", "--cluster-id", id)
	if op.ID == "" {
		t.Fatalf("backup create printed no operation: %+v", op)
	}
	var b backup
	var all, byCluster backupList
	e.cli.JSON(t, &all, "msp", "postgresql", "v1alpha1", "backup", "list", "--parent-id", e.project)
	e.cli.JSON(t, &byCluster, "msp", "postgresql", "v1alpha1", "backup", "list-by-cluster", "--cluster-id", id)
	if len(all.Backups) != len(byCluster.Backups) || len(byCluster.Backups) < 2 { // the automatic first backup and ours
		t.Fatalf("backups: list = %+v, list-by-cluster = %+v", all, byCluster)
	}
	for _, x := range byCluster.Backups {
		if x.OnDemand {
			b = x
		}
	}
	if b.ID == "" || b.SourceName != "cli-pg" || !b.SourceVisible || b.SourceClusterID != id {
		t.Fatalf("no correct on-demand backup in %+v", byCluster)
	}
	if op.ResourceID != b.ID {
		t.Fatalf("backup create operation resource_id = %q, want the new backup %q", op.ResourceID, b.ID)
	}
	sdkList, err := e.backups.List(ctx, &pgpb.ListBackupsRequest{ParentId: e.project})
	if err != nil || len(sdkList.GetBackups()) != len(all.Backups) {
		t.Errorf("sdk backups = %v, %v, cli lists %d", sdkList, err, len(all.Backups))
	}

	// nebius msp postgresql v1alpha1 cluster restore … from the on-demand backup
	var r cluster
	e.cli.JSON(t, &r, "msp", "postgresql", "v1alpha1", "cluster", "restore",
		"--parent-id", e.project, "--name", "cli-pg-restored", "--source-cluster-id", id, "--backup-id", b.ID,
		"--network-id", e.network,
		"--bootstrap-user-name", "app", "--bootstrap-db-name", "appdb", "--bootstrap-user-password", password,
		"--config-version", "16", "--config-template-resources-platform", "cpu-e2", "--config-template-resources-preset", "2vcpu-8gb",
		"--config-template-hosts-count", "1", "--config-template-disk-type", "network-ssd", "--config-template-disk-size-gibibytes", "30")
	restoredID := r.Metadata.ID
	if restoredID == "" || restoredID == id {
		t.Fatalf("restore = %+v", r)
	}
	e.cli.JSON(t, &got, "msp", "postgresql", "v1alpha1", "cluster", "get", restoredID)
	if got.Metadata.Name != "cli-pg-restored" || got.Status.Phase != "PHASE_RUNNING" {
		t.Errorf("restored cluster = %+v", got)
	}
	if rc := e.sdkCluster(t, restoredID); rc.GetSpec().GetConfig().GetTemplate().GetDisk().GetSizeGibibytes() != 30 {
		t.Errorf("sdk restored cluster = %v", rc)
	}
	// A disk smaller than the source's is refused.
	e.fails(t, "disk", "msp", "postgresql", "v1alpha1", "cluster", "restore",
		"--parent-id", e.project, "--name", "too-small", "--source-cluster-id", id, "--backup-id", b.ID, "--network-id", e.network,
		"--bootstrap-user-name", "app", "--bootstrap-db-name", "appdb", "--bootstrap-user-password", password,
		"--config-version", "16", "--config-template-resources-platform", "cpu-e2", "--config-template-resources-preset", "2vcpu-8gb",
		"--config-template-hosts-count", "1", "--config-template-disk-type", "network-ssd", "--config-template-disk-size-gibibytes", "10")

	// backup delete: on-demand ones only
	var automatic backup
	for _, x := range byCluster.Backups {
		if !x.OnDemand {
			automatic = x
		}
	}
	if automatic.ID == "" {
		t.Fatalf("no automatic backup in %+v", byCluster)
	}
	e.fails(t, "on-demand", "msp", "postgresql", "v1alpha1", "backup", "delete", "--cluster-id", id, "--backup-id", automatic.ID)
	e.cli.JSON(t, &struct{}{}, "msp", "postgresql", "v1alpha1", "backup", "delete", "--cluster-id", id, "--backup-id", b.ID)
	if _, err := e.backups.Get(ctx, &pgpb.GetBackupRequest{ClusterId: id, BackupId: b.ID}); status.Code(err) != codes.NotFound {
		t.Errorf("sdk get deleted backup: err = %v, want NotFound", err)
	}
	byCluster = backupList{}
	e.cli.JSON(t, &byCluster, "msp", "postgresql", "v1alpha1", "backup", "list-by-cluster", "--cluster-id", id)
	for _, x := range byCluster.Backups {
		if x.ID == b.ID {
			t.Errorf("deleted backup %s still listed", b.ID)
		}
	}

	// cluster delete, both clusters
	for _, cid := range []string{restoredID, id} {
		e.cli.JSON(t, &struct{}{}, "msp", "postgresql", "v1alpha1", "cluster", "delete", cid)
		if _, err := e.clusters.Get(ctx, &pgpb.GetClusterRequest{Id: cid}); status.Code(err) != codes.NotFound {
			t.Errorf("sdk get deleted cluster %s: err = %v, want NotFound", cid, err)
		}
	}
	list.Clusters = nil
	e.cli.JSON(t, &list, "msp", "postgresql", "v1alpha1", "cluster", "list", "--parent-id", e.project)
	if len(list.Clusters) != 0 {
		t.Errorf("clusters after delete = %+v", list)
	}
	e.fails(t, "NotFound", "msp", "postgresql", "v1alpha1", "cluster", "get", id)
}
