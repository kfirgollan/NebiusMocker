//go:build e2e

// Package capacity runs the official Terraform provider (nebius/nebius from
// the Terraform Registry) against NebiusMocker's capacity service: the
// capacity block group data source and the capacity allowance resource and
// data source (project limits). The mock's state is checked with the official
// Go SDK after every step.
//
// Run with: make e2e
package capacity

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	capacitysvc "github.com/nebius/gosdk/services/nebius/capacity/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/capacity"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

const (
	resAllowance = "nebius_capacity_v1_capacity_allowance.dev"
	dsH100ByAff  = "data.nebius_capacity_v1_capacity_block_group.h100_by_affinity"
	dsH100ByID   = "data.nebius_capacity_v1_capacity_block_group.h100_by_id"
	dsH200ByAff  = "data.nebius_capacity_v1_capacity_block_group.h200_by_affinity"
	dsAllowByID  = "data.nebius_capacity_v1_capacity_allowance.by_id"
	dsAllowByGrp = "data.nebius_capacity_v1_capacity_allowance.by_group"
)

// groups renders the block group data sources.
func groups(tenantID string) string {
	return fmt.Sprintf(`
data "nebius_capacity_v1_capacity_block_group" "h100_by_affinity" {
  by_resource_affinity = {
    parent_id = %[1]q
    region    = "eu-north1"
    resource_affinity = {
      compute_v1 = { platform = "gpu-h100-sxm", fabric = "fabric-2" }
    }
  }
}

data "nebius_capacity_v1_capacity_block_group" "h100_by_id" {
  id = data.nebius_capacity_v1_capacity_block_group.h100_by_affinity.id
}

data "nebius_capacity_v1_capacity_block_group" "h200_by_affinity" {
  by_resource_affinity = {
    parent_id = %[1]q
    region    = "eu-north1"
    resource_affinity = {
      compute_v1 = { platform = "gpu-h200-sxm", fabric = "fabric-7" }
    }
  }
}
`, tenantID)
}

// allowanceHCL renders the managed allowance and its two data sources. limit
// is omitted when empty (unlimited); group names the block group data source
// the allowance is attached to.
func allowanceHCL(projectID, group, limit, labels string) string {
	limitLine := ""
	if limit != "" {
		limitLine = "limit                   = " + limit
	}
	return fmt.Sprintf(`
resource "nebius_capacity_v1_capacity_allowance" "dev" {
  parent_id               = %[1]q
  name                    = "dev-allowance"
  capacity_block_group_id = data.nebius_capacity_v1_capacity_block_group.%[2]s.id
  %[3]s
  labels                  = { %[4]s }
}

data "nebius_capacity_v1_capacity_allowance" "by_id" {
  id = nebius_capacity_v1_capacity_allowance.dev.id
}

data "nebius_capacity_v1_capacity_allowance" "by_group" {
  by_parent_and_capacity_block_group = {
    parent_id               = %[1]q
    capacity_block_group_id = nebius_capacity_v1_capacity_allowance.dev.capacity_block_group_id
  }
  depends_on = [nebius_capacity_v1_capacity_allowance.dev]
}
`, projectID, group, limitLine, labels)
}

func affinityReq(tenantID, platform, fabric string) *capacitypb.GetCapacityBlockGroupByResourceAffinityRequest {
	return &capacitypb.GetCapacityBlockGroupByResourceAffinityRequest{
		ParentId: tenantID, Region: "eu-north1",
		ResourceAffinity: &capacitypb.ResourceAffinity{Versions: &capacitypb.ResourceAffinity_ComputeV1{
			ComputeV1: &capacitypb.ResourceAffinityComputeV1{Platform: platform, Fabric: fabric},
		}},
	}
}

