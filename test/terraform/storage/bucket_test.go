//go:build e2e

package storage

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
)

type bucketStep struct {
	labels     string
	versioning string // "" leaves the attribute out (server default DISABLED)
	maxSize    int64
	check      func(*storagepb.Bucket) bool
}

func bucketConfig(project, name string, st bucketStep) string {
	versioning := ""
	if st.versioning != "" {
		versioning = fmt.Sprintf("  versioning_policy = %q\n", st.versioning)
	}
	return fmt.Sprintf(`
resource "nebius_storage_v1_bucket" "b" {
  parent_id      = %q
  name           = %q
  labels         = { %s }
  max_size_bytes = %d
%s}

data "nebius_storage_v1_bucket" "by_name" {
  parent_id = nebius_storage_v1_bucket.b.parent_id
  name      = nebius_storage_v1_bucket.b.name
}

output "by_name_id" { value = data.nebius_storage_v1_bucket.by_name.id }
`, project, name, st.labels, st.maxSize, versioning)
}

// TestBucketLifecycle drives nebius_storage_v1_bucket through create → empty
// plan → in-place updates (labels, versioning, max size) → no drift → destroy
// with the official provider, checking the mock with the Go SDK after every step.
func TestBucketLifecycle(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("off"))},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	buckets := storagesvc.NewBucketService(inst.SDK(t))
	tf := newTerraform(t)
	const name = "tf-lifecycle-bucket"
	get := func(id string) *storagepb.Bucket {
		t.Helper()
		b, err := buckets.Get(t.Context(), &storagepb.GetBucketRequest{Id: id})
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		return b
	}

	steps := []bucketStep{
		{labels: `team = "a"`, maxSize: 0, check: func(b *storagepb.Bucket) bool {
			return b.GetMetadata().GetLabels()["team"] == "a" && b.GetSpec().GetVersioningPolicy() == storagepb.VersioningPolicy_DISABLED &&
				b.GetStatus().GetState() == storagepb.BucketStatus_ACTIVE
		}},
		{labels: `team = "b", tier = "1"`, versioning: "ENABLED", maxSize: 1 << 30, check: func(b *storagepb.Bucket) bool {
			return b.GetMetadata().GetLabels()["team"] == "b" && b.GetSpec().GetVersioningPolicy() == storagepb.VersioningPolicy_ENABLED &&
				b.GetSpec().GetMaxSizeBytes() == 1<<30
		}},
		{labels: `tier = "1"`, versioning: "SUSPENDED", maxSize: 2 << 30, check: func(b *storagepb.Bucket) bool {
			_, hasTeam := b.GetMetadata().GetLabels()["team"]
			return !hasTeam && b.GetSpec().GetVersioningPolicy() == storagepb.VersioningPolicy_SUSPENDED && b.GetSpec().GetMaxSizeBytes() == 2<<30
		}},
	}
	var id string
	for i, st := range steps {
		tf.write(t, nebiusProvider(inst.Addr, inst.Token)+bucketConfig(inst.ProjectID, name, st))
		action := "update"
		if i == 0 {
			tf.init(t)
			action = "create"
		}
		tf.expectPlan(t, map[string]string{"nebius_storage_v1_bucket.b": action})
		tf.apply(t)
		got, _ := tf.state(t)["nebius_storage_v1_bucket.b"]["id"].(string)
		if i == 0 {
			id = got
		} else if got != id {
			t.Fatalf("step %d: bucket replaced: %s → %s", i, id, got)
		}
		if b := get(id); !st.check(b) {
			t.Fatalf("step %d: mock bucket %v does not reflect the config", i, b)
		}
		if out := tf.output(t, "by_name_id"); out != id {
			t.Fatalf("step %d: data source by name = %v, want %s", i, out, id)
		}
		tf.expectNoChanges(t)
	}
	if v := get(id).GetMetadata().GetResourceVersion(); v != 3 {
		t.Errorf("resource_version = %d, want 3 after two updates", v)
	}

	tf.destroy(t)
	if len(tf.state(t)) != 0 {
		t.Fatalf("state after destroy = %v, want empty", tf.state(t))
	}
	// Nebius soft-deletes: the bucket stays restorable until purge_at.
	if b := get(id); b.GetStatus().GetState() != storagepb.BucketStatus_SCHEDULED_FOR_DELETION || b.GetStatus().GetPurgeAt() == nil {
		t.Fatalf("after destroy: status %v, want SCHEDULED_FOR_DELETION with purge_at", b.GetStatus())
	}
}

