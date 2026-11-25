//go:build e2e

// Package mysterybox runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's
// MysteryBox (secrets manager) module.
//
// Nothing is patched or forked: the provider is only pointed at the mock with
// its own configuration (resolvers, address_options, token).
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package mysterybox

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

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	mysteryboxsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

// providerVersion pins the official provider release under test.
const providerVersion = "0.6.91"

const planFile = "tfplan"

var mockerLogs = flag.Bool("mocker-logs", false, "print NebiusMocker's request log to the test output")

// env bundles one running mock, its SDK clients and a Terraform directory.
type env struct {
	inst     *testkit.Instance
	secrets  mysteryboxsvc.SecretService
	versions mysteryboxsvc.SecretVersionService
	payloads mysteryboxsvc.PayloadService
	tf       *tfRun
}

// newEnv starts the mock with authn and enforced authz, and a non-zero
// operation delay so the provider's OperationService polling path is exercised.
func newEnv(t *testing.T) *env {
	t.Helper()
	opts := []testkit.Option{
		testkit.WithOperationDelay(300 * time.Millisecond),
		testkit.WithAuthnRequired(),
		testkit.WithAuthz(authz.ModeEnforce),
	}
	if *mockerLogs {
		opts = append(opts, testkit.WithLogs())
	}
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()}, opts...)
	sdk := inst.SDK(t)
	return &env{
		inst:     inst,
		secrets:  mysteryboxsvc.NewSecretService(sdk),
		versions: mysteryboxsvc.NewSecretVersionService(sdk),
		payloads: mysteryboxsvc.NewPayloadService(sdk),
		tf:       newTerraform(t, inst),
	}
}

// getSecret reads a secret from the mock, including soft-deleted ones.
func (e *env) getSecret(t *testing.T, id string) *mysteryboxpb.Secret {
	t.Helper()
	s, err := e.secrets.Get(t.Context(), &mysteryboxpb.GetSecretRequest{Id: id, ShowScheduledForDeletion: true})
	if err != nil {
		t.Fatalf("Get secret %s: %v", id, err)
	}
	return s
}

// getVersion reads a secret version from the mock, including soft-deleted ones.
func (e *env) getVersion(t *testing.T, id string) *mysteryboxpb.SecretVersion {
	t.Helper()
	v, err := e.versions.Get(t.Context(), &mysteryboxpb.GetSecretVersionRequest{Id: id, ShowScheduledForDeletion: true})
	if err != nil {
		t.Fatalf("Get secret version %s: %v", id, err)
	}
	return v
}

// activeSecrets maps name → id of the non-deleted secrets in the seeded project.
func (e *env) activeSecrets(t *testing.T) map[string]string {
	t.Helper()
	resp, err := e.secrets.List(t.Context(), &mysteryboxpb.ListSecretsRequest{ParentId: e.inst.ProjectID})
	if err != nil {
		t.Fatalf("List secrets: %v", err)
	}
	got := map[string]string{}
	for _, s := range resp.GetItems() {
		got[s.GetMetadata().GetName()] = s.GetMetadata().GetId()
	}
	return got
}

// activeVersions returns the sorted IDs of the non-deleted versions of a secret.
func (e *env) activeVersions(t *testing.T, secretID string) []string {
	t.Helper()
	resp, err := e.versions.List(t.Context(), &mysteryboxpb.ListSecretVersionsRequest{ParentId: secretID})
	if err != nil {
		t.Fatalf("List versions of %s: %v", secretID, err)
	}
	var ids []string
	for _, v := range resp.GetItems() {
		ids = append(ids, v.GetMetadata().GetId())
	}
	slices.Sort(ids)
	return ids
}

// assertSecrets checks that the mock's active secrets are exactly want (name → id).
func (e *env) assertSecrets(t *testing.T, want map[string]string) {
	t.Helper()
	if got := e.activeSecrets(t); !maps.Equal(got, want) {
		t.Fatalf("mock secrets = %v, want %v", got, want)
	}
}

// assertSecretSoftDeleted checks the secret is hidden from a plain Get but
// still readable with show_scheduled_for_deletion, in SCHEDULED_FOR_DELETION.
func (e *env) assertSecretSoftDeleted(t *testing.T, id string) {
	t.Helper()
	_, err := e.secrets.Get(t.Context(), &mysteryboxpb.GetSecretRequest{Id: id})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("plain Get(%s) after delete: code %v, want NotFound (err: %v)", id, status.Code(err), err)
	}
	if st := e.getSecret(t, id).GetStatus(); st.GetState() != mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION || st.GetDeletedAt() == nil || st.GetPurgeAt() == nil {
		t.Fatalf("secret %s status = %v, want SCHEDULED_FOR_DELETION with deleted_at and purge_at", id, st)
	}
}

// assertVersionSoftDeleted is assertSecretSoftDeleted for a secret version.
func (e *env) assertVersionSoftDeleted(t *testing.T, id string) {
	t.Helper()
	_, err := e.versions.Get(t.Context(), &mysteryboxpb.GetSecretVersionRequest{Id: id})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("plain Get version(%s) after delete: code %v, want NotFound (err: %v)", id, status.Code(err), err)
	}
	if st := e.getVersion(t, id).GetStatus(); st.GetState() != mysteryboxpb.SecretVersionStatus_SCHEDULED_FOR_DELETION {
		t.Fatalf("version %s state = %v, want SCHEDULED_FOR_DELETION", id, st.GetState())
	}
}

