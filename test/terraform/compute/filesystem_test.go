//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

const (
	fsAddr     = "nebius_compute_v1_filesystem."
	bucketAddr = "nebius_storage_v1_bucket."
	gib        = int64(1) << 30
)

// fsEnv is a mocker (enforce mode) with the Compute and Object Storage
// modules and the official SDK clients for filesystems and buckets.
type fsEnv struct {
	*imagescenario.Env
	Filesystems computesvc.FilesystemService
	Buckets     storagesvc.BucketService
}

func startFilesystemEnv(t *testing.T) *fsEnv {
	t.Helper()
	env := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{storage.New(storage.WithS3Listen("off"))},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	sdk := env.Inst.SDK(t)
	return &fsEnv{Env: env, Filesystems: computesvc.NewFilesystemService(sdk), Buckets: storagesvc.NewBucketService(sdk)}
}

func (e *fsEnv) filesystem(t *testing.T, id string) *computepb.Filesystem {
	t.Helper()
	f, err := e.Filesystems.Get(t.Context(), &computepb.GetFilesystemRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get filesystem %s: %v", id, err)
	}
	return f
}

func (e *fsEnv) assertFilesystemGone(t *testing.T, id string) {
	t.Helper()
	_, err := e.Filesystems.Get(t.Context(), &computepb.GetFilesystemRequest{Id: id})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("filesystem %s: SDK Get code = %v (err %v), want NotFound", id, got, err)
	}
}

func (e *fsEnv) bucket(t *testing.T, id string) *storagepb.Bucket {
	t.Helper()
	b, err := e.Buckets.Get(t.Context(), &storagepb.GetBucketRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get bucket %s: %v", id, err)
	}
	return b
}

func (e *fsEnv) assertBucketGone(t *testing.T, id string) {
	t.Helper()
	_, err := e.Buckets.Get(t.Context(), &storagepb.GetBucketRequest{Id: id})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("bucket %s: SDK Get code = %v (err %v), want NotFound", id, got, err)
	}
}

// fsCfg renders a nebius_compute_v1_filesystem resource.
func fsCfg(res, parent, name, typ string, sizeGiB, block int, labels map[string]string, extra string) string {
	return fmt.Sprintf(`
resource "nebius_compute_v1_filesystem" %q {
  parent_id        = %q
  name             = %q
  type             = %q
  size_gibibytes   = %d
  block_size_bytes = %d
%s%s}
`, res, parent, name, typ, sizeGiB, block, hclLabels(labels), extra)
}

// fsBucketCfg renders a nebius_storage_v1_bucket whose filesystem_bucket
// attribute is the HCL expression filesystemBucket.
func fsBucketCfg(res, parent, name string, labels map[string]string, filesystemBucket string) string {
	return fmt.Sprintf(`
resource "nebius_storage_v1_bucket" %q {
  parent_id         = %q
  name              = %q
%s  filesystem_bucket = %s
}
`, res, parent, name, hclLabels(labels), filesystemBucket)
}

// checkFilesystem compares Terraform's view of a filesystem (state attributes
// or a data source output) with the mock's, read with the SDK.
func checkFilesystem(t *testing.T, tfAttrs map[string]any, want *computepb.Filesystem) {
	t.Helper()
	md, st := want.GetMetadata(), want.GetStatus()
	checks := map[string][2]string{
		"id":                {str(tfAttrs, "id"), md.GetId()},
		"name":              {str(tfAttrs, "name"), md.GetName()},
		"parent_id":         {str(tfAttrs, "parent_id"), md.GetParentId()},
		"type":              {str(tfAttrs, "type"), want.GetSpec().GetType().String()},
		"status.state":      {str(tfAttrs, "status", "state"), st.GetState().String()},
		"status.size_bytes": {str(tfAttrs, "status", "size_bytes"), strconv.FormatInt(st.GetSizeBytes(), 10)},
		"block_size_bytes":  {str(tfAttrs, "block_size_bytes"), strconv.FormatInt(want.GetSpec().GetBlockSizeBytes(), 10)},
		"resource_version":  {str(tfAttrs, "resource_version"), strconv.FormatInt(md.GetResourceVersion(), 10)},
	}
	for field, c := range checks {
		if c[0] != c[1] {
			t.Errorf("filesystem %s: terraform %s = %q, mock has %q", md.GetId(), field, c[0], c[1])
		}
	}
	if got := labelsOf(tfAttrs); !maps.Equal(got, md.GetLabels()) && (len(got) > 0 || len(md.GetLabels()) > 0) {
		t.Errorf("filesystem %s: terraform labels = %v, mock has %v", md.GetId(), got, md.GetLabels())
	}
}

