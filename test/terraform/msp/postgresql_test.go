//go:build e2e

// Package msp runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's
// Managed PostgreSQL service (nebius_msp_postgresql_v1alpha1_cluster, mock
// backend). Nothing is patched or forked.
//
// Run with: go test -tags e2e ./test/terraform/msp/...   (needs terraform on PATH and registry access on first run)
package msp

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"text/template"
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
)

const (
	password1 = "Sup3r-secret-pw"
	password2 = "An0ther-secret-pw"
	resName   = "pg"
	resAddr   = "nebius_msp_postgresql_v1alpha1_cluster.pg"
)

func startPG(t *testing.T) *testkit.Instance {
	t.Helper()
	// A non-zero delay makes operations asynchronous, so the provider's
	// OperationService polling path is exercised too.
	return testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), msppostgresql.New()},
		mockerOptions(testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(200*time.Millisecond),
			testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
}

// cfg describes the rendered Terraform config.
type cfg struct {
	Proj        string
	Description string
	Preset      string
	Hosts       int
	DiskGiB     int
	DiskType    string
	DBName      string
	Password    string
	Pooling     string
	StmtTimeout int // postgresql_config_16.statement_timeout, ms
	MaxConns    int
	Public      bool
	DataSources bool
	Labels      string
}

var cfgTmpl = template.Must(template.New("cfg").Parse(`
data "nebius_vpc_v1_network" "default" {
  parent_id = "{{.Proj}}"
  name      = "default-network"
}

resource "nebius_msp_postgresql_v1alpha1_cluster" "pg" {
  parent_id   = "{{.Proj}}"
  name        = "tf-pg"
  description = "{{.Description}}"
  labels      = { {{.Labels}} }
  network_id  = data.nebius_vpc_v1_network.default.id

  config = {
    version = "16"
    template = {
      resources = {
        platform = "cpu-e2"
        preset   = "{{.Preset}}"
      }
      hosts = { count = {{.Hosts}} }
      disk = {
        type           = "{{.DiskType}}"
        size_gibibytes = {{.DiskGiB}}
      }
    }
    pooler_config = {
      pooling_mode  = "{{.Pooling}}"
      max_pool_size = 50
    }
    postgresql_config_16 = {
      max_connections   = {{.MaxConns}}
      statement_timeout = {{.StmtTimeout}}
    }
{{- if .Public}}
    public_access = true
{{- end}}
  }

  bootstrap = {
    user_name     = "app"
    user_password = "{{.Password}}"
    db_name       = "{{.DBName}}"
  }

  backup = {
    backup_window_start = "03:30:00"
    retention_policy    = "10d"
  }
}
{{if .DataSources}}
data "nebius_msp_postgresql_v1alpha1_cluster" "by_id" {
  id = nebius_msp_postgresql_v1alpha1_cluster.pg.id
}
data "nebius_msp_postgresql_v1alpha1_cluster" "by_name" {
  parent_id = "{{.Proj}}"
  name      = nebius_msp_postgresql_v1alpha1_cluster.pg.name
}
{{end}}`))

