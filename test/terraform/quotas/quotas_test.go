//go:build e2e

// Package quotas runs the official Terraform provider (nebius/nebius from the
// Terraform Registry) against NebiusMocker's quota allowances and checks the
// mock's state with the official Go SDK after every step.
//
// Run with: make e2e
package quotas

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"
	quotassvc "github.com/nebius/gosdk/services/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

const (
	resAddr  = "nebius_quotas_v1_quota_allowance.networks"
	dataByID = "data.nebius_quotas_v1_quota_allowance.by_id"
	dataByNm = "data.nebius_quotas_v1_quota_allowance.by_name"
	region   = "eu-north1"
)

// allowanceConfig manages the project's vpc.network.count allowance and reads
// it back through both data source getters. regionAttr is "" (the project's
// region) or a region = "…" line.
func allowanceConfig(projectID string, limit int, regionAttr, labels string) string {
	return fmt.Sprintf(`
resource "nebius_quotas_v1_quota_allowance" "networks" {
  parent_id = %[1]q
  name      = %[2]q
  limit     = %[3]d
  %[4]s
  labels    = { %[5]s }
}

data "nebius_quotas_v1_quota_allowance" "by_id" {
  id = nebius_quotas_v1_quota_allowance.networks.id
}

data "nebius_quotas_v1_quota_allowance" "by_name" {
  by_name_in_region = {
    parent_id = %[1]q
    name      = %[2]q
    region    = %[6]q
  }
  depends_on = [nebius_quotas_v1_quota_allowance.networks]
}
`, projectID, quota.VPCNetworkCount, limit, regionAttr, labels, region)
}

func start(t *testing.T) (*testkit.Instance, quotassvc.QuotaAllowanceService) {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), quotas.New()},
		testkit.WithOperationDelay(300*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	return inst, quotassvc.NewQuotaAllowanceService(inst.SDK(t))
}

// TestQuotaAllowanceCRUD: create → empty plan → in-place limit and label
// updates → empty plan → replace on region → destroy (resets the limit), as a
// tenant admin (only admins change quotas) under enforced authz.
func TestQuotaAllowanceCRUD(t *testing.T) {
	inst, svc := start(t)
	tf := tfharness.New(t, inst.Addr, config.DefaultDevToken)

	tf.Write(t, allowanceConfig(inst.ProjectID, 20, "", `team = "net"`))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{resAddr: "create"})
	tf.Apply(t)
	id, _ := tf.State(t)[resAddr]["id"].(string)
	got := get(t, svc, id)
	if got.GetSpec().GetLimit() != 20 || got.GetMetadata().GetName() != quota.VPCNetworkCount ||
		got.GetMetadata().GetParentId() != inst.ProjectID || got.GetMetadata().GetLabels()["team"] != "net" {
		t.Fatalf("mock allowance after create = %v", got)
	}
	assertData(t, tf.State(t), id, 20)
	tf.ExpectNoChanges(t)

	for i, st := range []struct {
		limit  int
		labels string
	}{{25, `team = "net"`}, {25, `team = "net", tier = "1"`}, {5, ``}} {
		tf.Write(t, allowanceConfig(inst.ProjectID, st.limit, "", st.labels))
		tf.ExpectPlan(t, map[string]string{resAddr: "update"})
		tf.Apply(t)
		if newID := tf.State(t)[resAddr]["id"]; newID != id {
			t.Fatalf("step %d: allowance recreated: %s → %v", i, id, newID)
		}
		got := get(t, svc, id)
		if got.GetSpec().GetLimit() != uint64(st.limit) || len(got.GetMetadata().GetLabels()) != strings.Count(st.labels, "=") {
			t.Fatalf("step %d: mock allowance = %v", i, got)
		}
		assertData(t, tf.State(t), id, st.limit)
		tf.ExpectNoChanges(t)
	}

	// spec.region is immutable: naming it replaces the resource. Create is an
	// upsert of the same (project, quota, region) allowance, so the ID stays.
	tf.Write(t, allowanceConfig(inst.ProjectID, 5, fmt.Sprintf("region = %q", region), ``))
	tf.ExpectPlan(t, map[string]string{resAddr: "replace"})
	tf.Apply(t)
	if newID := tf.State(t)[resAddr]["id"]; newID != id {
		t.Fatalf("replace: id %s → %v, want the same allowance", id, newID)
	}
	if got := get(t, svc, id); got.GetSpec().GetLimit() != 5 || got.GetSpec().GetRegion() != region {
		t.Fatalf("after replace = %v", got)
	}
	tf.ExpectNoChanges(t)

	// Destroy resets the limit; the allowance itself remains (as in Nebius).
	tf.Destroy(t)
	if got := get(t, svc, id); got.GetSpec().GetLimit() != 1000 {
		t.Fatalf("after destroy limit = %d, want the default 1000", got.GetSpec().GetLimit())
	}
	if st := tf.State(t); len(st) != 0 {
		t.Fatalf("state after destroy = %v, want empty", st)
	}
}