// payloadStrings returns key → value of a secret version's payload as served
// by the data plane (binary values are returned as raw strings). An empty
// versionID selects the primary version.
func (e *env) payloadStrings(t *testing.T, secretID, versionID string) map[string]string {
	t.Helper()
	p, err := e.payloads.Get(t.Context(), &mysteryboxpb.GetPayloadRequest{SecretId: secretID, VersionId: versionID})
	if err != nil {
		t.Fatalf("Get payload of %s/%q: %v", secretID, versionID, err)
	}
	got := map[string]string{}
	for _, d := range p.GetData() {
		if _, ok := d.GetPayload().(*mysteryboxpb.Payload_BinaryValue); ok {
			got[d.GetKey()] = string(d.GetBinaryValue())
		} else {
			got[d.GetKey()] = d.GetStringValue()
		}
	}
	return got
}

// primaryVersion returns the version ID the data plane serves as primary.
func (e *env) primaryVersion(t *testing.T, secretID string) string {
	t.Helper()
	p, err := e.payloads.Get(t.Context(), &mysteryboxpb.GetPayloadRequest{SecretId: secretID})
	if err != nil {
		t.Fatalf("Get primary payload of %s: %v", secretID, err)
	}
	return p.GetVersionId()
}

// TestMain removes the private plugin cache after the run.
func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }

// tfRun wraps one Terraform working directory.
type tfRun struct {
	tf    *tfexec.Terraform
	dir   string
	addr  string
	token string
	out   bytes.Buffer
}

func newTerraform(t *testing.T, inst *testkit.Instance) *tfRun {
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
	r := &tfRun{tf: tf, dir: dir, addr: inst.Addr, token: inst.Token}
	env := map[string]string{
		"CHECKPOINT_DISABLE":         "1",
		"TF_CLI_CONFIG_FILE":         writeCLIConfig(t, dir),
		"TF_REGISTRY_CLIENT_TIMEOUT": "60",
		"NEBIUS_IAM_TOKEN":           "", // credentials come from the provider block only
	}
	maps.Copy(env, tfcache.Env(t)) // a private plugin cache: a shared one breaks under concurrent runs
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

// requireVersion skips the test unless terraform is at least major.minor.
func (r *tfRun) requireVersion(t *testing.T, major, minor int64) {
	t.Helper()
	v, _, err := r.tf.Version(t.Context(), false)
	if err != nil {
		t.Fatalf("terraform version: %v", err)
	}
	seg := v.Segments64()
	if seg[0] < major || (seg[0] == major && seg[1] < minor) {
		t.Skipf("terraform %s < %d.%d", v, major, minor)
	}
}

// planActions plans to a file and returns the action for each managed resource (by name).
func (r *tfRun) planActions(t *testing.T) map[string]string {
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
		if rc.Mode == tfjson.ManagedResourceMode {
			got[rc.Name] = actionName(rc.Change.Actions)
		}
	}
	return got
}

// expectPlan asserts the planned action for each managed resource (by name).
func (r *tfRun) expectPlan(t *testing.T, want map[string]string) {
	t.Helper()
	if got := r.planActions(t); !maps.Equal(got, want) {
		t.Fatalf("plan actions = %v, want %v", got, want)
	}
}

// expectNoChanges asserts that a fresh plan after apply is empty: what the mock
// returns from Get round-trips through the provider schema without drift.
func (r *tfRun) expectNoChanges(t *testing.T) {
	t.Helper()
	changed, err := r.tf.Plan(t.Context(), tfexec.Out(planFile))
	if err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	if changed {
		if raw, err := r.tf.ShowPlanFileRaw(t.Context(), planFile); err == nil {
			t.Logf("drift plan:\n%s", raw)
		}
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

// expectApplyError runs a plan+apply that must fail with want in the output.
func (r *tfRun) expectApplyError(t *testing.T, want string) {
	t.Helper()
	before := r.out.Len()
	err := r.tf.Apply(t.Context())
	if err == nil {
		t.Fatalf("terraform apply succeeded, want failure containing %q", want)
	}
	out := r.out.String()[before:] + err.Error()
	if !strings.Contains(out, want) {
		t.Fatalf("terraform apply failed without %q:\n%s", want, out)
	}
}

// state returns the managed and data resources of the Terraform state by
// address (for example "nebius_mysterybox_v1_secret.s" or "data.x.y").
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
		out[res.Address] = res.AttributeValues
	}
	return out
}

// stateIDs returns managed resource name → ID from the Terraform state.
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
		if res.Mode != tfjson.ManagedResourceMode {
			continue
		}
		if id, ok := res.AttributeValues["id"].(string); ok {
			ids[res.Name] = id
		}
	}
	return ids
}

// attr digs a dotted path (object keys only) out of a state attribute map.
func attr(t *testing.T, m map[string]any, path string) any {
	t.Helper()
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("attribute path %q: %v is not an object", path, cur)
		}
		cur = mm[p]
	}
	return cur
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

// pluginCacheDir keeps downloaded providers between runs.
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
