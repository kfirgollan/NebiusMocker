//go:build e2e

package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"
	storagev1alpha1svc "github.com/nebius/gosdk/services/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
)

const (
	trfSrcSecret = "trf-src-secret-value"
	trfDstSecret = "trf-dst-secret-value"
)

// trfBuckets declares the Nebius source and destination buckets.
func trfBuckets(project string) string {
	return fmt.Sprintf(`
resource "nebius_storage_v1_bucket" "src" {
  parent_id = %[1]q
  name      = "tf-trf-source"
}

resource "nebius_storage_v1_bucket" "dst" {
  parent_id = %[1]q
  name      = "tf-trf-destination"
}
`, project)
}

// trfV1 describes one v1 transfer configuration.
type trfV1 struct {
	name      string
	labels    string
	source    string // the `source = {...}` value; empty means the Nebius source bucket
	dest      string // the `destination = {...}` value; empty means the Nebius destination bucket
	limiters  string
	stop      string // e.g. `after_one_iteration = {}`
	interval  string // "" leaves the attribute out
	overwrite string
	sensitive string // the `sensitive = {...}` block; when set, the secrets stay out of source/destination
}

// trfV1AccessKey renders an access_key value; an empty secret leaves it out (write-only).
func trfV1AccessKey(id, secret string) string {
	if secret == "" {
		return fmt.Sprintf("{ access_key_id = %q }", id)
	}
	return fmt.Sprintf("{ access_key_id = %q, secret_access_key = %q }", id, secret)
}

func (c trfV1) render(project string) string {
	srcSecret, dstSecret := trfSrcSecret, trfDstSecret
	if c.sensitive != "" {
		srcSecret, dstSecret = "", ""
	}
	source := c.source
	if source == "" {
		source = fmt.Sprintf(`{
    nebius = {
      region      = "eu-north1"
      bucket_name = nebius_storage_v1_bucket.src.name
      access_key  = %s
    }
    prefix = "in/"
  }`, trfV1AccessKey("AKSRC", srcSecret))
	}
	dest := c.dest
	if dest == "" {
		dest = fmt.Sprintf(`{
    nebius = {
      region      = "eu-north1"
      bucket_name = nebius_storage_v1_bucket.dst.name
      access_key  = %s
    }
    prefix = "out/"
  }`, trfV1AccessKey("AKDST", dstSecret))
	}
	interval := ""
	if c.interval != "" {
		interval = fmt.Sprintf("  inter_iteration_interval = %q\n", c.interval)
	}
	return fmt.Sprintf(`
resource "nebius_storage_v1_transfer" "t" {
  parent_id                     = %[1]q
  name                          = %[2]q
  labels                        = { %[3]s }
  source                        = %[4]s
  destination                   = %[5]s
  limiters                      = %[6]s
  %[7]s
%[8]s  overwrite_strategy            = %[9]q
  enable_deletes_in_destination = false
  touch_unmanaged               = false
%[10]s}

data "nebius_storage_v1_transfer" "by_id" {
  id = nebius_storage_v1_transfer.t.id
}

data "nebius_storage_v1_transfer" "by_name" {
  parent_id = nebius_storage_v1_transfer.t.parent_id
  name      = nebius_storage_v1_transfer.t.name
}

output "by_id" { value = data.nebius_storage_v1_transfer.by_id }
output "by_name" { value = data.nebius_storage_v1_transfer.by_name }
`, project, c.name, c.labels, source, dest, c.limiters, c.stop, interval, c.overwrite, c.sensitive)
}

// trfEnv is one running mock with Go SDK clients for both transfer API versions.
type trfEnv struct {
	inst *testkit.Instance
	v1   storagesvc.TransferService
	alph storagev1alpha1svc.TransferService
	tf   *tfRun
}

func trfStart(t *testing.T) *trfEnv {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("off"))},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	return &trfEnv{
		inst: inst,
		v1:   storagesvc.NewTransferService(inst.SDK(t)),
		alph: storagev1alpha1svc.NewTransferService(inst.SDK(t)),
		tf:   newTerraform(t),
	}
}