// TestCapacityAllowanceCRUD: read block groups, create the allowance, update
// its limit above the group size, remove the limit, move it to another group
// and destroy it, as the editor under enforced authz.
func TestCapacityAllowanceCRUD(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), capacity.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	sdk := inst.SDK(t)
	groupsSvc := capacitysvc.NewCapacityBlockGroupService(sdk)
	allowances := capacitysvc.NewCapacityAllowanceService(sdk)
	tf := tfharness.New(t, inst.Addr, config.DefaultEditorToken)

	h100, err := groupsSvc.GetByResourceAffinity(t.Context(), affinityReq(inst.TenantID, "gpu-h100-sxm", "fabric-2"))
	if err != nil {
		t.Fatalf("gosdk GetByResourceAffinity h100: %v", err)
	}
	h200, err := groupsSvc.GetByResourceAffinity(t.Context(), affinityReq(inst.TenantID, "gpu-h200-sxm", "fabric-7"))
	if err != nil {
		t.Fatalf("gosdk GetByResourceAffinity h200: %v", err)
	}

	cfg := func(group, limit, labels string) string {
		return groups(inst.TenantID) + allowanceHCL(inst.ProjectID, group, limit, labels)
	}

	// a. block group data sources, before any resource exists.
	tf.Write(t, groups(inst.TenantID))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{}) // data sources are read during plan
	tf.Apply(t)                           // and saved to state
	state := tf.State(t)
	assertGroup(t, state, dsH100ByAff, h100)
	assertGroup(t, state, dsH100ByID, h100)
	assertGroup(t, state, dsH200ByAff, h200)
	if lim := h100.GetStatus().GetCurrentLimit(); lim != 16 {
		t.Fatalf("seeded h100 group current_limit = %d, want 16", lim)
	}

	// b. create with limit 4.
	tf.Write(t, cfg("h100_by_affinity", "4", `env = "dev"`))
	tf.ExpectPlan(t, map[string]string{resAllowance: "create"})
	tf.Apply(t)
	state = tf.State(t)
	id, _ := state[resAllowance]["id"].(string)
	got := getAllowance(t, allowances, id)
	if got.GetSpec().Limit == nil || got.GetSpec().GetLimit() != 4 || got.GetSpec().GetCapacityBlockGroupId() != h100.GetMetadata().GetId() ||
		got.GetMetadata().GetParentId() != inst.ProjectID || got.GetMetadata().GetName() != "dev-allowance" ||
		!maps.Equal(got.GetMetadata().GetLabels(), map[string]string{"env": "dev"}) {
		t.Fatalf("mock allowance after create = %v", got)
	}
	version := got.GetMetadata().GetResourceVersion()
	assertAllowanceData(t, state, got)
	tf.ExpectNoChanges(t)

	// c. in-place update: limit above the group size, labels change.
	tf.Write(t, cfg("h100_by_affinity", "40", `env = "prod", team = "ml"`))
	tf.ExpectPlan(t, map[string]string{resAllowance: "update"})
	tf.Apply(t)
	state = tf.State(t)
	if state[resAllowance]["id"] != id {
		t.Fatalf("allowance recreated: %s -> %v", id, state[resAllowance]["id"])
	}
	got = getAllowance(t, allowances, id)
	if got.GetSpec().Limit == nil || got.GetSpec().GetLimit() != 40 ||
		!maps.Equal(got.GetMetadata().GetLabels(), map[string]string{"env": "prod", "team": "ml"}) {
		t.Fatalf("mock allowance after update = %v", got)
	}
	if v := got.GetMetadata().GetResourceVersion(); v <= version {
		t.Errorf("resource_version = %d, want > %d", v, version)
	}
	version = got.GetMetadata().GetResourceVersion()
	assertAllowanceData(t, state, got)
	tf.ExpectNoChanges(t)

	// d. remove limit: unlimited.
	tf.Write(t, cfg("h100_by_affinity", "", `env = "prod", team = "ml"`))
	tf.ExpectPlan(t, map[string]string{resAllowance: "update"})
	tf.Apply(t)
	state = tf.State(t)
	got = getAllowance(t, allowances, id)
	if got.GetSpec().Limit != nil {
		t.Fatalf("limit after removal = %d, want unset (unlimited): %v", got.GetSpec().GetLimit(), got)
	}
	if v := got.GetMetadata().GetResourceVersion(); v <= version {
		t.Errorf("resource_version = %d, want > %d", v, version)
	}
	// e. data sources by id and by parent + block group.
	assertAllowanceData(t, state, got)
	tf.ExpectNoChanges(t)

	// f. move to the h200 group, in place.
	tf.Write(t, cfg("h200_by_affinity", "8", `env = "prod", team = "ml"`))
	tf.ExpectPlan(t, map[string]string{resAllowance: "update"})
	tf.Apply(t)
	state = tf.State(t)
	if state[resAllowance]["id"] != id {
		t.Fatalf("allowance recreated on move: %s -> %v", id, state[resAllowance]["id"])
	}
	got = getAllowance(t, allowances, id)
	if got.GetSpec().GetCapacityBlockGroupId() != h200.GetMetadata().GetId() || got.GetSpec().GetLimit() != 8 {
		t.Fatalf("mock allowance after move = %v, want group %s limit 8", got, h200.GetMetadata().GetId())
	}
	assertAllowanceData(t, state, got)
	tf.ExpectNoChanges(t)

	// g. destroy: the group's allowance is the default one again.
	tf.Destroy(t)
	if _, err := allowances.Get(t.Context(), &capacitypb.GetCapacityAllowanceRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get after destroy: %v, want NotFound", err)
	}
	def, err := allowances.GetByParentAndCapacityBlockGroup(t.Context(), &capacitypb.GetCapacityAllowanceByParentAndCapacityBlockGroupRequest{
		ParentId: inst.ProjectID, CapacityBlockGroupId: h200.GetMetadata().GetId(),
	})
	if err != nil {
		t.Fatalf("GetByParentAndCapacityBlockGroup after destroy: %v", err)
	}
	if def.GetMetadata().GetId() != "" || def.GetSpec().Limit != nil {
		t.Fatalf("allowance after destroy = %v, want the default one (no id, no limit)", def)
	}
	if st := tf.State(t); len(st) != 0 {
		t.Fatalf("state after destroy = %v, want empty", st)
	}
}

