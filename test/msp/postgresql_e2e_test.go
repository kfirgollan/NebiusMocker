//go:build e2e_msp

package msp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	pgsvc "github.com/nebius/gosdk/services/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

const (
	postgresImage = "postgres:16.11-alpine"
	pgUser        = "app_user"
	pgDB          = "appdb"
	pgPassOne     = "First-secret-pw1"
	pgPassTwo     = "Second-secret-pw2"
)

// pgHCL is the PostgreSQL cluster as the provider's documentation writes it,
// with two hosts, a postgresql_config_16 parameter and the transaction pooler.
func pgHCL(project, password string, diskGiB int) string {
	return fmt.Sprintf(`
data "nebius_vpc_v1_network" "default" {
  parent_id = %[1]q
  name      = "default-network"
}

resource "nebius_msp_postgresql_v1alpha1_cluster" "pg" {
  parent_id  = %[1]q
  name       = "e2e-pg"
  network_id = data.nebius_vpc_v1_network.default.id

  config = {
    version = "16"
    template = {
      resources = { platform = "cpu-e2", preset = "2vcpu-8gb" }
      hosts     = { count = 2 }
      disk      = { type = "network-ssd", size_gibibytes = %[5]d }
    }
    pooler_config        = { pooling_mode = "TRANSACTION", max_pool_size = 20 }
    postgresql_config_16 = { statement_timeout = 45000 }
  }

  bootstrap = {
    user_name     = %[3]q
    user_password = %[2]q
    db_name       = %[4]q
  }
}

output "network_id" { value = data.nebius_vpc_v1_network.default.id }
output "read_write" { value = nebius_msp_postgresql_v1alpha1_cluster.pg.status.connection_endpoints.private_read_write }
output "read_only" { value = nebius_msp_postgresql_v1alpha1_cluster.pg.status.connection_endpoints.private_read_only }
`, project, password, pgUser, pgDB, diskGiB)
}

// psql runs psql in the user machine exactly as the Nebius PostgreSQL docs
// show: a connection string with sslmode=verify-full, the CA in
// ~/.postgresql/root.crt, the password in PGPASSWORD.
func psql(ctx context.Context, u *userMachine, endpoint, password, sql string) (string, error) {
	i := strings.LastIndex(endpoint, ":")
	conn := fmt.Sprintf("host=%s port=%s sslmode=verify-full dbname=%s user=%s connect_timeout=10", endpoint[:i], endpoint[i+1:], pgDB, pgUser)
	return u.try(ctx, fmt.Sprintf("PGPASSWORD=%q psql -X -tA -v ON_ERROR_STOP=1 %q -c %q", password, conn, sql))
}