func (e *trfEnv) getV1(t *testing.T, id string) *storagepb.Transfer {
	t.Helper()
	got, err := e.v1.Get(t.Context(), &storagepb.GetTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("v1 Get %s: %v", id, err)
	}
	return got
}

func (e *trfEnv) getAlpha(t *testing.T, id string) *storagev1alpha1pb.Transfer {
	t.Helper()
	got, err := e.alph.Get(t.Context(), &storagev1alpha1pb.GetTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("v1alpha1 Get %s: %v", id, err)
	}
	return got
}

func (e *trfEnv) expectGoneV1(t *testing.T, id string) {
	t.Helper()
	if _, err := e.v1.Get(t.Context(), &storagepb.GetTransferRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("v1 Get %s after removal = %v, want NotFound", id, err)
	}
}

func (e *trfEnv) expectGoneAlpha(t *testing.T, id string) {
	t.Helper()
	if _, err := e.alph.Get(t.Context(), &storagev1alpha1pb.GetTransferRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("v1alpha1 Get %s after removal = %v, want NotFound", id, err)
	}
}

// trfNoSecrets fails when a secret value reached a response: they are INPUT_ONLY.
func trfNoSecrets(t *testing.T, step string, msg fmt.Stringer) {
	t.Helper()
	for _, s := range []string{trfSrcSecret, trfDstSecret} {
		if strings.Contains(msg.String(), s) {
			t.Errorf("%s: the API returned the secret %q: %v", step, s, msg)
		}
	}
}

