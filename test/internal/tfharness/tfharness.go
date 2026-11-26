// Package tfharness runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against an in-process
// NebiusMocker. Nothing is patched: the provider is pointed at the mock only
// through its own settings (resolvers, address_options, token).
//
// It is the reusable form of the helpers in test/terraform/iam.
package tfharness

import (
	"bytes"
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

	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

// ProviderVersion pins the official provider release under test.
const ProviderVersion = "0.6.91"

const planFile = "tfplan"

// Run wraps one Terraform working directory.
type Run struct {
	tf    *tfexec.Terraform
	dir   string
	addr  string
	token string
	// addressOptions is the provider's address_options value for "*".
	addressOptions string
	out            bytes.Buffer
}

// New prepares a working directory whose provider talks to addr with token.
// The test is skipped when terraform is not on PATH.
func New(t *testing.T, addr, token string) *Run {
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
	r := &Run{tf: tf, dir: dir, addr: addr, token: token, addressOptions: "{ insecure = true }"}
	env := map[string]string{
		"CHECKPOINT_DISABLE":         "1",
		"TF_CLI_CONFIG_FILE":         writeCLIConfig(t, dir),
		"TF_REGISTRY_CLIENT_TIMEOUT": "60",
		"NEBIUS_IAM_TOKEN":           "", // credentials come from the provider block only
	}
	maps.Copy(env, tfcache.Env(t)) // private plugin cache; callers add TestMain with tfcache.Main
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

// UseTLSWithoutVerify points the provider at a mocker that serves TLS
// (NEBIUS_MOCKER_TLS_CERT_FILE) through its documented no_tls_verify
// address option, instead of plaintext.
func (r *Run) UseTLSWithoutVerify() { r.addressOptions = "{ no_tls_verify = true }" }

// Write renders main.tf: the provider block followed by body.
func (r *Run) Write(t *testing.T, body string) {
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
  address_options = { "*" = %s }
  token           = %q
}
`, ProviderVersion, r.addr, r.addressOptions, r.token) + body
	if err := os.WriteFile(filepath.Join(r.dir, "main.tf"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write main.tf: %v", err)
	}
}

// Init runs terraform init.
func (r *Run) Init(t *testing.T) {
	t.Helper()
	if err := r.tf.Init(t.Context()); err != nil {
		t.Fatalf("terraform init: %v", err)
	}
}

// ExpectPlan plans to a file and asserts the action per managed resource
// address ("nebius_x.name" → create|update|delete|replace|no-op).
func (r *Run) ExpectPlan(t *testing.T, want map[string]string) {
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
			continue
		}
		got[rc.Address] = actionName(rc.Change.Actions)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("plan actions = %v, want %v", got, want)
	}
}

// ExpectNoChanges asserts that a fresh plan is empty (no drift).
func (r *Run) ExpectNoChanges(t *testing.T) {
	t.Helper()
	changed, err := r.tf.Plan(t.Context())
	if err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	if changed {
		t.Fatalf("plan after apply is not empty: the mock's responses drift from the applied config")
	}
}

// Apply applies the last plan file.
func (r *Run) Apply(t *testing.T) {
	t.Helper()
	if err := r.tf.Apply(t.Context(), tfexec.DirOrPlan(planFile)); err != nil {
		t.Fatalf("terraform apply: %v", err)
	}
}

// Destroy runs terraform destroy.
func (r *Run) Destroy(t *testing.T) {
	t.Helper()
	if err := r.tf.Destroy(t.Context()); err != nil {
		t.Fatalf("terraform destroy: %v", err)
	}
}

// State returns address → attribute values of every resource and data source.
func (r *Run) State(t *testing.T) map[string]map[string]any {
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
	case a.Read():
		return "read"
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

// writeCLIConfig isolates the run from ~/.terraformrc (no dev_overrides or mirrors).
func writeCLIConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "terraform.rc")
	if err := os.WriteFile(path, []byte("# intentionally empty: official registry only\n"), 0o600); err != nil {
		t.Fatalf("write CLI config: %v", err)
	}
	return path
}