// TestFilesystemLifecycle: create, in-place grow and relabel, data sources,
// replacement on block size change, and a type change the mock refuses.
func TestFilesystemLifecycle(t *testing.T) {
	env := startFilesystemEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	const name = "tf-fs-life"
	cfg := func(typ string, sizeGiB, block int, labels map[string]string) string {
		return fsCfg("main", pid, name, typ, sizeGiB, block, labels, "") + fmt.Sprintf(`
data "nebius_compute_v1_filesystem" "by_id" { id = nebius_compute_v1_filesystem.main.id }
data "nebius_compute_v1_filesystem" "by_name" {
  parent_id  = %q
  name       = %q
  depends_on = [nebius_compute_v1_filesystem.main]
}
output "fs_by_id"   { value = data.nebius_compute_v1_filesystem.by_id }
output "fs_by_name" { value = data.nebius_compute_v1_filesystem.by_name }
`, pid, name)
	}
	tf.writeConfig(t, cfg("NETWORK_SSD", 1, 4096, map[string]string{"env": "dev"}))
	tf.init(t)

	tf.step(t, map[string]string{"main": "create"})
	id := tf.stateID(t, fsAddr+"main")
	f := env.filesystem(t, id)
	checkFilesystem(t, tf.stateOf(t, fsAddr+"main"), f)
	if f.GetStatus().GetState() != computepb.FilesystemStatus_READY || f.GetStatus().GetSizeBytes() != gib ||
		f.GetSpec().GetType() != computepb.FilesystemSpec_NETWORK_SSD || f.GetSpec().GetBlockSizeBytes() != 4096 ||
		f.GetStatus().GetBlockSizeBytes() != 4096 || f.GetMetadata().GetLabels()["env"] != "dev" {
		t.Fatalf("created filesystem = %v", f)
	}
	out := tf.outputs(t)
	for _, o := range []string{"fs_by_id", "fs_by_name"} {
		checkFilesystem(t, out[o], f)
	}

	// Grow in place (not a replacement).
	tf.writeConfig(t, cfg("NETWORK_SSD", 2, 4096, map[string]string{"env": "dev"}))
	tf.step(t, map[string]string{"main": "update"})
	if got := tf.stateID(t, fsAddr+"main"); got != id {
		t.Fatalf("grow changed the id: %s -> %s", id, got)
	}
	f = env.filesystem(t, id)
	checkFilesystem(t, tf.stateOf(t, fsAddr+"main"), f)
	if f.GetStatus().GetSizeBytes() != 2*gib || f.GetStatus().GetState() != computepb.FilesystemStatus_READY {
		t.Fatalf("grown filesystem = %v", f)
	}

	// Labels only.
	tf.writeConfig(t, cfg("NETWORK_SSD", 2, 4096, map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "update"})
	f = env.filesystem(t, id)
	checkFilesystem(t, tf.stateOf(t, fsAddr+"main"), f)
	if f.GetMetadata().GetLabels()["env"] != "prod" || f.GetMetadata().GetLabels()["team"] != "ml" || f.GetStatus().GetSizeBytes() != 2*gib {
		t.Fatalf("relabelled filesystem = %v", f)
	}
	out = tf.outputs(t)
	for _, o := range []string{"fs_by_id", "fs_by_name"} {
		checkFilesystem(t, out[o], f)
	}

	// Block size is immutable: replacement with a new id.
	tf.writeConfig(t, cfg("NETWORK_SSD", 2, 8192, map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "replace"})
	newID := tf.stateID(t, fsAddr+"main")
	if newID == id {
		t.Fatal("block size change did not replace the filesystem")
	}
	env.assertFilesystemGone(t, id)
	f = env.filesystem(t, newID)
	checkFilesystem(t, tf.stateOf(t, fsAddr+"main"), f)
	if f.GetSpec().GetBlockSizeBytes() != 8192 || f.GetStatus().GetBlockSizeBytes() != 8192 {
		t.Fatalf("replaced filesystem = %v", f)
	}

	// Only NETWORK_SSD is available: the plan replaces, the apply is refused.
	tf.writeConfig(t, cfg("NETWORK_HDD", 2, 8192, map[string]string{"env": "prod", "team": "ml"}))
	tf.planIs(t, map[string]string{"main": "replace"})
	before := tf.out.Len()
	err := tf.tf.Apply(t.Context())
	if err == nil {
		t.Fatal("applying a NETWORK_HDD filesystem succeeded")
	}
	if msg := tf.out.String()[before:] + err.Error(); !strings.Contains(msg, "not available") {
		t.Fatalf("apply error does not mention \"not available\":\n%s", msg)
	}
	// A type change replaces: Terraform deletes the old filesystem, then the
	// create is refused, so no filesystem (and certainly no HDD one) remains.
	env.assertFilesystemGone(t, newID)
	l, err := env.Filesystems.List(t.Context(), &computepb.ListFilesystemsRequest{ParentId: pid})
	if err != nil {
		t.Fatalf("SDK List filesystems: %v", err)
	}
	if n := len(l.GetItems()); n != 0 {
		t.Fatalf("%d filesystems exist after the refused apply, want 0", n)
	}

	// Back to an available type: the resource is recreated.
	tf.writeConfig(t, cfg("NETWORK_SSD", 2, 8192, map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "create"})
	lastID := tf.stateID(t, fsAddr+"main")
	checkFilesystem(t, tf.stateOf(t, fsAddr+"main"), env.filesystem(t, lastID))
	tf.destroy(t)
	env.assertFilesystemGone(t, lastID)
}