func settingsConfig(tenant, project, name, class string) string {
	return fmt.Sprintf(`
resource "nebius_iam_v1_group" "readers" {
  parent_id = %[1]q
  name      = "readers-%[3]s"
}

resource "nebius_storage_v1_bucket" "s" {
  parent_id             = %[2]q
  name                  = %[3]q
  versioning_policy     = "ENABLED"
  max_size_bytes        = 1048576
  default_storage_class = %[4]q

  cors = {
    rules = [{
      id              = "web"
      allowed_methods = ["GET", "HEAD"]
      allowed_origins = ["https://example.com"]
      allowed_headers = ["*"]
      max_age_seconds = 600
    }]
  }

  bucket_policy = {
    rules = [{
      group_id = nebius_iam_v1_group.readers.id
      paths    = ["public/*"]
      roles    = ["storage.object-viewer"]
    }]
  }

  lifecycle_configuration = {
    rules = [{
      id     = "expire-tmp"
      status = "ENABLED"
      filter     = { prefix = "tmp/" }
      expiration = { days = 7 }
    }]
  }
}
`, tenant, project, name, class)
}

// TestBucketSettings applies the settings the mock acts on (cors, bucket
// policy, lifecycle, max size) and proves they round-trip without drift; a
// setting the mock cannot emulate fails the apply, naming the field.
func TestBucketSettings(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("off"))},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	buckets := storagesvc.NewBucketService(inst.SDK(t))
	tf := newTerraform(t)
	const name = "tf-settings-bucket"

	tf.write(t, nebiusProvider(inst.Addr, inst.Token)+settingsConfig(inst.TenantID, inst.ProjectID, name, "STANDARD"))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"nebius_iam_v1_group.readers": "create", "nebius_storage_v1_bucket.s": "create"})
	tf.apply(t)
	tf.expectNoChanges(t)

	id, _ := tf.state(t)["nebius_storage_v1_bucket.s"]["id"].(string)
	b, err := buckets.Get(t.Context(), &storagepb.GetBucketRequest{Id: id})
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	sp := b.GetSpec()
	if sp.GetMaxSizeBytes() != 1<<20 || len(sp.GetCors().GetRules()) != 1 || len(sp.GetBucketPolicy().GetRules()) != 1 ||
		sp.GetLifecycleConfiguration().GetRules()[0].GetExpiration().GetDays() != 7 {
		t.Fatalf("mock spec does not reflect the config: %v", sp)
	}
	if b.GetStatus().GetAnonymousAccessEnabled() {
		t.Fatalf("anonymous_access_enabled = true for a group-only policy")
	}

	// Add an anonymous rule: status.anonymous_access_enabled follows.
	cfg := settingsConfig(inst.TenantID, inst.ProjectID, name, "STANDARD")
	cfg = strings.Replace(cfg, "    rules = [{\n      group_id", `    rules = [{
      anonymous = {}
      paths     = ["open/*"]
      roles     = ["storage.viewer"]
    }, {
      group_id`, 1)
	tf.write(t, nebiusProvider(inst.Addr, inst.Token)+cfg)
	tf.expectPlan(t, map[string]string{"nebius_iam_v1_group.readers": "no-op", "nebius_storage_v1_bucket.s": "update"})
	tf.apply(t)
	tf.expectNoChanges(t)
	b, err = buckets.Get(t.Context(), &storagepb.GetBucketRequest{Id: id})
	if err != nil || !b.GetStatus().GetAnonymousAccessEnabled() || len(b.GetSpec().GetBucketPolicy().GetRules()) != 2 {
		t.Fatalf("after anonymous rule: %v, %v; want anonymous_access_enabled", b, err)
	}

	// Unsupported: INTELLIGENT storage class fails the update, naming the field.
	tf.write(t, nebiusProvider(inst.Addr, inst.Token)+settingsConfig(inst.TenantID, inst.ProjectID, name, "INTELLIGENT"))
	if _, err := tf.tf.Plan(t.Context(), tfexec.Out(planFile)); err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	err = tf.tf.Apply(t.Context(), tfexec.DirOrPlan(planFile))
	if err == nil {
		t.Fatalf("apply with default_storage_class = INTELLIGENT succeeded; want an error")
	}
	if got := tf.out.String(); !strings.Contains(got, "default_storage_class") {
		t.Errorf("apply error does not mention default_storage_class:\n%s%v", got, err)
	}
}
