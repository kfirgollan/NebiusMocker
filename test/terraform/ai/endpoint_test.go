//go:build e2e

// Package ai runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's AI
// module. Nothing is patched: the provider is only pointed at the mock through
// its own configuration (resolvers, address_options, token).
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package ai

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	aisvc "github.com/nebius/gosdk/services/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/ai"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

// providerVersion pins the official provider release under test.
const providerVersion = "0.6.91"

const planFile = "tfplan"

const endpointToken = "tf-token-1"

// mock is a running NebiusMocker plus the CA its AI data plane is signed by.
type mock struct {
	*testkit.Instance
	// CAFile is the PEM CA certificate users trust to reach the data plane.
	CAFile string
}

// startMocker starts the IAM module, the extra modules and the AI module with
// an HTTPS data plane on loopback and a private TLS directory.
func startMocker(t *testing.T, extra ...mocker.Module) *mock {
	t.Helper()
	aiModule := ai.New(ai.WithListen("127.0.0.1:0"), ai.WithTLSDir(t.TempDir()))
	modules := append([]mocker.Module{iam.New()}, extra...)
	modules = append(modules, aiModule)
	inst := testkit.StartT(t, modules, testkit.WithAuthz(authz.ModeEnforce))
	return &mock{Instance: inst, CAFile: aiModule.CAFile()}
}

func startMock(t *testing.T) *mock {
	t.Helper()
	return startMocker(t)
}

// httpsClient trusts only the mock's CA, like curl --cacert.
func httpsClient(t *testing.T, caFile string) *http.Client {
	t.Helper()
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    caPool(t, caFile),
		MinVersion: tls.VersionTLS12,
	}}}
}

func caPool(t *testing.T, caFile string) *x509.CertPool {
	t.Helper()
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read CA %s: %v", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("CA %s holds no certificate", caFile)
	}
	return pool
}

// endpointConfig renders one nginx endpoint with the given image and labels.
func endpointConfig(projectID, image, labels string) string {
	return fmt.Sprintf(`
resource "nebius_ai_v1_endpoint" "e" {
  parent_id  = %q
  name       = "web"
  image      = %q
  platform   = "cpu-d3"
  preset     = "4vcpu-16gb"
  disk       = { type = "NETWORK_SSD", size_bytes = 21474836480 }
  ports      = [{ container_port = 80, protocol = "HTTP" }]
  auth_token = %q
  labels     = { %s }
}

output "public_endpoint" {
  value = nebius_ai_v1_endpoint.e.status.public_endpoints[0]
}
`, projectID, image, endpointToken, labels)
}

func TestEndpointLifecycle(t *testing.T) {
	inst := startMock(t)
	endpoints := aisvc.NewEndpointService(inst.SDK(t))
	tf := newTerraform(t, inst.Instance)

	tf.writeConfig(t, endpointConfig(inst.ProjectID, "nginx:alpine", `env = "dev"`))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"e": "create"})
	tf.apply(t)
	id := tf.stateIDs(t)["e"]
	if id == "" {
		t.Fatal("no endpoint ID in state")
	}

	got, err := endpoints.Get(t.Context(), &aipb.GetEndpointRequest{Id: id})
	if err != nil {
		t.Fatalf("Get endpoint in mock: %v", err)
	}
	if got.GetMetadata().GetName() != "web" || got.GetMetadata().GetLabels()["env"] != "dev" {
		t.Errorf("mock metadata = %v", got.GetMetadata())
	}
	if got.GetSpec().GetImage() != "nginx:alpine" || got.GetSpec().GetAuthToken() != endpointToken {
		t.Errorf("mock spec = %v", got.GetSpec())
	}
	if got.GetStatus().GetState() != aipb.EndpointStatus_RUNNING {
		t.Errorf("mock state = %v, want RUNNING", got.GetStatus().GetState())
	}
	tf.expectNoChanges(t)

	// The endpoint URL in the state answers authorized calls.
	url := tf.outputString(t, "public_endpoint")
	assertCall(t, inst.CAFile, url, endpointToken, http.StatusOK)
	assertCall(t, inst.CAFile, url, "", http.StatusUnauthorized)

	// Changing the image and labels forces a replacement (there is no Update RPC).
	tf.writeConfig(t, endpointConfig(inst.ProjectID, "nginx:1.27-alpine", `env = "prod"`))
	tf.expectPlan(t, map[string]string{"e": "replace"})
	tf.apply(t)
	newID := tf.stateIDs(t)["e"]
	if newID == "" || newID == id {
		t.Fatalf("endpoint ID after replace = %q, want a new one (was %q)", newID, id)
	}
	if _, err := endpoints.Get(t.Context(), &aipb.GetEndpointRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get(old %s): code %v, want NotFound", id, status.Code(err))
	}
	repl, err := endpoints.Get(t.Context(), &aipb.GetEndpointRequest{Id: newID})
	if err != nil {
		t.Fatalf("Get(new %s): %v", newID, err)
	}
	if repl.GetSpec().GetImage() != "nginx:1.27-alpine" || repl.GetMetadata().GetLabels()["env"] != "prod" {
		t.Errorf("replaced endpoint = %v", repl)
	}
	tf.expectNoChanges(t)
	assertCall(t, inst.CAFile, tf.outputString(t, "public_endpoint"), endpointToken, http.StatusOK)

	tf.destroy(t)
	list, err := endpoints.List(t.Context(), &aipb.ListEndpointsRequest{ParentId: inst.ProjectID})
	if err != nil {
		t.Fatalf("List endpoints: %v", err)
	}
	if n := len(list.GetItems()); n != 0 {
		t.Fatalf("mock has %d endpoints after destroy, want 0", n)
	}
}