// TestFilesystemForbidDeletion: destroy of a protected filesystem fails with
// FailedPrecondition; after clearing the flag the destroy succeeds.
func TestFilesystemForbidDeletion(t *testing.T) {
	env := startFilesystemEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	cfg := func(forbid bool) string {
		return fsCfg("keep", pid, "tf-fs-keep", "NETWORK_SSD", 1, 4096, nil, "  forbid_deletion = "+strconv.FormatBool(forbid)+"\n")
	}
	tf.writeConfig(t, cfg(true))
	tf.init(t)
	tf.step(t, map[string]string{"keep": "create"})
	id := tf.stateID(t, fsAddr+"keep")
	if !env.filesystem(t, id).GetSpec().GetForbidDeletion() {
		t.Fatal("forbid_deletion was not applied")
	}

	before := tf.out.Len()
	err := tf.tf.Destroy(t.Context())
	if err == nil {
		t.Fatal("terraform destroy of a forbid_deletion filesystem succeeded")
	}
	if msg := tf.out.String()[before:] + err.Error(); !strings.Contains(msg, "FailedPrecondition") || !strings.Contains(msg, "forbid_deletion") {
		t.Fatalf("destroy failed without FailedPrecondition / forbid_deletion:\n%s", msg)
	}
	if !env.filesystem(t, id).GetSpec().GetForbidDeletion() {
		t.Fatal("filesystem changed after the failed destroy")
	}

	tf.writeConfig(t, cfg(false))
	tf.step(t, map[string]string{"keep": "update"})
	if env.filesystem(t, id).GetSpec().GetForbidDeletion() {
		t.Fatal("forbid_deletion = false was not applied")
	}
	tf.destroy(t)
	env.assertFilesystemGone(t, id)
}