// trfRequireTerraform skips the test on a Terraform older than major.minor.
func trfRequireTerraform(t *testing.T, r *tfRun, major, minor int64) {
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

// trfStateHas reports whether the Terraform state file contains the value.
func trfStateHas(t *testing.T, dir, value string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return strings.Contains(string(raw), value)
}

// trfV1Spec checks the parts of a v1 spec that every scenario sets the same way.
func trfV1Spec(t *testing.T, step string, got *storagepb.Transfer) {
	t.Helper()
	spec := got.GetSpec()
	if n := spec.GetSource().GetNebius(); n.GetRegion() != "eu-north1" || n.GetBucketName() != "tf-trf-source" ||
		n.GetAccessKey().GetAccessKeyId() != "AKSRC" {
		t.Errorf("%s: source = %v, want nebius tf-trf-source with access key AKSRC", step, spec.GetSource())
	}
	if n := spec.GetDestination().GetNebius(); n.GetRegion() != "eu-north1" || n.GetBucketName() != "tf-trf-destination" ||
		n.GetAccessKey().GetAccessKeyId() != "AKDST" {
		t.Errorf("%s: destination = %v, want nebius tf-trf-destination with access key AKDST", step, spec.GetDestination())
	}
	if spec.GetSource().GetPrefix() != "in/" || spec.GetDestination().GetPrefix() != "out/" {
		t.Errorf("%s: prefixes = %q, %q", step, spec.GetSource().GetPrefix(), spec.GetDestination().GetPrefix())
	}
	if got.GetStatus().GetState() == storagepb.TransferStatus_STATE_UNSPECIFIED {
		t.Errorf("%s: status is not populated: %v", step, got.GetStatus())
	}
	trfNoSecrets(t, step, got)
}

// trfDataSources checks both data source reads against the resource.
func (e *trfEnv) trfDataSources(t *testing.T, step, res string, fields ...string) {
	t.Helper()
	st := e.tf.state(t)[res]
	for _, out := range []string{"by_id", "by_name"} {
		ds, ok := e.tf.output(t, out).(map[string]any)
		if !ok {
			t.Fatalf("%s: output %s is not an object", step, out)
		}
		for _, f := range fields {
			if ds[f] != st[f] {
				t.Errorf("%s: data source %s.%s = %#v, resource has %#v", step, out, f, ds[f], st[f])
			}
		}
	}
}

// trfWaitSettled waits until a transfer is STOPPED or FAILED, the states in
// which v1alpha1 lets it be deleted.
func (e *trfEnv) trfWaitSettled(t *testing.T, id string) {
	t.Helper()
	for range 300 {
		s := e.getAlpha(t, id).GetStatus().GetState()
		if s == storagev1alpha1pb.TransferStatus_STOPPED || s == storagev1alpha1pb.TransferStatus_FAILED {
			return
		}
		select {
		case <-clock.Real{}.After(100 * time.Millisecond):
		case <-t.Context().Done():
			t.Fatalf("test ended while waiting for transfer %s", id)
		}
	}
	t.Fatalf("transfer %s did not reach STOPPED or FAILED: %v", id, e.getAlpha(t, id).GetStatus())
}

// TestTransferV1Lifecycle drives nebius_storage_v1_transfer and its data
// source with the official provider: create, empty plan (secrets are INPUT_ONLY),
// in-place update, empty plan, replace on an immutable field, data sources,
// destroy. The Go SDK checks the mock after every step.
func TestTransferV1Lifecycle(t *testing.T) {
	e := trfStart(t)
	const res = "nebius_storage_v1_transfer.t"
	project := e.inst.ProjectID
	cfg := func(c trfV1) string {
		return nebiusProvider(e.inst.Addr, e.inst.Token) + trfBuckets(project) + c.render(project)
	}
	noop := func(action string) map[string]string {
		return map[string]string{"nebius_storage_v1_bucket.src": "no-op", "nebius_storage_v1_bucket.dst": "no-op", res: action}
	}
	dsFields := []string{"id", "name", "parent_id", "overwrite_strategy", "inter_iteration_interval"}

	base := trfV1{
		name: "tf-transfer", labels: `team = "a"`,
		limiters: `{ bandwidth_bytes_per_second = 1048576, requests_per_second = 100 }`,
		stop:     `after_one_iteration = {}`, overwrite: "IF_NEWER",
	}

	// 1. Create.
	e.tf.write(t, cfg(base))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{
		"nebius_storage_v1_bucket.src": "create", "nebius_storage_v1_bucket.dst": "create", res: "create",
	})
	e.tf.apply(t)
	id, _ := e.tf.state(t)[res]["id"].(string)
	if id == "" {
		t.Fatalf("transfer has no id in state: %v", e.tf.state(t)[res])
	}
	got := e.getV1(t, id)
	if m := got.GetMetadata(); m.GetName() != "tf-transfer" || m.GetParentId() != project || m.GetLabels()["team"] != "a" {
		t.Fatalf("create: metadata = %v", m)
	}
	trfV1Spec(t, "create", got)
	spec := got.GetSpec()
	if spec.GetAfterOneIteration() == nil || spec.GetOverwriteStrategy() != storagepb.TransferSpec_IF_NEWER ||
		spec.GetLimiters().GetBandwidthBytesPerSecond() != 1048576 || spec.GetLimiters().GetRequestsPerSecond() != 100 {
		t.Fatalf("create: spec = %v", spec)
	}
	if d := spec.GetInterIterationInterval().AsDuration(); d != 15*time.Minute {
		t.Errorf("create: inter_iteration_interval = %v, want the 15m default", d)
	}

	// 2. Empty plan despite the secrets not being returned; data sources agree.
	e.tf.expectNoChanges(t)
	e.trfDataSources(t, "create", res, dsFields...)

	// 3. In-place update: limiters, interval, name, labels, stop condition.
	upd := base
	upd.name = "tf-transfer-renamed"
	upd.labels = `team = "b"`
	upd.limiters = `{ bandwidth_bytes_per_second = 2097152, requests_per_second = 50 }`
	upd.interval = "30m"
	upd.stop = `after_n_empty_iterations = { empty_iterations_threshold = 2 }`
	e.tf.write(t, cfg(upd))
	e.tf.expectPlan(t, noop("update"))
	e.tf.apply(t)
	if now, _ := e.tf.state(t)[res]["id"].(string); now != id {
		t.Fatalf("update replaced the transfer: %s → %s", id, now)
	}
	got = e.getV1(t, id)
	trfV1Spec(t, "update", got)
	spec = got.GetSpec()
	if got.GetMetadata().GetName() != "tf-transfer-renamed" || got.GetMetadata().GetLabels()["team"] != "b" ||
		spec.GetLimiters().GetBandwidthBytesPerSecond() != 2097152 || spec.GetLimiters().GetRequestsPerSecond() != 50 ||
		spec.GetInterIterationInterval().AsDuration() != 30*time.Minute ||
		spec.GetAfterNEmptyIterations().GetEmptyIterationsThreshold() != 2 || spec.GetAfterOneIteration() != nil {
		t.Fatalf("update: transfer = %v", got)
	}
	if v := got.GetMetadata().GetResourceVersion(); v < 2 {
		t.Errorf("resource_version = %d, want at least 2 after an update", v)
	}

	// 4. Empty plan; data sources agree.
	e.tf.expectNoChanges(t)
	e.trfDataSources(t, "update", res, dsFields...)

	// 5. Replace: overwrite_strategy is RequiresReplace.
	rep := upd
	rep.overwrite = "NEVER"
	e.tf.write(t, cfg(rep))
	e.tf.expectPlan(t, noop("replace"))
	e.tf.apply(t)
	id2, _ := e.tf.state(t)[res]["id"].(string)
	if id2 == "" || id2 == id {
		t.Fatalf("replace: id %q, want a new id (old %s)", id2, id)
	}
	e.expectGoneV1(t, id)
	if got := e.getV1(t, id2); got.GetSpec().GetOverwriteStrategy() != storagepb.TransferSpec_NEVER {
		t.Fatalf("replace: spec = %v", got.GetSpec())
	}
	e.tf.expectNoChanges(t)
	e.trfDataSources(t, "replace", res, dsFields...)

	// 6. Destroy.
	e.tf.destroy(t)
	if len(e.tf.state(t)) != 0 {
		t.Fatalf("state after destroy = %v, want empty", e.tf.state(t))
	}
	e.expectGoneV1(t, id2)
}

