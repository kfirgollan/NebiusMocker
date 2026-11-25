//go:build e2e || e2e_storage

// Package storage runs the official Terraform binary with official, unmodified
// providers from the Terraform Registry against NebiusMocker's Object Storage:
// nebius/nebius for buckets and credentials, and hashicorp/aws for objects
// (pointed at the mocker's S3 front, the way Nebius documents S3 tools).
//
// Run with: make e2e (buckets) and make e2e-storage (objects, needs Docker).
package storage

import (
	"bytes"
	"encoding/json"
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

// Pinned official provider releases under test.
const (
	nebiusProviderVersion = "0.6.91"
	awsProviderVersion    = "6.70.0"
)

const planFile = "tfplan"

var mockerLogs = flag.Bool("mocker-logs", false, "print NebiusMocker's request log to the test output")

func mockerOptions(opts ...testkit.Option) []testkit.Option {
	if *mockerLogs {
		opts = append(opts, testkit.WithLogs())
	}
	return opts
}

func nebiusProvider(addr, token string) string {
	return fmt.Sprintf(`terraform {
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
`, nebiusProviderVersion, addr, token)
}

// tfRun wraps one Terraform working directory.
type tfRun struct {
	tf  *tfexec.Terraform
	dir string
	out bytes.Buffer
}

func newTerraform(t *testing.T) *tfRun {
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
	r := &tfRun{tf: tf, dir: dir}
	if err := tf.SetEnv(childEnv(map[string]string{
		"CHECKPOINT_DISABLE":          "1",
		"TF_CLI_CONFIG_FILE":          writeCLIConfig(t, dir),
		"TF_REGISTRY_CLIENT_TIMEOUT":  "60",
		"NEBIUS_IAM_TOKEN":            "", // credentials come from the provider blocks only
		"AWS_ACCESS_KEY_ID":           "",
		"AWS_SECRET_ACCESS_KEY":       "",
		"AWS_PROFILE":                 "",
		"AWS_CONFIG_FILE":             filepath.Join(dir, "no-aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "no-aws-credentials"),
	}, tfcache.Env(t))); err != nil {
		t.Fatalf("tfexec env: %v", err)
	}
	tf.SetStdout(&r.out)
	tf.SetStderr(&r.out)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("terraform output (%s):\n%s", dir, r.out.String())
		}
	})
	return r
}

// write renders main.tf.
func (r *tfRun) write(t *testing.T, cfg string) {
	t.Helper()
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

// expectPlan plans to a file and asserts the action of each resource address.
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
		if rc.Mode == tfjson.DataResourceMode {
			continue
		}
		got[rc.Address] = actionName(rc.Change.Actions)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("plan actions = %v, want %v", got, want)
	}
}

// expectNoChanges asserts that a fresh plan is empty: what the mock returns
// round-trips through the provider schemas without drift.
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

// state returns address → attribute values of every managed resource.
func (r *tfRun) state(t *testing.T) map[string]map[string]any {
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
		if res.Mode == tfjson.ManagedResourceMode {
			out[res.Address] = res.AttributeValues
		}
	}
	return out
}

// output returns a root output value.
func (r *tfRun) output(t *testing.T, name string) any {
	t.Helper()
	outs, err := r.tf.Output(t.Context())
	if err != nil {
		t.Fatalf("terraform output: %v", err)
	}
	o, ok := outs[name]
	if !ok {
		t.Fatalf("output %q missing (have %v)", name, slices.Collect(maps.Keys(outs)))
	}
	var v any
	if err := json.Unmarshal(o.Value, &v); err != nil {
		t.Fatalf("decode output %q: %v", name, err)
	}
	return v
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
// tfexec.SetEnv replaces the child's environment, so PATH, HOME and proxy
// settings must be passed through.
func childEnv(overrides ...map[string]string) map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() { //nolint:forbidigo // the terraform child process must inherit the developer's environment
		if k, v, ok := strings.Cut(kv, "="); ok && !strings.HasPrefix(k, "TF_") && !strings.HasPrefix(k, "AWS_") {
			env[k] = v
		}
	}
	for _, o := range overrides {
		maps.Copy(env, o)
	}
	return env
}

// writeCLIConfig isolates the run from ~/.terraformrc, so no dev_overrides or
// mirror can swap in a non-official provider.
func writeCLIConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "terraform.rc")
	if err := os.WriteFile(path, []byte("# intentionally empty: official registry only\n"), 0o600); err != nil {
		t.Fatalf("write CLI config: %v", err)
	}
	return path
}
