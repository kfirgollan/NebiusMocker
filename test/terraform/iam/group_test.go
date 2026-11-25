//go:build e2e

// Package iam runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker.
//
// Nothing is patched or forked: the provider is only pointed at the mock with
// its own configuration (resolvers, address_options, token).
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package iam

import (
	"bytes"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

// providerVersion pins the official provider release under test.
const providerVersion = "0.6.91"

const planFile = "tfplan"

// seededGroups exist in every mock tenant (like Nebius's default groups) and
// are not managed by these configs.
var seededGroups = map[string]bool{"admins": true, "editors": true, "viewers": true}

var mockerLogs = flag.Bool("mocker-logs", false, "print NebiusMocker's request log to the test output")

func mockerOptions(opts ...testkit.Option) []testkit.Option {
	if *mockerLogs {
		opts = append(opts, testkit.WithLogs())
	}
	return opts
}

// TestGroupsLifecycle drives create → add → remove → destroy and checks, after
// every step, both Terraform's plan diff and the mock's actual state.
func TestGroupsLifecycle(t *testing.T) {
	// A non-zero delay makes every operation asynchronous, so the provider's
	// OperationService polling path is exercised, not just synchronous replies.
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, mockerOptions(testkit.WithOperationDelay(300*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	groups := iamsvc.NewGroupService(inst.SDK(t))
	tf := newTerraform(t, inst)

	// Step 0: init, then the first plan creates exactly one group.
	tf.write(t, "alpha")
	tf.init(t)
	tf.expectPlan(t, map[string]string{"alpha": "create"})
	tf.apply(t)
	state := tf.stateIDs(t)
	assertService(t, groups, inst.TenantID, state)
	tf.expectNoChanges(t)
	alphaID := state["alpha"]

	// Step 1: add a second group. Only beta is created; alpha is untouched.
	tf.write(t, "alpha", "beta")
	tf.expectPlan(t, map[string]string{"alpha": "no-op", "beta": "create"})
	tf.apply(t)
	state = tf.stateIDs(t)
	if state["alpha"] != alphaID {
		t.Fatalf("alpha was recreated: id %s → %s", alphaID, state["alpha"])
	}
	assertService(t, groups, inst.TenantID, state)
	tf.expectNoChanges(t)
	betaID := state["beta"]

	// Step 2: remove alpha. Only alpha is deleted; beta is untouched.
	tf.write(t, "beta")
	tf.expectPlan(t, map[string]string{"alpha": "delete", "beta": "no-op"})
	tf.apply(t)
	state = tf.stateIDs(t)
	if state["beta"] != betaID {
		t.Fatalf("beta was recreated: id %s → %s", betaID, state["beta"])
	}
	assertService(t, groups, inst.TenantID, state)
	assertGone(t, groups, alphaID)
	tf.expectNoChanges(t)

	// Step 3: destroy everything.
	tf.destroy(t)
	if got := tf.stateIDs(t); len(got) != 0 {
		t.Fatalf("state after destroy = %v, want empty", got)
	}
	assertService(t, groups, inst.TenantID, map[string]string{})
	assertGone(t, groups, betaID)
}

// TestGroupInPlaceUpdate checks that label and name changes are planned and
// applied in place (Update with X-ResetMask), keep the same ID and leave no drift.
func TestGroupInPlaceUpdate(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, mockerOptions(testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	groups := iamsvc.NewGroupService(inst.SDK(t))
	tf := newTerraform(t, inst)
	group := func(name, labels string) string {
		return fmt.Sprintf(`
resource "nebius_iam_v1_group" "g" {
  parent_id = %q
  name      = %q
  labels    = { %s }
}
`, inst.TenantID, name, labels)
	}

	tf.writeConfig(t, group("engineers", `team = "platform"`))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"g": "create"})
	tf.apply(t)
	id := tf.stateIDs(t)["g"]

	steps := []struct {
		name, labels string
		check        func(*iampb.Group) bool
	}{
		{"engineers", `team = "infra", tier = "1"`, func(g *iampb.Group) bool {
			return g.GetMetadata().GetLabels()["team"] == "infra" && g.GetMetadata().GetLabels()["tier"] == "1"
		}},
		{"engineers", `tier = "1"`, func(g *iampb.Group) bool { // removed label must be cleared (reset mask)
			_, hasTeam := g.GetMetadata().GetLabels()["team"]
			return !hasTeam && g.GetMetadata().GetLabels()["tier"] == "1"
		}},
		{"platform-engineers", `tier = "1"`, func(g *iampb.Group) bool {
			return g.GetMetadata().GetName() == "platform-engineers"
		}},
	}
	for i, st := range steps {
		tf.writeConfig(t, group(st.name, st.labels))
		tf.expectPlan(t, map[string]string{"g": "update"})
		tf.apply(t)
		if got := tf.stateIDs(t)["g"]; got != id {
			t.Fatalf("step %d: group recreated: %s → %s", i, id, got)
		}
		g, err := groups.Get(t.Context(), &iampb.GetGroupRequest{Id: id})
		if err != nil {
			t.Fatalf("step %d: Get: %v", i, err)
		}
		if !st.check(g) {
			t.Fatalf("step %d: mock group = %v, does not reflect the config", i, g.GetMetadata())
		}
		if v := g.GetMetadata().GetResourceVersion(); v != int64(i+2) {
			t.Errorf("step %d: resource_version = %d, want %d", i, v, i+2)
		}
		tf.expectNoChanges(t)
	}
	tf.destroy(t)
	assertGone(t, groups, id)
}

// tfRun wraps one Terraform working directory.
type tfRun struct {
	tf       *tfexec.Terraform
	dir      string
	addr     string
	token    string
	tenantID string
	out      bytes.Buffer
}

func newTerraform(t *testing.T, inst *testkit.Instance) *tfRun {
	t.Helper()
	return newTerraformAs(t, inst, inst.Token)
}

// newTerraformAs is newTerraform with the provider authenticating with token.
func newTerraformAs(t *testing.T, inst *testkit.Instance, token string) *tfRun {
	t.Helper()
	bin, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not found on PATH")
	}
	dir := t.TempDir()
	tf, err := tfexec.NewTerraform(dir, bin)
	if err != nil {
		t.Fatalf("tfexec: %v", err)
	}
	r := &tfRun{tf: tf, dir: dir, addr: inst.Addr, token: token, tenantID: inst.TenantID}
	if err := tf.SetEnv(childEnv(map[string]string{
		"CHECKPOINT_DISABLE":                             "1",
		"TF_PLUGIN_CACHE_DIR":                            tfcache.Dir(t),
		"TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE": "1", // reuse the first download; the cache is private
		"TF_CLI_CONFIG_FILE":                             writeCLIConfig(t, dir),
		"TF_REGISTRY_CLIENT_TIMEOUT":                     "60",
		"NEBIUS_IAM_TOKEN":                               "", // credentials come from the provider block only
	})); err != nil {
		t.Fatalf("tfexec env: %v", err)
	}
	tf.SetStdout(&r.out)
	tf.SetStderr(&r.out)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("terraform output:\n%s", r.out.String())
		}
	})
	return r
}