// TestTransferV1WriteOnlySecrets uses the provider's write-only `sensitive`
// block: the secrets reach the mock but never the Terraform state.
func TestTransferV1WriteOnlySecrets(t *testing.T) {
	e := trfStart(t)
	trfRequireTerraform(t, e.tf, 1, 11)
	project := e.inst.ProjectID
	const res = "nebius_storage_v1_transfer.t"
	sensitive := func(version string) string {
		return fmt.Sprintf(`  sensitive = {
    version = %q
    source = { nebius = { access_key = { secret_access_key = %q } } }
    destination = { nebius = { access_key = { secret_access_key = %q } } }
  }
`, version, trfSrcSecret, trfDstSecret)
	}
	c := trfV1{
		name: "tf-transfer-wo", labels: `team = "a"`,
		limiters: `{ bandwidth_bytes_per_second = 1048576, requests_per_second = 100 }`,
		stop:     `after_one_iteration = {}`, overwrite: "IF_NEWER", sensitive: sensitive("1"),
	}
	e.tf.write(t, nebiusProvider(e.inst.Addr, e.inst.Token)+trfBuckets(project)+c.render(project))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{
		"nebius_storage_v1_bucket.src": "create", "nebius_storage_v1_bucket.dst": "create", res: "create",
	})
	e.tf.apply(t)
	id, _ := e.tf.state(t)[res]["id"].(string)
	if id == "" {
		t.Fatalf("transfer has no id in state: %v", e.tf.state(t)[res])
	}
	trfV1Spec(t, "create", e.getV1(t, id))
	for _, s := range []string{trfSrcSecret, trfDstSecret} {
		if trfStateHas(t, e.tf.dir, s) {
			t.Errorf("write-only secret %q leaked into the Terraform state", s)
		}
	}
	e.tf.expectNoChanges(t)

	// Bumping sensitive.version triggers an update that resends the secrets.
	c.sensitive = sensitive("2")
	e.tf.write(t, nebiusProvider(e.inst.Addr, e.inst.Token)+trfBuckets(project)+c.render(project))
	e.tf.expectPlan(t, map[string]string{"nebius_storage_v1_bucket.src": "no-op", "nebius_storage_v1_bucket.dst": "no-op", res: "update"})
	e.tf.apply(t)
	if now, _ := e.tf.state(t)[res]["id"].(string); now != id {
		t.Fatalf("version bump replaced the transfer: %s → %s", id, now)
	}
	trfV1Spec(t, "version bump", e.getV1(t, id))
	for _, s := range []string{trfSrcSecret, trfDstSecret} {
		if trfStateHas(t, e.tf.dir, s) {
			t.Errorf("write-only secret %q leaked into the Terraform state after the version bump", s)
		}
	}
	e.tf.expectNoChanges(t)

	e.tf.destroy(t)
	e.expectGoneV1(t, id)
}