func (c *cfg) render(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := cfgTmpl.Execute(&b, c); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func baseCfg(inst *testkit.Instance) cfg {
	return cfg{
		Proj: inst.ProjectID, Description: "orders db", Preset: "2vcpu-8gb", Hosts: 1, DiskGiB: 20, DiskType: "network-ssd",
		DBName: "appdb", Password: password1, Pooling: "SESSION", StmtTimeout: 60000, MaxConns: 100, Labels: `env = "dev"`,
	}
}

// want is what the mock must hold for the cluster after a step.
type want struct {
	preset         string
	cpu            int32
	mem            int64
	hosts          int64
	disk           int64
	db             string
	pooling        pgpb.ConnectionPoolerConfig_PoolingMode
	stmtTimeout    int64
	maxConns       int64
	public         bool
	resourceVerMin int64
	description    string
}

func checkCluster(t *testing.T, step string, clusters pgsvc.ClusterService, inst *testkit.Instance, id string, w want) {
	t.Helper()
	c, err := clusters.Get(t.Context(), &pgpb.GetClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("%s: get cluster %s: %v", step, id, err)
	}
	st, sp := c.GetStatus(), c.GetSpec()
	if st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("%s: status = %v, want RUNNING/FINISHED", step, st)
	}
	if c.GetMetadata().GetName() != "tf-pg" || c.GetMetadata().GetParentId() != inst.ProjectID || c.GetMetadata().GetResourceVersion() < w.resourceVerMin {
		t.Errorf("%s: metadata = %v", step, c.GetMetadata())
	}
	if sp.GetDescription() != w.description || sp.GetNetworkId() == "" {
		t.Errorf("%s: description/network = %q/%q", step, sp.GetDescription(), sp.GetNetworkId())
	}
	cf := sp.GetConfig()
	tpl := cf.GetTemplate()
	if cf.GetVersion() != "16" || tpl.GetResources().GetPlatform() != "cpu-e2" || tpl.GetResources().GetPreset() != w.preset ||
		tpl.GetHosts().GetCount() != w.hosts || tpl.GetDisk().GetSizeGibibytes() != w.disk || tpl.GetDisk().GetType() != "network-ssd" {
		t.Errorf("%s: config = %v", step, cf)
	}
	if st.GetPresetDetails().GetCpuCount() != w.cpu || st.GetPresetDetails().GetMemoryGibibytes() != w.mem {
		t.Errorf("%s: preset details = %v, want %d cpu / %d GiB", step, st.GetPresetDetails(), w.cpu, w.mem)
	}
	if cf.GetPoolerConfig().GetPoolingMode() != w.pooling || cf.GetPoolerConfig().GetMaxPoolSize() != 50 ||
		cf.GetPostgresqlConfig_16().GetStatementTimeout() != w.stmtTimeout || cf.GetPostgresqlConfig_16().GetMaxConnections() != w.maxConns {
		t.Errorf("%s: pooler/postgresql config = %v / %v", step, cf.GetPoolerConfig(), cf.GetPostgresqlConfig_16())
	}
	if cf.GetPublicAccess() != w.public {
		t.Errorf("%s: public_access = %v, want %v", step, cf.GetPublicAccess(), w.public)
	}
	if b := sp.GetBootstrap(); b.GetUserName() != "app" || b.GetDbName() != w.db || b.GetUserPassword() != "" {
		t.Errorf("%s: bootstrap = %v (the password must never be returned)", step, b)
	}
	if bk := sp.GetBackup(); bk.GetBackupWindowStart() != "03:30:00" || bk.GetRetentionPolicy() != "10d" {
		t.Errorf("%s: backup = %v", step, bk)
	}
	ep := st.GetConnectionEndpoints()
	if !strings.HasPrefix(ep.GetPrivateReadWrite(), id+".") || ep.GetPrivateReadOnly() == "" {
		t.Errorf("%s: private endpoints = %v", step, ep)
	}
	if hasPublic := ep.GetPublicReadWrite() != "" && ep.GetPublicReadOnly() != ""; hasPublic != w.public {
		t.Errorf("%s: public endpoints = %v, want present=%v", step, ep, w.public)
	}
}

func clusterState(t *testing.T, tf *tfRun) map[string]any {
	t.Helper()
	attrs, ok := tf.stateAttrs(t)[resAddr]
	if !ok {
		t.Fatal("cluster missing from the Terraform state")
	}
	return attrs
}

func statePassword(t *testing.T, tf *tfRun) any {
	t.Helper()
	b, _ := clusterState(t, tf)["bootstrap"].(map[string]any)
	return b["user_password"]
}

