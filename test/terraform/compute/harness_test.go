//go:build e2e || e2e_images

// Package compute runs the official Terraform binary with the official
// nebius/nebius provider against NebiusMocker's Compute images.
//
// In provider 0.6.91 nebius_compute_v1_image is a data source only (no
// resource), read with ImageService Get (by id) and GetByName (by
// parent_id + name). The tests read images through it and compare every value
// with the mock's state, read with the official Go SDK.
//
//   - public_test.go (make e2e): seeded public images, no Docker.
//   - custom_images_test.go (make e2e-images): custom images imported from a
//     bucket the image was uploaded to through the S3 front, on RustFS.
package compute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

// providerVersion pins the official provider release under test.
const providerVersion = "0.6.91"

const (
	outsiderToken = "outsider-token"
	outsiderID    = "serviceaccount-e00mock00000000outsider"
)

// assertImage compares a data source's attributes with the image in the mock.
func assertImage(t *testing.T, got map[string]any, want *computepb.Image) {
	t.Helper()
	md, spec, st := want.GetMetadata(), want.GetSpec(), want.GetStatus()
	checks := map[string][2]string{
		"id":                          {str(got, "id"), md.GetId()},
		"name":                        {str(got, "name"), md.GetName()},
		"parent_id":                   {str(got, "parent_id"), md.GetParentId()},
		"image_family":                {str(got, "image_family"), spec.GetImageFamily()},
		"version":                     {str(got, "version"), spec.GetVersion()},
		"description":                 {str(got, "description"), spec.GetDescription()},
		"cpu_architecture":            {str(got, "cpu_architecture"), spec.GetCpuArchitecture().String()},
		"image_family_human_readable": {str(got, "image_family_human_readable"), spec.GetImageFamilyHumanReadable()},
		"status.state":                {str(got, "status", "state"), st.GetState().String()},
		"status.storage_size_bytes":   {str(got, "status", "storage_size_bytes"), strconv.FormatInt(st.GetStorageSizeBytes(), 10)},
		"status.min_disk_size_bytes":  {str(got, "status", "min_disk_size_bytes"), strconv.FormatInt(st.GetMinDiskSizeBytes(), 10)},
		"resource_version":            {str(got, "resource_version"), strconv.FormatInt(md.GetResourceVersion(), 10)},
		"source_storage.object_name":  {str(got, "source_storage", "object_name"), spec.GetSourceStorage().GetObjectName()},
	}
	for field, c := range checks {
		if c[0] != c[1] {
			t.Errorf("image %s: terraform %s = %q, mock has %q", md.GetId(), field, c[0], c[1])
		}
	}
	labels := map[string]string{}
	if l, ok := got["labels"].(map[string]any); ok {
		for k, v := range l {
			labels[k], _ = v.(string)
		}
	}
	if !maps.Equal(labels, md.GetLabels()) && (len(labels) > 0 || len(md.GetLabels()) > 0) {
		t.Errorf("image %s: terraform labels = %v, mock has %v", md.GetId(), labels, md.GetLabels())
	}
}

// str walks nested output maps and renders the leaf ("" when absent).
func str(v map[string]any, path ...string) string {
	var cur any = v
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	switch x := cur.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	default:
		return fmt.Sprint(x)
	}
}

// tfRun wraps one Terraform working directory.
type tfRun struct {
	tf    *tfexec.Terraform
	dir   string
	addr  string
	token string
	out   bytes.Buffer
}

func newTerraform(t *testing.T, addr, token string) *tfRun {
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
	r := &tfRun{tf: tf, dir: dir, addr: addr, token: token}
	rc := filepath.Join(dir, "terraform.rc")
	if err := os.WriteFile(rc, []byte("# intentionally empty: official registry only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tf.SetEnv(childEnv(map[string]string{
		"CHECKPOINT_DISABLE":                             "1",
		"TF_PLUGIN_CACHE_DIR":                            tfcache.Dir(t),
		"TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE": "1", // reuse the first download; the cache is private
		"TF_CLI_CONFIG_FILE":                             rc,
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

// writeConfig renders main.tf: the provider block followed by body.
func (r *tfRun) writeConfig(t *testing.T, body string) {
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
`, providerVersion, r.addr, r.token) + body
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

func (r *tfRun) apply(t *testing.T) {
	t.Helper()
	if err := r.tf.Apply(t.Context()); err != nil {
		t.Fatalf("terraform apply: %v", err)
	}
}

func (r *tfRun) applyRefresh(t *testing.T) {
	t.Helper()
	if err := r.tf.Apply(t.Context(), tfexec.Refresh(true)); err != nil {
		t.Fatalf("terraform apply: %v", err)
	}
}

func (r *tfRun) planChanges(t *testing.T) bool {
	t.Helper()
	changed, err := r.tf.Plan(t.Context())
	if err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	return changed
}

// expectNoChanges asserts that a fresh plan is empty: the data sources read
// back exactly what is in the state, so the mock's Get/GetByName are stable.
func (r *tfRun) expectNoChanges(t *testing.T) {
	t.Helper()
	if r.planChanges(t) {
		t.Fatal("plan is not empty: the mock's image responses drift between reads")
	}
}

func (r *tfRun) destroy(t *testing.T) {
	t.Helper()
	if err := r.tf.Destroy(t.Context()); err != nil {
		t.Fatalf("terraform destroy: %v", err)
	}
}

func (r *tfRun) outputs(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := r.tf.Output(t.Context())
	if err != nil {
		t.Fatalf("terraform output: %v", err)
	}
	out := map[string]map[string]any{}
	for name, meta := range raw {
		var v map[string]any
		if err := json.Unmarshal(meta.Value, &v); err != nil {
			t.Fatalf("output %s: %v", name, err)
		}
		out[name] = v
	}
	return out
}

// childEnv returns the inherited environment with overrides applied.
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