// TestTransferV1S3CompatibleSource reads an external S3 source with anonymous
// access into a Nebius destination bucket.
func TestTransferV1S3CompatibleSource(t *testing.T) {
	e := trfStart(t)
	project := e.inst.ProjectID
	const res = "nebius_storage_v1_transfer.t"
	c := trfV1{
		name: "tf-transfer-s3",
		source: `{
    s3_compatible = {
      endpoint    = "http://127.0.0.1:9"
      region      = "us-east-1"
      bucket_name = "external-bucket"
      anonymous   = {}
    }
    prefix = "in/"
  }`,
		limiters: `{ bandwidth_bytes_per_second = 1048576, requests_per_second = 100 }`,
		stop:     `infinite = {}`, overwrite: "NEVER", interval: "1h",
	}
	e.tf.write(t, nebiusProvider(e.inst.Addr, e.inst.Token)+trfBuckets(project)+c.render(project))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{
		"nebius_storage_v1_bucket.src": "create", "nebius_storage_v1_bucket.dst": "create", res: "create",
	})
	e.tf.apply(t)
	id, _ := e.tf.state(t)[res]["id"].(string)
	got := e.getV1(t, id)
	s3 := got.GetSpec().GetSource().GetS3Compatible()
	if s3.GetEndpoint() != "http://127.0.0.1:9" || s3.GetRegion() != "us-east-1" || s3.GetBucketName() != "external-bucket" ||
		s3.GetAnonymous() == nil || got.GetSpec().GetSource().GetPrefix() != "in/" {
		t.Fatalf("source = %v", got.GetSpec().GetSource())
	}
	if n := got.GetSpec().GetDestination().GetNebius(); n.GetBucketName() != "tf-trf-destination" || got.GetSpec().GetInfinite() == nil ||
		got.GetSpec().GetInterIterationInterval().AsDuration() != time.Hour {
		t.Fatalf("spec = %v", got.GetSpec())
	}
	if got.GetStatus().GetState() == storagepb.TransferStatus_STATE_UNSPECIFIED {
		t.Errorf("status is not populated: %v", got.GetStatus())
	}
	e.tf.expectNoChanges(t)
	e.trfDataSources(t, "s3", res, "id", "name", "parent_id", "overwrite_strategy")

	e.tf.destroy(t)
	e.expectGoneV1(t, id)
}

// trfAlpha describes one v1alpha1 transfer configuration.
type trfAlpha struct {
	limiters  string
	interval  string
	overwrite string
}

