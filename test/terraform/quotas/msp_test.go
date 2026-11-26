//go:build e2e

package quotas

import (
	"fmt"
	"strings"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"
	pgsvc "github.com/nebius/gosdk/services/nebius/msp/postgresql/v1alpha1"
	quotassvc "github.com/nebius/gosdk/services/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

// pgClusterConfig manages the project's msp.postgresql.cluster.count
// allowance and a Managed PostgreSQL cluster (mock backend) created after it.
func pgClusterConfig(projectID string, limit uint64) string {
	return fmt.Sprintf(`
resource "nebius_quotas_v1_quota_allowance" "pg_clusters" {
  parent_id = %[1]q
  name      = %[2]q
  limit     = %[3]d
}

resource "nebius_msp_postgresql_v1alpha1_cluster" "pg" {
  parent_id = %[1]q
  name      = "over-quota"
  config = {
    version = "16"
    template = {
      resources = {
        platform = "cpu-e2"
        preset   = "2vcpu-8gb"
      }
      hosts = { count = 1 }
      disk = {
        type           = "network-ssd"
        size_gibibytes = 20
      }
    }
  }
  bootstrap = {
    user_name     = "app"
    user_password = "Sup3r-secret-pw"
    db_name       = "appdb"
  }
  depends_on = [nebius_quotas_v1_quota_allowance.pg_clusters]
}
`, projectID, quota.MSPPostgreSQLClusterCount, limit)
}

// TestLoweredAllowanceRefusesPostgreSQLCluster lowers
// msp.postgresql.cluster.count to the current usage with Terraform: the
// cluster in the same configuration fails with RESOURCE_EXHAUSTED naming the
// quota; raising the allowance lets the same apply succeed, the usage shows
// the cluster and its vCPUs, and destroy releases them.
func TestLoweredAllowanceRefusesPostgreSQLCluster(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), quotas.New(), msppostgresql.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(200*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	svc := quotassvc.NewQuotaAllowanceService(inst.SDK(t))
	clusters := pgsvc.NewClusterService(inst.SDK(t))
	allowance := func(name string) *quotaspb.QuotaAllowance {
		t.Helper()
		a, err := svc.GetByName(t.Context(), &quotaspb.GetByNameRequest{ParentId: inst.ProjectID, Name: name, Region: region})
		if err != nil {
			t.Fatalf("GetByName(%s): %v", name, err)
		}
		return a
	}
	usage := allowance(quota.MSPPostgreSQLClusterCount).GetStatus().GetUsage()
	tf := tfharness.New(t, inst.Addr, config.DefaultDevToken)

	tf.Write(t, pgClusterConfig(inst.ProjectID, usage))
	tf.Init(t)
	err := tf.ApplyError(t)
	if err == nil {
		t.Fatal("apply with the allowance at its usage succeeded, want a quota error")
	}
	for _, want := range []string{"ResourceExhausted", quota.MSPPostgreSQLClusterCount} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("apply error does not mention %q:\n%v", want, err)
		}
	}
	if _, err := clusters.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: inst.ProjectID, Name: "over-quota"}); err == nil {
		t.Fatal("the refused cluster exists in the mock")
	}
	if got := allowance(quota.MSPPostgreSQLClusterCount); got.GetSpec().GetLimit() != usage || got.GetStatus().GetUsage() != usage {
		t.Fatalf("allowance after refused apply = %v", got)
	}

	tf.Write(t, pgClusterConfig(inst.ProjectID, usage+1))
	if err := tf.ApplyError(t); err != nil {
		t.Fatalf("apply after raising the allowance: %v", err)
	}
	c, err := clusters.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: inst.ProjectID, Name: "over-quota"})
	if err != nil {
		t.Fatalf("cluster after apply: %v", err)
	}
	if c.GetSpec().GetConfig().GetTemplate().GetResources().GetPreset() != "2vcpu-8gb" {
		t.Errorf("cluster spec = %v", c.GetSpec())
	}
	if got := allowance(quota.MSPPostgreSQLClusterCount).GetStatus().GetUsage(); got != usage+1 {
		t.Fatalf("cluster usage = %d, want %d", got, usage+1)
	}
	if got := allowance(quota.MSPPostgreSQLVCPU).GetStatus().GetUsage(); got != 2 {
		t.Fatalf("vCPU usage = %d, want 2", got)
	}
	tf.ExpectNoChanges(t)
	tf.Destroy(t)
	if _, err := clusters.Get(t.Context(), &pgpb.GetClusterRequest{Id: c.GetMetadata().GetId()}); err == nil {
		t.Error("the cluster survived destroy")
	}
	if got := allowance(quota.MSPPostgreSQLVCPU).GetStatus().GetUsage(); got != 0 {
		t.Errorf("vCPU usage after destroy = %d, want 0", got)
	}
}