func mustPSQL(t *testing.T, u *userMachine, endpoint, password, sql string) string {
	t.Helper()
	out, err := psql(t.Context(), u, endpoint, password, sql)
	if err != nil {
		t.Fatalf("psql %s: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(out)
}

func TestPostgreSQLScenario(t *testing.T) {
	e := startEnv(t)
	u := startCLIMachine(t, e)

	tf := tfharness.New(t, e.addr, e.inst.Token)
	tf.UseTLSWithoutVerify()
	tf.Write(t, pgHCL(e.inst.ProjectID, pgPassOne, 10))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{"nebius_msp_postgresql_v1alpha1_cluster.pg": "create"})
	tf.Apply(t)
	tf.ExpectNoChanges(t)

	st := tf.State(t)
	clusterID := st["nebius_msp_postgresql_v1alpha1_cluster.pg"]["id"].(string)
	rw, ro := tf.Output(t, "read_write"), tf.Output(t, "read_only")
	if !strings.HasPrefix(rw, "host.docker.internal:") || !strings.HasPrefix(ro, "host.docker.internal:") || rw == ro {
		t.Fatalf("endpoints = %q, %q; want two host.docker.internal addresses", rw, ro)
	}

	// The documented connection: verify-full through the pooler.
	mustPSQL(t, u, rw, pgPassOne, "create table orders (id int primary key, item text)")
	mustPSQL(t, u, rw, pgPassOne, "insert into orders values (1, 'book'), (2, 'pen'), (3, 'ink')")
	if got := mustPSQL(t, u, rw, pgPassOne, "select count(*) from orders"); got != "3" {
		t.Fatalf("rows = %q, want 3", got)
	}
	if got := mustPSQL(t, u, rw, pgPassOne, "show statement_timeout"); got != "45s" {
		t.Errorf("statement_timeout = %q, want 45s (postgresql_config_16.statement_timeout = 45000)", got)
	}
	// The read-only endpoint is a replica that has the rows.
	eventually(t, time.Minute, func() error {
		got, err := psql(t.Context(), u, ro, pgPassOne, "select count(*) from orders")
		if err != nil || strings.TrimSpace(got) != "3" {
			return fmt.Errorf("replica rows = %q: %w", got, err)
		}
		return nil
	})
	if got := mustPSQL(t, u, ro, pgPassOne, "select pg_is_in_recovery()"); got != "t" {
		t.Errorf("read-only endpoint pg_is_in_recovery = %q, want t", got)
	}

	// A password change through Terraform.
	tf.Write(t, pgHCL(e.inst.ProjectID, pgPassTwo, 10))
	tf.ExpectPlan(t, map[string]string{"nebius_msp_postgresql_v1alpha1_cluster.pg": "update"})
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	if rw2 := tf.Output(t, "read_write"); rw2 != rw {
		t.Errorf("read-write endpoint changed with the password: %s -> %s", rw, rw2)
	}
	if out, err := psql(t.Context(), u, rw, pgPassOne, "select 1"); err == nil {
		t.Errorf("the old password still works: %s", out)
	}
	if got := mustPSQL(t, u, rw, pgPassTwo, "select count(*) from orders"); got != "3" {
		t.Errorf("rows after the password change = %q, want 3", got)
	}

	// Backup and restore through the nebius CLI.
	e.cliProfile(t, u)
	u.sh(t, "nebius msp postgresql v1alpha1 backup create --cluster-id "+clusterID)
	backupID := strings.TrimSpace(u.sh(t, "nebius msp postgresql v1alpha1 backup list-by-cluster --cluster-id "+clusterID+" --format json | jq -r '[.backups[] | select(.on_demand)][0].id'"))
	if backupID == "" || backupID == "null" {
		t.Fatalf("backup id = %q", backupID)
	}
	backups, err := pgsvc.NewBackupService(e.sdk).Get(t.Context(), &pgpb.GetBackupRequest{ClusterId: clusterID, BackupId: backupID})
	if err != nil || backups.GetSizeBytes() <= 0 {
		t.Fatalf("Backup.Get = %v, %v; want a backup with data", backups, err)
	}
	mustPSQL(t, u, rw, pgPassTwo, "insert into orders values (4, 'after the backup')")

	network := tf.Output(t, "network_id")
	u.sh(t, fmt.Sprintf(`nebius msp postgresql v1alpha1 cluster restore --source-cluster-id %s --backup-id %s --name e2e-pg-restored `+
		`--network-id %s --bootstrap-user-name %s --bootstrap-db-name %s --bootstrap-user-password %s `+
		`--config-version 16 --config-template-resources-platform cpu-e2 --config-template-resources-preset 2vcpu-8gb `+
		`--config-template-hosts-count 1 --config-template-disk-type network-ssd --config-template-disk-size-gibibytes 20 `+
		`--config-pooler-config-pooling-mode SESSION --config-pooler-config-max-pool-size 10`,
		clusterID, backupID, network, pgUser, pgDB, pgPassTwo))
	restoredID := strings.TrimSpace(u.sh(t, "nebius msp postgresql v1alpha1 cluster get-by-name --parent-id "+e.inst.ProjectID+" --name e2e-pg-restored --format json | jq -r '.metadata.id'"))
	restored, err := pgsvc.NewClusterService(e.sdk).Get(t.Context(), &pgpb.GetClusterRequest{Id: restoredID})
	if err != nil {
		t.Fatalf("Cluster.Get(restored): %v", err)
	}
	if restored.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING {
		t.Fatalf("restored cluster status = %v", restored.GetStatus())
	}
	rrw := restored.GetStatus().GetConnectionEndpoints().GetPrivateReadWrite()
	if got := mustPSQL(t, u, rrw, pgPassTwo, "select count(*) || ':' || max(id) from orders"); got != "3:3" {
		t.Errorf("restored rows = %q, want 3:3 (the data as of the backup, without the row inserted after it)", got)
	}

	// Cleanup of the restored cluster through the same tools.
	u.sh(t, "nebius msp postgresql v1alpha1 cluster delete --id "+restoredID)

	diskFull(t, e, tf, u, clusterID, rw)

	tf.Destroy(t)
	if left := e.d.leftovers(t, "nebius-mocker.backend=msp-postgresql", "nebius-mocker.owner="+e.addr); len(left) > 0 {
		t.Fatalf("containers left after the clusters were deleted: %v", left)
	}
}

// diskFull fills the 10 GiB disk (80 MiB with this suite's disk unit) past its
// limit: writes are refused, reads still work, the cluster is DEGRADED. A 30
// GiB disk through Terraform brings it back.
func diskFull(t *testing.T, e *env, tf *tfharness.Run, u *userMachine, clusterID, rw string) {
	t.Helper()
	clusters := pgsvc.NewClusterService(e.sdk)
	state := func() mspv1alpha1pb.ClusterStatus_State {
		c, err := clusters.Get(t.Context(), &pgpb.GetClusterRequest{Id: clusterID})
		if err != nil {
			t.Fatalf("Cluster.Get: %v", err)
		}
		if c.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING {
			t.Fatalf("phase = %v, a full disk must not change the phase", c.GetStatus().GetPhase())
		}
		return c.GetStatus().GetState()
	}
	if got := state(); got != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("state before filling = %v", got)
	}
	mustPSQL(t, u, rw, pgPassTwo, "create table filler (id int, pad text)")
	mustPSQL(t, u, rw, pgPassTwo, "insert into filler select g, repeat('x', 200) from generate_series(1, 200000) g")

	eventually(t, time.Minute, func() error {
		out, err := psql(t.Context(), u, rw, pgPassTwo, "insert into filler values (-1, 'over the limit')")
		if err == nil {
			return fmt.Errorf("the write still succeeds: %s", out)
		}
		if !strings.Contains(out, "read-only transaction") {
			return fmt.Errorf("write failed, but not as read-only: %w: %s", err, out)
		}
		return nil
	})
	if got := mustPSQL(t, u, rw, pgPassTwo, "select count(*) >= 200000 from filler"); got != "t" {
		t.Errorf("at least 200000 rows readable on a full disk = %q, want t", got)
	}
	eventually(t, time.Minute, func() error {
		if got := state(); got != mspv1alpha1pb.ClusterStatus_STATE_DEGRADED {
			return fmt.Errorf("state = %v, want DEGRADED", got)
		}
		return nil
	})

	// Growing the disk through Terraform.
	tf.Write(t, pgHCL(e.inst.ProjectID, pgPassTwo, 30))
	tf.ExpectPlan(t, map[string]string{"nebius_msp_postgresql_v1alpha1_cluster.pg": "update"})
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	eventually(t, time.Minute, func() error {
		if out, err := psql(t.Context(), u, rw, pgPassTwo, "insert into filler values (-2, 'room again')"); err != nil {
			return fmt.Errorf("write after growing the disk: %w: %s", err, out)
		}
		return nil
	})
	if got := state(); got != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("state after growing the disk = %v, want FINISHED", got)
	}
}