// TestFilesystemBuckets applies a filesystem and filesystem buckets (default
// and explicit settings) in one configuration.
func TestFilesystemBuckets(t *testing.T) {
	env := startFilesystemEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	cfg := func(labels map[string]string) string {
		return fsCfg("fs", pid, "tf-fs-bucket", "NETWORK_SSD", 1, 4096, nil, "") +
			fsBucketCfg("plain", pid, "tf-fsb-plain", labels, "{ filesystem_id = nebius_compute_v1_filesystem.fs.id }") +
			fsBucketCfg("explicit", pid, "tf-fsb-explicit", nil, `{
    filesystem_id  = nebius_compute_v1_filesystem.fs.id
    directory      = "/"
    uid            = 0
    gid            = 0
    file_mode      = "644"
    directory_mode = "755"
  }`)
	}
	tf.writeConfig(t, cfg(map[string]string{"env": "dev"}))
	tf.init(t)
	tf.step(t, map[string]string{"fs": "create", "plain": "create", "explicit": "create"})

	fsID := tf.stateID(t, fsAddr+"fs")
	plainID, explicitID := tf.stateID(t, bucketAddr+"plain"), tf.stateID(t, bucketAddr+"explicit")
	for _, id := range []string{plainID, explicitID} {
		b := env.bucket(t, id)
		if b.GetStatus().GetBucketType() != storagepb.BucketStatus_FILESYSTEM ||
			b.GetSpec().GetFilesystemBucket().GetFilesystemId() != fsID || b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
			t.Fatalf("filesystem bucket %s = %v", id, b)
		}
	}
	if got := env.bucket(t, plainID).GetMetadata().GetLabels()["env"]; got != "dev" {
		t.Fatalf("plain bucket label env = %q, want dev", got)
	}

	// Labels update in place; the filesystem is untouched.
	tf.writeConfig(t, cfg(map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"fs": "no-op", "plain": "update", "explicit": "no-op"})
	if got := tf.stateID(t, bucketAddr+"plain"); got != plainID {
		t.Fatalf("label update changed the bucket id: %s -> %s", plainID, got)
	}
	if l := env.bucket(t, plainID).GetMetadata().GetLabels(); l["env"] != "prod" || l["team"] != "ml" {
		t.Fatalf("plain bucket labels = %v", l)
	}

	// Destroy removes the buckets first, then the filesystem.
	tf.destroy(t)
	env.assertBucketGone(t, plainID)
	env.assertBucketGone(t, explicitID)
	env.assertFilesystemGone(t, fsID)
}

// TestFilesystemBucketUnknownFilesystem: a bucket over a filesystem that does
// not exist is refused.
func TestFilesystemBucketUnknownFilesystem(t *testing.T) {
	env := startFilesystemEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	tf.writeConfig(t, fsBucketCfg("bad", pid, "tf-fsb-bad", nil, `{ filesystem_id = "computefilesystem-e00doesnotexist0000" }`))
	tf.init(t)
	before := tf.out.Len()
	err := tf.tf.Apply(t.Context())
	if err == nil {
		t.Fatal("applying a filesystem bucket over a missing filesystem succeeded")
	}
	if msg := tf.out.String()[before:]; !strings.Contains(msg, "computefilesystem-e00doesnotexist0000") || !strings.Contains(msg, "not found") {
		t.Fatalf("apply error does not name the missing filesystem:\n%s", msg)
	}
	l, err := env.Buckets.List(t.Context(), &storagepb.ListBucketsRequest{ParentId: pid})
	if err != nil {
		t.Fatalf("SDK List buckets: %v", err)
	}
	if n := len(l.GetItems()); n != 0 {
		t.Fatalf("%d buckets exist after the refused apply, want 0", n)
	}
}