func TestPostgreSQLClusterLifecycle(t *testing.T) {
	inst := startPG(t)
	clusters := pgsvc.NewClusterService(inst.SDK(t))
	tf := newTerraform(t, inst)
	c := baseCfg(inst)

	// Create: the network comes from the project's default network, as a user's config would read it.
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	tf.expectPlan(t, map[string]string{resName: "create"})
	tf.apply(t)
	tf.expectNoChanges(t)
	id1 := tf.stateIDs(t)[resName]
	if id1 == "" {
		t.Fatal("no cluster ID in the state")
	}
	checkCluster(t, "create", clusters, inst, id1, want{
		preset: "2vcpu-8gb", cpu: 2, mem: 8, hosts: 1, disk: 20, db: "appdb", pooling: pgpb.ConnectionPoolerConfig_SESSION,
		stmtTimeout: 60000, maxConns: 100, resourceVerMin: 1, description: "orders db",
	})
	if pw := statePassword(t, tf); pw != password1 {
		t.Errorf("state user_password = %v, want the configured value", pw)
	}

	// In-place update: preset, hosts, disk, a postgresql parameter, pooler mode, the password and public access.
	c.Preset, c.Hosts, c.DiskGiB, c.StmtTimeout, c.Pooling, c.Password, c.Public = "4vcpu-16gb", 2, 30, 120000, "TRANSACTION", password2, true
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{resName: "update"})
	tf.apply(t)
	tf.expectNoChanges(t)
	if got := tf.stateIDs(t)[resName]; got != id1 {
		t.Fatalf("in-place update changed the ID: %s -> %s", id1, got)
	}
	checkCluster(t, "update", clusters, inst, id1, want{
		preset: "4vcpu-16gb", cpu: 4, mem: 16, hosts: 2, disk: 30, db: "appdb", pooling: pgpb.ConnectionPoolerConfig_TRANSACTION,
		stmtTimeout: 120000, maxConns: 100, public: true, resourceVerMin: 2, description: "orders db",
	})
	if pw := statePassword(t, tf); pw != password2 {
		t.Errorf("state user_password after update = %v, want the new value", pw)
	}

	// An immutable field plans as a replacement: the description.
	c.Description = "orders db v2"
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{resName: "replace"})
	tf.apply(t)
	tf.expectNoChanges(t)
	id2 := tf.stateIDs(t)[resName]
	if id2 == "" || id2 == id1 {
		t.Fatalf("description change did not replace the cluster: %s -> %s", id1, id2)
	}
	if _, err := clusters.Get(t.Context(), &pgpb.GetClusterRequest{Id: id1}); status.Code(err) != codes.NotFound {
		t.Errorf("replaced cluster %s: get err = %v, want NotFound", id1, err)
	}
	checkCluster(t, "replace(description)", clusters, inst, id2, want{
		preset: "4vcpu-16gb", cpu: 4, mem: 16, hosts: 2, disk: 30, db: "appdb", pooling: pgpb.ConnectionPoolerConfig_TRANSACTION,
		stmtTimeout: 120000, maxConns: 100, public: true, resourceVerMin: 1, description: "orders db v2",
	})

	// Replacement: bootstrap.db_name.
	c.DBName = "salesdb"
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{resName: "replace"})
	tf.apply(t)
	tf.expectNoChanges(t)
	id3 := tf.stateIDs(t)[resName]
	if id3 == "" || id3 == id2 {
		t.Fatalf("db_name change did not replace the cluster: %s -> %s", id2, id3)
	}
	if _, err := clusters.Get(t.Context(), &pgpb.GetClusterRequest{Id: id2}); status.Code(err) != codes.NotFound {
		t.Errorf("replaced cluster %s: get err = %v, want NotFound", id2, err)
	}
	checkCluster(t, "replace(db_name)", clusters, inst, id3, want{
		preset: "4vcpu-16gb", cpu: 4, mem: 16, hosts: 2, disk: 30, db: "salesdb", pooling: pgpb.ConnectionPoolerConfig_TRANSACTION,
		stmtTimeout: 120000, maxConns: 100, public: true, resourceVerMin: 1, description: "orders db v2",
	})

	// Data sources by ID and by name (parent_id + name) agree with the resource.
	c.DataSources = true
	tf.writeConfig(t, c.render(t))
	tf.apply(t)
	tf.expectNoChanges(t)
	state := tf.stateAttrs(t)
	byID, byName := state["data.nebius_msp_postgresql_v1alpha1_cluster.by_id"], state["data.nebius_msp_postgresql_v1alpha1_cluster.by_name"]
	if byID == nil || byName == nil {
		t.Fatalf("data sources missing from the state: %v", state)
	}
	if byID["id"] != id3 || byName["id"] != id3 {
		t.Errorf("data source ids = %v / %v, want %s", byID["id"], byName["id"], id3)
	}
	if !reflect.DeepEqual(byID, byName) {
		t.Errorf("data sources by id and by name differ:\n%v\n%v", byID, byName)
	}
	resAttrs := clusterState(t, tf)
	for _, k := range []string{"name", "parent_id", "description", "network_id", "labels", "status"} {
		if !reflect.DeepEqual(byID[k], resAttrs[k]) {
			t.Errorf("data source %s = %v, resource has %v", k, byID[k], resAttrs[k])
		}
	}
	dsConfig, _ := byID["config"].(map[string]any)
	resConfig, _ := resAttrs["config"].(map[string]any)
	for _, k := range []string{"template", "version", "public_access", "pooler_config", "postgresql_config_16"} {
		if !reflect.DeepEqual(dsConfig[k], resConfig[k]) {
			t.Errorf("data source config.%s = %v, resource has %v", k, dsConfig[k], resConfig[k])
		}
	}
	dsBootstrap, _ := byID["bootstrap"].(map[string]any)
	if dsBootstrap["user_name"] != "app" || dsBootstrap["db_name"] != "salesdb" || dsBootstrap["user_password"] != nil {
		t.Errorf("data source bootstrap = %v, want user_name and db_name only", dsBootstrap)
	}

	// Destroy removes the cluster from the mock.
	tf.destroy(t)
	if _, err := clusters.Get(t.Context(), &pgpb.GetClusterRequest{Id: id3}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy: get err = %v, want NotFound", err)
	}
	l, err := clusters.List(t.Context(), &pgpb.ListClustersRequest{ParentId: inst.ProjectID})
	if err != nil || len(l.GetClusters()) != 0 {
		t.Errorf("after destroy: list = %v, %v", l, err)
	}
}

