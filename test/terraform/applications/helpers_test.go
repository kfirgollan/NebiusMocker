//go:build e2e

package applications

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

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

// providerVersion pins the official provider release under test.
const providerVersion = "0.6.91"

const planFile = "tfplan"

var mockerLogs = flag.Bool("mocker-logs", false, "print NebiusMocker's request log to the test output")

func mockerOptions(opts ...testkit.Option) []testkit.Option {
	if *mockerLogs {
		opts = append(opts, testkit.WithLogs())
	}
	return opts
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
	env := map[string]string{
		"CHECKPOINT_DISABLE":         "1",
		"TF_CLI_CONFIG_FILE":         writeCLIConfig(t, dir),
		"TF_REGISTRY_CLIENT_TIMEOUT": "60",
		"NEBIUS_IAM_TOKEN":           "", // credentials come from the provider block only
	}
	// The plugin cache is private to this test binary (see tfcache), so
	// concurrent runs never exec each other's half-written provider.
	maps.Copy(env, tfcache.Env(t))
	if err := tf.SetEnv(childEnv(env)); err != nil {
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

// expectPlan plans to a file and asserts the action for each managed resource (by name).
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
		if rc.Mode != tfjson.ManagedResourceMode {
			continue // data source reads
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
	var changed bool
	changed, err := r.tf.Plan(t.Context())
	if err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	if changed {
		t.Fatalf("plan after apply is not empty: the mock's responses drift from the applied config")
	}
}

// apply plans the current config to a fresh plan file, then applies it.
func (r *tfRun) apply(t *testing.T) {
	t.Helper()
	if _, err := r.tf.Plan(t.Context(), tfexec.Out(planFile)); err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
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

// TestMain removes this binary's private plugin cache (tfcache) afterwards.
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

// stateAttrs returns the attributes of every resource and data source in the
// state, keyed by address (e.g. "data.nebius_kms_v1_symmetric_key.by_id").
func (r *tfRun) stateAttrs(t *testing.T) map[string]map[string]any {
	t.Helper()
	st, err := r.tf.Show(t.Context())
	if err != nil {
		t.Fatalf("terraform show: %v", err)
	}
	out := map[string]map[string]any{}
	if st.Values == nil || st.Values.RootModule == nil {
		return out
	}
	for _, res := range st.Values.RootModule.Resources {
		out[res.Address] = res.AttributeValues
	}
	return out
}

// stateIDs returns managed resource name → ID from the Terraform state.
func (r *tfRun) stateIDs(t *testing.T) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for addr, attrs := range r.stateAttrs(t) {
		if strings.HasPrefix(addr, "data.") {
			continue
		}
		_, name, _ := strings.Cut(addr, ".")
		ids[name], _ = attrs["id"].(string)
	}
	return ids
}