func (c trfAlpha) render(project string) string {
	return fmt.Sprintf(`
resource "nebius_storage_v1alpha1_transfer" "t" {
  parent_id = %[1]q
  name      = "tf-transfer-alpha"

  source = {
    endpoint    = "http://127.0.0.1:9"
    bucket_name = "external-bucket"
    region      = "us-east-1"
    prefix      = "in/"
    credentials = { access_key = { access_key_id = "AKSRC", secret_access_key = %[5]q } }
    limiters    = %[2]s
  }

  destination = {
    bucket_name = nebius_storage_v1_bucket.dst.name
    credentials = { access_key = { access_key_id = "AKDST", secret_access_key = %[6]q } }
  }

  after_one_iteration      = {}
  inter_iteration_interval = %[3]q
  overwrite_strategy       = %[4]q
}

data "nebius_storage_v1alpha1_transfer" "by_id" {
  id = nebius_storage_v1alpha1_transfer.t.id
}

data "nebius_storage_v1alpha1_transfer" "by_name" {
  parent_id = nebius_storage_v1alpha1_transfer.t.parent_id
  name      = nebius_storage_v1alpha1_transfer.t.name
}

output "by_id" { value = data.nebius_storage_v1alpha1_transfer.by_id }
output "by_name" { value = data.nebius_storage_v1alpha1_transfer.by_name }
`, project, c.limiters, c.interval, c.overwrite, trfSrcSecret, trfDstSecret)
}

// TestTransferV1Alpha1Lifecycle drives nebius_storage_v1alpha1_transfer and its
// data source: create, empty plan (credentials are INPUT_ONLY as a whole),
// in-place update including overwrite_strategy, data sources, destroy.
func TestTransferV1Alpha1Lifecycle(t *testing.T) {
	e := trfStart(t)
	project := e.inst.ProjectID
	const res = "nebius_storage_v1alpha1_transfer.t"
	cfg := func(c trfAlpha) string {
		return nebiusProvider(e.inst.Addr, e.inst.Token) + trfBuckets(project) + c.render(project)
	}
	noop := func(action string) map[string]string {
		return map[string]string{"nebius_storage_v1_bucket.src": "no-op", "nebius_storage_v1_bucket.dst": "no-op", res: action}
	}
	dsFields := []string{"id", "name", "parent_id", "overwrite_strategy", "inter_iteration_interval"}

	base := trfAlpha{
		limiters: `{ bandwidth_bytes_per_second = 1048576, requests_per_second = 100 }`,
		interval: "20m", overwrite: "ALWAYS",
	}
	e.tf.write(t, cfg(base))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{
		"nebius_storage_v1_bucket.src": "create", "nebius_storage_v1_bucket.dst": "create", res: "create",
	})
	e.tf.apply(t)
	id, _ := e.tf.state(t)[res]["id"].(string)
	if id == "" {
		t.Fatalf("transfer has no id in state: %v", e.tf.state(t)[res])
	}
	check := func(step string, bandwidth uint64, interval time.Duration, overwrite storagev1alpha1pb.TransferSpec_OverwriteStrategy) {
		t.Helper()
		got := e.getAlpha(t, id)
		spec := got.GetSpec()
		if spec.GetSource().GetEndpoint() != "http://127.0.0.1:9" || spec.GetSource().GetBucketName() != "external-bucket" ||
			spec.GetSource().GetRegion() != "us-east-1" || spec.GetSource().GetPrefix() != "in/" ||
			spec.GetDestination().GetBucketName() != "tf-trf-destination" || spec.GetAfterOneIteration() == nil ||
			spec.GetSource().GetLimiters().GetBandwidthBytesPerSecond() != bandwidth ||
			spec.GetInterIterationInterval().AsDuration() != interval || spec.GetOverwriteStrategy() != overwrite {
			t.Fatalf("%s: spec = %v", step, spec)
		}
		if got.GetStatus().GetState() == storagev1alpha1pb.TransferStatus_STATE_UNSPECIFIED {
			t.Errorf("%s: status is not populated: %v", step, got.GetStatus())
		}
		trfNoSecrets(t, step, got)
		// The v1 API sees the transfer created through v1alpha1.
		v1got := e.getV1(t, id)
		if v1got.GetMetadata().GetName() != "tf-transfer-alpha" || v1got.GetSpec().GetDestination().GetNebius().GetBucketName() != "tf-trf-destination" {
			t.Errorf("%s: v1 Get of the v1alpha1 transfer = %v", step, v1got)
		}
		trfNoSecrets(t, step, v1got)
	}
	check("create", 1048576, 20*time.Minute, storagev1alpha1pb.TransferSpec_ALWAYS)
	e.tf.expectNoChanges(t)
	e.trfDataSources(t, "create", res, dsFields...)

	upd := trfAlpha{
		limiters: `{ bandwidth_bytes_per_second = 2097152, requests_per_second = 10 }`,
		interval: "45m", overwrite: "IF_NEWER",
	}
	e.tf.write(t, cfg(upd))
	e.tf.expectPlan(t, noop("update"))
	e.tf.apply(t)
	if now, _ := e.tf.state(t)[res]["id"].(string); now != id {
		t.Fatalf("update replaced the transfer: %s → %s", id, now)
	}
	check("update", 2097152, 45*time.Minute, storagev1alpha1pb.TransferSpec_IF_NEWER)
	e.tf.expectNoChanges(t)
	e.trfDataSources(t, "update", res, dsFields...)

	// v1alpha1 refuses to delete an ACTIVE transfer: let the iteration finish
	// (it fails here, there is no data plane), as a user would wait for it.
	e.trfWaitSettled(t, id)
	e.tf.destroy(t)
	if len(e.tf.state(t)) != 0 {
		t.Fatalf("state after destroy = %v, want empty", e.tf.state(t))
	}
	e.expectGoneAlpha(t, id)
}