// TestPostgreSQLInvalidPresetFails applies an unknown preset: the mock's
// validation error reaches the user and no cluster is left behind.
func TestPostgreSQLInvalidPresetFails(t *testing.T) {
	inst := startPG(t)
	tf := newTerraform(t, inst)
	c := baseCfg(inst)
	c.Preset = "3vcpu-7gb"
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	if err := tf.tf.Apply(t.Context()); err == nil {
		t.Fatal("apply with an invalid preset succeeded")
	}
	if out := tf.out.String(); !strings.Contains(out, "3vcpu-7gb") {
		t.Errorf("apply error does not name the preset:\n%s", out)
	}
	l, err := pgsvc.NewClusterService(inst.SDK(t)).List(t.Context(), &pgpb.ListClustersRequest{ParentId: inst.ProjectID})
	if err != nil || len(l.GetClusters()) != 0 {
		t.Errorf("a cluster exists after the failed apply: %v, %v", l, err)
	}
}

// TestPostgreSQLViewerCannotApply: a viewer token reads but cannot create.
func TestPostgreSQLViewerCannotApply(t *testing.T) {
	inst := startPG(t)
	tf := newTerraformAs(t, inst, "nmock-dev-viewer")
	c := baseCfg(inst)
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	if err := tf.tf.Apply(t.Context()); err == nil {
		t.Fatal("a viewer created a cluster")
	}
	if out := strings.ToLower(tf.out.String()); !strings.Contains(out, "permission") {
		t.Errorf("apply error is not a permission error:\n%s", out)
	}
}