// write renders main.tf with one nebius_iam_v1_group per name.
func (r *tfRun) write(t *testing.T, names ...string) {
	t.Helper()
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, `
resource "nebius_iam_v1_group" %q {
  parent_id = %q
  name      = %q
}
`, n, r.tenantID, n)
	}
	r.writeConfig(t, b.String())
}

// writeConfig renders main.tf: the provider block followed by resources.
func (r *tfRun) writeConfig(t *testing.T, resources string) {
	t.Helper()
	cfg := fmt.Sprintf(`terraform {
  required_providers {
    nebius = {
      source  = "nebius/nebius"
      version = "= %s"
    }
  }
}

# The official provider, pointed at NebiusMocker through its own settings.
provider "nebius" {
  resolvers       = { "*" = %q }
  address_options = { "*" = { insecure = true } }
  token           = %q
}
`, providerVersion, r.addr, r.token) + resources
	if err := os.WriteFile(filepath.Join(r.dir, "main.tf"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write main.tf: %v", err)
	}
}

func (r *tfRun) init(t *testing.T) {
	t.Helper()
	if err := r.tf.Init(t.Context()); err != nil {
		t.Fatalf("terraform init: %v", err)
	}
}

// expectPlan plans to a file and asserts the action for each resource (by name).
func (r *tfRun) expectPlan(t *testing.T, want map[string]string) {
	t.Helper()
	if _, err := r.tf.Plan(t.Context(), tfexec.Out(planFile)); err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	plan, err := r.tf.ShowPlanFile(t.Context(), planFile)
	if err != nil {
		t.Fatalf("terraform show plan: %v", err)
	}
	got := map[string]string{}
	for _, rc := range plan.ResourceChanges {
		if !strings.HasPrefix(rc.Type, "nebius_") {
			t.Errorf("unexpected resource in plan: %s", rc.Address)
			continue
		}
		got[rc.Name] = actionName(rc.Change.Actions)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("plan actions = %v, want %v", got, want)
	}
}

// expectNoChanges asserts that a fresh plan after apply is empty: what the mock
// returns from Get round-trips through the provider schema without drift.
func (r *tfRun) expectNoChanges(t *testing.T) {
	t.Helper()
	changed, err := r.tf.Plan(t.Context())
	if err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	if changed {
		t.Fatalf("plan after apply is not empty: the mock's responses drift from the applied config")
	}
}

func (r *tfRun) apply(t *testing.T) {
	t.Helper()
	if err := r.tf.Apply(t.Context(), tfexec.DirOrPlan(planFile)); err != nil {
		t.Fatalf("terraform apply: %v", err)
	}
}

func (r *tfRun) destroy(t *testing.T) {
	t.Helper()
	if err := r.tf.Destroy(t.Context()); err != nil {
		t.Fatalf("terraform destroy: %v", err)
	}
}

// stateIDs returns group name → ID from the Terraform state.
func (r *tfRun) stateIDs(t *testing.T) map[string]string {
	t.Helper()
	st, err := r.tf.Show(t.Context())
	if err != nil {
		t.Fatalf("terraform show: %v", err)
	}
	ids := map[string]string{}
	if st.Values == nil || st.Values.RootModule == nil {
		return ids
	}
	for _, res := range st.Values.RootModule.Resources {
		id, _ := res.AttributeValues["id"].(string)
		ids[res.Name] = id
	}
	return ids
}

// assertService checks that the mock holds exactly the groups in the state,
// with matching names and IDs.
func assertService(t *testing.T, groups iamsvc.GroupService, tenantID string, want map[string]string) {
	t.Helper()
	resp, err := groups.List(t.Context(), &iampb.ListGroupsRequest{ParentId: tenantID})
	if err != nil {
		t.Fatalf("List groups in mock: %v", err)
	}
	got := map[string]string{}
	for _, g := range resp.GetItems() {
		if !seededGroups[g.GetMetadata().GetName()] {
			got[g.GetMetadata().GetName()] = g.GetMetadata().GetId()
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("mock groups = %v, want (terraform state) %v", got, want)
	}
}

func assertGone(t *testing.T, groups iamsvc.GroupService, id string) {
	t.Helper()
	_, err := groups.Get(t.Context(), &iampb.GetGroupRequest{Id: id})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Get(%s) after delete: code %v, want NotFound (err: %v)", id, status.Code(err), err)
	}
}

func actionName(a tfjson.Actions) string {
	switch {
	case a.NoOp():
		return "no-op"
	case a.Create():
		return "create"
	case a.Delete():
		return "delete"
	case a.Update():
		return "update"
	case a.Replace():
		return "replace"
	default:
		names := make([]string, 0, len(a))
		for _, x := range a {
			names = append(names, string(x))
		}
		slices.Sort(names)
		return strings.Join(names, "+")
	}
}

// childEnv returns the inherited environment with overrides applied.
// tfexec.SetEnv replaces the child's environment, so PATH, HOME (needed by
// version managers such as tfenv) and proxy settings must be passed through.
func childEnv(overrides map[string]string) map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() { //nolint:forbidigo // the terraform child process must inherit the developer's environment
		if k, v, ok := strings.Cut(kv, "="); ok && !strings.HasPrefix(k, "TF_") {
			env[k] = v
		}
	}
	maps.Copy(env, overrides)
	return env
}

// The plugin cache is private to this test binary (see tfcache): a cache
// shared with concurrently running suites gets its provider binary rewritten
// under a running terraform.
func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }

// writeCLIConfig isolates the run from the developer's ~/.terraformrc, so a
// local dev_overrides or mirror config cannot swap in a non-official provider.
func writeCLIConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "terraform.rc")
	if err := os.WriteFile(path, []byte("# intentionally empty: official registry only\n"), 0o600); err != nil {
		t.Fatalf("write CLI config: %v", err)
	}
	return path
}