// TestTransferRejections proves the mock's validation errors reach Terraform
// users: external-to-external transfers and a region the mock does not serve.
func TestTransferRejections(t *testing.T) {
	e := trfStart(t)
	project := e.inst.ProjectID
	base := nebiusProvider(e.inst.Addr, e.inst.Token) + trfBuckets(project)

	applyFails := func(c trfV1, wants ...string) {
		t.Helper()
		e.tf.write(t, base+c.render(project))
		if _, err := e.tf.tf.Plan(t.Context(), tfexec.Out(planFile)); err != nil {
			t.Fatalf("terraform plan: %v", err)
		}
		e.tf.out.Reset()
		if err := e.tf.tf.Apply(t.Context(), tfexec.DirOrPlan(planFile)); err == nil {
			t.Fatalf("apply succeeded; want an error mentioning %v", wants)
		}
		for _, w := range wants {
			if !strings.Contains(e.tf.out.String(), w) {
				t.Errorf("apply error does not mention %q:\n%s", w, e.tf.out.String())
			}
		}
	}
	external := func(side string) string {
		return fmt.Sprintf(`{
    s3_compatible = {
      endpoint    = "http://127.0.0.1:9"
      region      = "us-east-1"
      bucket_name = "ext-%s"
      anonymous   = {}
    }
  }`, side)
	}
	e.tf.write(t, base)
	e.tf.init(t)

	extExt := trfV1{
		name: "tf-ext-to-ext", source: external("src"), dest: strings.Replace(external("dst"), "anonymous   = {}",
			`access_key = { access_key_id = "AK", secret_access_key = "SK" }`, 1),
		limiters: `{ bandwidth_bytes_per_second = 1048576, requests_per_second = 100 }`,
		stop:     `after_one_iteration = {}`, overwrite: "NEVER",
	}
	applyFails(extExt, "external")

	badRegion := trfV1{
		name: "tf-bad-region",
		dest: `{
    nebius = {
      region      = "eu-west1"
      bucket_name = nebius_storage_v1_bucket.dst.name
      access_key  = { access_key_id = "AKDST", secret_access_key = "SK" }
    }
  }`,
		limiters: `{ bandwidth_bytes_per_second = 1048576, requests_per_second = 100 }`,
		stop:     `after_one_iteration = {}`, overwrite: "NEVER",
	}
	applyFails(badRegion, "eu-west1", "not served")
}