// TestLoweredAllowanceRefusesCreate lowers vpc.network.count to the current
// usage with Terraform, then a network in the same configuration fails with
// RESOURCE_EXHAUSTED and a QuotaFailure for that quota; raising it again lets
// the same apply succeed.
func TestLoweredAllowanceRefusesCreate(t *testing.T) {
	inst, svc := start(t)
	usage := getByName(t, svc, inst.ProjectID).GetStatus().GetUsage()
	tf := tfharness.New(t, inst.Addr, config.DefaultDevToken)
	cfg := func(limit uint64) string {
		return fmt.Sprintf(`
resource "nebius_quotas_v1_quota_allowance" "networks" {
  parent_id = %[1]q
  name      = %[2]q
  limit     = %[3]d
}

resource "nebius_vpc_v1_network" "extra" {
  parent_id  = %[1]q
  name       = "over-quota"
  depends_on = [nebius_quotas_v1_quota_allowance.networks]
}
`, inst.ProjectID, quota.VPCNetworkCount, limit)
	}
	tf.Write(t, cfg(usage))
	tf.Init(t)
	err := tf.ApplyError(t)
	if err == nil {
		t.Fatal("apply with the allowance at its usage succeeded, want a quota error")
	}
	for _, want := range []string{"ResourceExhausted", quota.VPCNetworkCount} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("apply error does not mention %q:\n%v", want, err)
		}
	}
	if got := getByName(t, svc, inst.ProjectID); got.GetSpec().GetLimit() != usage || got.GetStatus().GetUsage() != usage {
		t.Fatalf("allowance after refused apply = %v", got)
	}

	tf.Write(t, cfg(usage+1))
	if err := tf.ApplyError(t); err != nil {
		t.Fatalf("apply after raising the allowance: %v", err)
	}
	if got := getByName(t, svc, inst.ProjectID); got.GetStatus().GetUsage() != usage+1 {
		t.Fatalf("usage after the network = %d, want %d", got.GetStatus().GetUsage(), usage+1)
	}
	tf.ExpectNoChanges(t)
	tf.Destroy(t)
}

// TestMain gives this binary a private Terraform plugin cache (see tfcache).
func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }

func get(t *testing.T, svc quotassvc.QuotaAllowanceService, id string) *quotaspb.QuotaAllowance {
	t.Helper()
	if id == "" {
		t.Fatal("allowance id missing from terraform state")
	}
	a, err := svc.Get(t.Context(), &quotaspb.GetQuotaAllowanceRequest{Id: id})
	if err != nil {
		t.Fatalf("Get(%s) from mock: %v", id, err)
	}
	return a
}

func getByName(t *testing.T, svc quotassvc.QuotaAllowanceService, projectID string) *quotaspb.QuotaAllowance {
	t.Helper()
	a, err := svc.GetByName(t.Context(), &quotaspb.GetByNameRequest{ParentId: projectID, Name: quota.VPCNetworkCount, Region: region})
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	return a
}

// assertData checks that both data sources resolved the managed allowance
// with its limit and live status.
func assertData(t *testing.T, state map[string]map[string]any, id string, limit int) {
	t.Helper()
	for _, addr := range []string{dataByID, dataByNm} {
		ds := state[addr]
		if ds["id"] != id {
			t.Errorf("%s.id = %v, want %s", addr, ds["id"], id)
		}
		if fmt.Sprint(ds["limit"]) != strconv.Itoa(limit) {
			t.Errorf("%s.limit = %v, want %d", addr, ds["limit"], limit)
		}
		st, _ := ds["status"].(map[string]any)
		if st["state"] != "STATE_ACTIVE" || st["service"] != "vpc" || st["unit"] != quota.UnitCount {
			t.Errorf("%s.status = %v", addr, st)
		}
	}
}