func TestEndpointDataSource(t *testing.T) {
	inst := startMock(t)
	tf := newTerraform(t, inst.Instance)

	tf.writeConfig(t, endpointConfig(inst.ProjectID, "nginx:alpine", `env = "dev"`)+`
data "nebius_ai_v1_endpoint" "by_id" {
  id = nebius_ai_v1_endpoint.e.id
}

data "nebius_ai_v1_endpoint" "by_name" {
  parent_id = nebius_ai_v1_endpoint.e.parent_id
  name      = nebius_ai_v1_endpoint.e.name
}

output "by_id" {
  value = {
    id       = data.nebius_ai_v1_endpoint.by_id.id
    image    = data.nebius_ai_v1_endpoint.by_id.image
    state    = data.nebius_ai_v1_endpoint.by_id.status.state
    endpoint = data.nebius_ai_v1_endpoint.by_id.status.public_endpoints[0]
  }
}

output "by_name" {
  value = {
    id       = data.nebius_ai_v1_endpoint.by_name.id
    image    = data.nebius_ai_v1_endpoint.by_name.image
    state    = data.nebius_ai_v1_endpoint.by_name.status.state
    endpoint = data.nebius_ai_v1_endpoint.by_name.status.public_endpoints[0]
  }
}
`)
	tf.init(t)
	tf.apply(t)
	id := tf.stateIDs(t)["e"]
	want := map[string]any{
		"id":       id,
		"image":    "nginx:alpine",
		"state":    "RUNNING",
		"endpoint": tf.outputString(t, "public_endpoint"),
	}
	for _, name := range []string{"by_id", "by_name"} {
		var got map[string]any
		tf.output(t, name, &got)
		if !maps.Equal(got, want) {
			t.Errorf("data source %s = %v, want %v", name, got, want)
		}
	}
	tf.expectNoChanges(t)
	tf.destroy(t)
}

// assertCall calls the endpoint URL, with a bearer token when token is set.
// The URL must be https, as the status of a real endpoint is.
func assertCall(t *testing.T, caFile, url, token string, wantStatus int) {
	t.Helper()
	if !strings.HasPrefix(url, "https://") {
		t.Fatalf("endpoint URL %q does not start with https://", url)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("request %q: %v", url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpsClient(t, caFile).Do(req)
	if err != nil {
		t.Fatalf("call %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("call %s (token %q) = %d %s, want %d", url, token, resp.StatusCode, body, wantStatus)
	}
	if wantStatus != http.StatusOK {
		return
	}
	var msg struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Message != "endpoint was called" {
		t.Fatalf("call %s body = %s (err %v), want message \"endpoint was called\"", url, body, err)
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
// returns from Get round-trips through the provider schema without drift. On
// failure the plan diff is in the terraform output logged by the cleanup.
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
	if err := r.tf.Apply(t.Context()); err != nil {
		t.Fatalf("terraform apply: %v", err)
	}
}

func (r *tfRun) destroy(t *testing.T) {
	t.Helper()
	if err := r.tf.Destroy(t.Context()); err != nil {
		t.Fatalf("terraform destroy: %v", err)
	}
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
		id, _ := res.AttributeValues["id"].(string)
		ids[res.Name] = id
	}
	return ids
}

// output decodes the named root output into v.
func (r *tfRun) output(t *testing.T, name string, v any) {
	t.Helper()
	outs, err := r.tf.Output(context.WithoutCancel(t.Context()))
	if err != nil {
		t.Fatalf("terraform output: %v", err)
	}
	o, ok := outs[name]
	if !ok {
		t.Fatalf("output %q missing (have %v)", name, slices.Sorted(maps.Keys(outs)))
	}
	if err := json.Unmarshal(o.Value, v); err != nil {
		t.Fatalf("decode output %q: %v", name, err)
	}
}

func (r *tfRun) outputString(t *testing.T, name string) string {
	t.Helper()
	var s string
	r.output(t, name, &s)
	return s
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