// TestViewerCannotCreateAllowance: a viewer may read block groups, but the
// official provider's apply fails with a permission error on create.
func TestViewerCannotCreateAllowance(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), capacity.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	// terraform is run directly because tfharness.Apply fails the test on error.
	bin, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not found on PATH")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "terraform.rc")
	body := fmt.Sprintf(`terraform {
  required_providers {
    nebius = {
      source  = "nebius/nebius"
      version = "= %s"
    }
  }
}

provider "nebius" {
  resolvers       = { "*" = %q }
  address_options = { "*" = { insecure = true } }
  token           = %q
}
`, tfharness.ProviderVersion, inst.Addr, config.DefaultViewerToken) + groups(inst.TenantID) + allowanceHCL(inst.ProjectID, "h100_by_affinity", "4", `env = "dev"`)
	for path, content := range map[string]string{cli: "# official registry only\n", filepath.Join(dir, "main.tf"): body} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	env := []string{
		"PATH=" + filepath.Dir(bin) + ":/usr/bin:/bin",
		"HOME=" + home, // tfenv reads its selected version from ~/.config/tfenv
		"TF_CLI_CONFIG_FILE=" + cli,
		"CHECKPOINT_DISABLE=1",
		"TF_REGISTRY_CLIENT_TIMEOUT=60",
		"TF_IN_AUTOMATION=1",
	}
	for k, v := range tfcache.Env(t) {
		env = append(env, k+"="+v)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("init", "-no-color"); err != nil {
		t.Fatalf("terraform init: %v\n%s", err, out)
	}
	out, err := run("apply", "-auto-approve", "-no-color")
	if err == nil {
		t.Fatalf("terraform apply as viewer succeeded:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "permission") {
		t.Fatalf("apply error does not mention a permission failure:\n%s", out)
	}
	// Nothing was created.
	sdk := inst.SDK(t)
	g, err := capacitysvc.NewCapacityBlockGroupService(sdk).GetByResourceAffinity(t.Context(), affinityReq(inst.TenantID, "gpu-h100-sxm", "fabric-2"))
	if err != nil {
		t.Fatalf("GetByResourceAffinity: %v", err)
	}
	resp, err := capacitysvc.NewCapacityAllowanceService(sdk).ListByCapacityBlockGroup(t.Context(),
		&capacitypb.ListCapacityAllowancesByCapacityBlockGroupRequest{CapacityBlockGroupId: g.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("ListByCapacityBlockGroup: %v", err)
	}
	if n := len(resp.GetItems()); n != 0 {
		t.Fatalf("allowances after the failed apply = %d, want 0", n)
	}
}

// TestMain gives this binary a private Terraform plugin cache (see tfcache).
func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }

func getAllowance(t *testing.T, svc capacitysvc.CapacityAllowanceService, id string) *capacitypb.CapacityAllowance {
	t.Helper()
	if id == "" {
		t.Fatal("allowance id missing from terraform state")
	}
	a, err := svc.Get(t.Context(), &capacitypb.GetCapacityAllowanceRequest{Id: id})
	if err != nil {
		t.Fatalf("Get(%s) from mock: %v", id, err)
	}
	return a
}

// assertGroup compares a block group data source with the mock's group.
func assertGroup(t *testing.T, state map[string]map[string]any, addr string, want *capacitypb.CapacityBlockGroup) {
	t.Helper()
	ds := state[addr]
	if ds["id"] != want.GetMetadata().GetId() {
		t.Errorf("%s.id = %v, want %s", addr, ds["id"], want.GetMetadata().GetId())
	}
	st, _ := ds["status"].(map[string]any)
	if lim, ok := number(st["current_limit"]); !ok || lim != want.GetStatus().GetCurrentLimit() {
		t.Errorf("%s.status.current_limit = %v, want %d", addr, st["current_limit"], want.GetStatus().GetCurrentLimit())
	}
	if st["state"] != want.GetStatus().GetState().String() && st["state"] != strings.TrimPrefix(want.GetStatus().GetState().String(), "STATE_") {
		t.Errorf("%s.status.state = %v, want %s", addr, st["state"], want.GetStatus().GetState())
	}
	aff, _ := st["resource_affinity"].(map[string]any)
	c1, _ := aff["compute_v1"].(map[string]any)
	wantAff := want.GetStatus().GetResourceAffinity().GetComputeV1()
	if c1["platform"] != wantAff.GetPlatform() || c1["fabric"] != wantAff.GetFabric() {
		t.Errorf("%s.status.resource_affinity = %v, want %v", addr, aff, wantAff)
	}
}

// assertAllowanceData checks the managed resource and both data sources
// against the mock's allowance.
func assertAllowanceData(t *testing.T, state map[string]map[string]any, want *capacitypb.CapacityAllowance) {
	t.Helper()
	for _, addr := range []string{resAllowance, dsAllowByID, dsAllowByGrp} {
		ds := state[addr]
		if ds["id"] != want.GetMetadata().GetId() {
			t.Errorf("%s.id = %v, want %s", addr, ds["id"], want.GetMetadata().GetId())
		}
		if ds["capacity_block_group_id"] != want.GetSpec().GetCapacityBlockGroupId() {
			t.Errorf("%s.capacity_block_group_id = %v, want %s", addr, ds["capacity_block_group_id"], want.GetSpec().GetCapacityBlockGroupId())
		}
		limit, has := number(ds["limit"])
		if has != (want.GetSpec().Limit != nil) || (has && limit != want.GetSpec().GetLimit()) {
			t.Errorf("%s.limit = %v, want %s", addr, ds["limit"], limitString(want))
		}
	}
}

// number reads a non-negative integer attribute of the Terraform JSON state.
func number(v any) (uint64, bool) {
	switch n := v.(type) {
	case json.Number:
		u, err := strconv.ParseUint(n.String(), 10, 64)
		return u, err == nil
	case float64:
		return uint64(n), n >= 0
	default:
		return 0, false
	}
}

func limitString(a *capacitypb.CapacityAllowance) string {
	if a.GetSpec().Limit == nil {
		return "unset"
	}
	return strconv.FormatUint(a.GetSpec().GetLimit(), 10)
}
