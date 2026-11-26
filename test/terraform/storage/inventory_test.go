//go:build e2e

package storage

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
)

// invBuckets declares the source, a second source and the destination bucket.
func invBuckets(project string) string {
	return fmt.Sprintf(`
resource "nebius_storage_v1_bucket" "src" {
  parent_id = %[1]q
  name      = "tf-inv-source"
}

resource "nebius_storage_v1_bucket" "src2" {
  parent_id = %[1]q
  name      = "tf-inv-source-two"
}

resource "nebius_storage_v1_bucket" "dst" {
  parent_id = %[1]q
  name      = "tf-inv-destination"
}
`, project)
}

// invDataSources reads the inventory back by id and by parent_id + name.
const invDataSources = `
data "nebius_storage_v1_inventory" "by_id" {
  id = nebius_storage_v1_inventory.i.id
}

data "nebius_storage_v1_inventory" "by_name" {
  parent_id = nebius_storage_v1_inventory.i.parent_id
  name      = nebius_storage_v1_inventory.i.name
}

output "by_id" { value = data.nebius_storage_v1_inventory.by_id }
output "by_name" { value = data.nebius_storage_v1_inventory.by_name }
`

// invFields are the attributes shared by the resource and the data source.
var invFields = []string{
	"id", "name", "parent_id", "enabled", "source_prefix", "destination_bucket_id", "destination_prefix",
	"output_format", "noncurrent_versions", "optional_fields", "schedule",
}

func invConfig(name, parent, body string) string {
	return fmt.Sprintf(`
resource "nebius_storage_v1_inventory" "i" {
  parent_id             = %s
  name                  = %q
  destination_bucket_id = nebius_storage_v1_bucket.dst.id
%s}
`, parent, name, body) + invDataSources
}

const invCreateBody = `  destination_prefix  = "reports/v1"
  source_prefix       = "data/"
  schedule            = "DAILY"
  output_format       = "CSV_GZIP"
  enabled             = true
  noncurrent_versions = true
  optional_fields     = ["IS_MULTIPART_UPLOAD", "TAGS"]
`

const invUpdateBody = `  destination_prefix  = "reports/v2"
  schedule            = "WEEKLY"
  output_format       = "PARQUET_SNAPPY"
  enabled             = false
  noncurrent_versions = false
  optional_fields     = ["TAGS", "LAST_ACCESS", "CHECKSUM_ALGORITHMS"]
`

// TestInventoryLifecycle drives nebius_storage_v1_inventory and its data
// source with the official provider: create, empty plan, in-place update, empty
// plan, replace by name and by parent, data source reads, destroy. The mock is
// checked with the Go SDK after every step.
func TestInventoryLifecycle(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("off"))},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	invs := storagesvc.NewInventoryService(inst.SDK(t))
	tf := newTerraform(t)

	const (
		res  = "nebius_storage_v1_inventory.i"
		name = "tf-inventory"
	)
	cfg := func(name, parent, body string) string {
		return nebiusProvider(inst.Addr, inst.Token) + invBuckets(inst.ProjectID) + invConfig(name, parent, body)
	}
	get := func(id string) *storagepb.Inventory {
		t.Helper()
		got, err := invs.Get(t.Context(), &storagepb.GetInventoryRequest{Id: id})
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		return got
	}
	expectGone := func(id string) {
		t.Helper()
		if _, err := invs.Get(t.Context(), &storagepb.GetInventoryRequest{Id: id}); status.Code(err) != codes.NotFound {
			t.Fatalf("Get %s after removal = %v, want NotFound", id, err)
		}
	}
	state := func() map[string]any { return tf.state(t)[res] }
	spec := func(id string, want *storagepb.InventorySpec, step string) {
		t.Helper()
		got := get(id).GetSpec()
		if got.GetEnabled() != want.GetEnabled() || got.GetSourcePrefix() != want.GetSourcePrefix() ||
			got.GetDestinationBucketId() != want.GetDestinationBucketId() || got.GetDestinationPrefix() != want.GetDestinationPrefix() ||
			got.GetOutputFormat() != want.GetOutputFormat() || got.GetNoncurrentVersions() != want.GetNoncurrentVersions() ||
			got.GetSchedule() != want.GetSchedule() || !reflect.DeepEqual(got.GetOptionalFields(), want.GetOptionalFields()) {
			t.Fatalf("%s: mock spec = %v, want %v", step, got, want)
		}
	}
	// dataSourcesMatch compares both data source reads with the resource.
	dataSourcesMatch := func(step string) {
		t.Helper()
		st := state()
		for _, out := range []string{"by_id", "by_name"} {
			ds, ok := tf.output(t, out).(map[string]any)
			if !ok {
				t.Fatalf("%s: output %s is not an object", step, out)
			}
			for _, f := range invFields {
				// The provider leaves a proto3 false bool null in a data source;
				// the Go SDK checks show the mock returns false, as for the resource.
				if ds[f] == nil && st[f] == false {
					continue
				}
				if !reflect.DeepEqual(ds[f], st[f]) {
					t.Errorf("%s: data source %s.%s = %#v, resource has %#v", step, out, f, ds[f], st[f])
				}
			}
		}
	}
	noop := func(inv string) map[string]string {
		return map[string]string{
			"nebius_storage_v1_bucket.src": "no-op", "nebius_storage_v1_bucket.src2": "no-op",
			"nebius_storage_v1_bucket.dst": "no-op", res: inv,
		}
	}

	// 1. Create.
	tf.write(t, cfg(name, "nebius_storage_v1_bucket.src.id", invCreateBody))
	tf.init(t)
	tf.expectPlan(t, map[string]string{
		"nebius_storage_v1_bucket.src": "create", "nebius_storage_v1_bucket.src2": "create",
		"nebius_storage_v1_bucket.dst": "create", res: "create",
	})
	tf.apply(t)
	id, _ := state()["id"].(string)
	srcID, _ := tf.state(t)["nebius_storage_v1_bucket.src"]["id"].(string)
	src2ID, _ := tf.state(t)["nebius_storage_v1_bucket.src2"]["id"].(string)
	dstID, _ := tf.state(t)["nebius_storage_v1_bucket.dst"]["id"].(string)
	if id == "" {
		t.Fatalf("inventory has no id in state: %v", state())
	}
	if got := get(id).GetMetadata(); got.GetParentId() != srcID || got.GetName() != name {
		t.Fatalf("create: mock metadata = %v, want parent %s name %s", got, srcID, name)
	}
	spec(id, &storagepb.InventorySpec{
		Enabled: true, SourcePrefix: "data/", DestinationBucketId: dstID, DestinationPrefix: "reports/v1",
		OutputFormat: storagepb.InventorySpec_CSV_GZIP, NoncurrentVersions: true, Schedule: storagepb.InventorySpec_DAILY,
		OptionalFields: []storagepb.InventorySpec_OptionalField{storagepb.InventorySpec_IS_MULTIPART_UPLOAD, storagepb.InventorySpec_TAGS},
	}, "create")

	// 2. Empty plan, data sources agree.
	tf.expectNoChanges(t)
	dataSourcesMatch("create")

	// 3. In-place update: the plan must be an update, not a replace.
	tf.write(t, cfg(name, "nebius_storage_v1_bucket.src.id", invUpdateBody))
	tf.expectPlan(t, noop("update"))
	tf.apply(t)
	if got, _ := state()["id"].(string); got != id {
		t.Fatalf("update replaced the inventory: %s → %s", id, got)
	}
	spec(id, &storagepb.InventorySpec{
		Enabled: false, SourcePrefix: "", DestinationBucketId: dstID, DestinationPrefix: "reports/v2",
		OutputFormat: storagepb.InventorySpec_PARQUET_SNAPPY, NoncurrentVersions: false, Schedule: storagepb.InventorySpec_WEEKLY,
		OptionalFields: []storagepb.InventorySpec_OptionalField{
			storagepb.InventorySpec_TAGS, storagepb.InventorySpec_LAST_ACCESS, storagepb.InventorySpec_CHECKSUM_ALGORITHMS,
		},
	}, "update")
	if v := get(id).GetMetadata().GetResourceVersion(); v != 2 {
		t.Errorf("resource_version = %d, want 2 after one update", v)
	}

	// 4. Empty plan, data sources agree.
	tf.expectNoChanges(t)
	dataSourcesMatch("update")

	// 5a. Replace: a new name is RequiresReplace.
	const renamed = "tf-inventory-renamed"
	tf.write(t, cfg(renamed, "nebius_storage_v1_bucket.src.id", invUpdateBody))
	tf.expectPlan(t, noop("replace"))
	tf.apply(t)
	id2, _ := state()["id"].(string)
	if id2 == "" || id2 == id {
		t.Fatalf("replace by name: id %q, want a new id (old %s)", id2, id)
	}
	expectGone(id)
	if got := get(id2).GetMetadata(); got.GetName() != renamed || got.GetParentId() != srcID {
		t.Fatalf("replace by name: mock metadata = %v", got)
	}
	tf.expectNoChanges(t)
	dataSourcesMatch("replace by name")

	// 5b. Replace: moving to another source bucket is RequiresReplace too.
	tf.write(t, cfg(renamed, "nebius_storage_v1_bucket.src2.id", invUpdateBody))
	tf.expectPlan(t, noop("replace"))
	tf.apply(t)
	id3, _ := state()["id"].(string)
	if id3 == "" || id3 == id2 {
		t.Fatalf("replace by parent: id %q, want a new id (old %s)", id3, id2)
	}
	expectGone(id2)
	if got := get(id3).GetMetadata(); got.GetParentId() != src2ID {
		t.Fatalf("replace by parent: mock metadata = %v, want parent %s", got, src2ID)
	}
	tf.expectNoChanges(t)
	dataSourcesMatch("replace by parent")

	// 7. Destroy.
	tf.destroy(t)
	if len(tf.state(t)) != 0 {
		t.Fatalf("state after destroy = %v, want empty", tf.state(t))
	}
	expectGone(id3)
}

// TestInventoryRejections proves the mock's validation and quota errors reach
// Terraform users, naming the field or the limit.
func TestInventoryRejections(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("off"))},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	tf := newTerraform(t)
	base := nebiusProvider(inst.Addr, inst.Token) + invBuckets(inst.ProjectID)

	applyFails := func(cfg string, wants ...string) {
		t.Helper()
		tf.write(t, cfg)
		if _, err := tf.tf.Plan(t.Context(), tfexec.Out(planFile)); err != nil {
			t.Fatalf("terraform plan: %v", err)
		}
		tf.out.Reset()
		if err := tf.tf.Apply(t.Context(), tfexec.DirOrPlan(planFile)); err == nil {
			t.Fatalf("apply succeeded; want an error mentioning %v", wants)
		}
		for _, w := range wants {
			if !strings.Contains(tf.out.String(), w) {
				t.Errorf("apply error does not mention %q:\n%s", w, tf.out.String())
			}
		}
	}

	// A destination_prefix over 256 characters is rejected, naming the field.
	tooLong := base + fmt.Sprintf(`
resource "nebius_storage_v1_inventory" "i" {
  parent_id             = nebius_storage_v1_bucket.src.id
  name                  = "too-long"
  destination_bucket_id = nebius_storage_v1_bucket.dst.id
  destination_prefix    = %q
  schedule              = "DAILY"
  output_format         = "CSV_GZIP"
}
`, strings.Repeat("p", 257))
	// The official provider validates the length itself, so Terraform stops at
	// plan; the mock's own check is covered by the contract tests.
	tf.write(t, tooLong)
	tf.init(t)
	tf.out.Reset()
	if _, err := tf.tf.Plan(t.Context()); err == nil || !strings.Contains(tf.out.String(), "destination_prefix") {
		t.Fatalf("plan with a 257-character destination_prefix: err = %v, output:\n%s", err, tf.out.String())
	}

	// A duplicate optional field passes the provider and is rejected by the mock.
	dup := base + `
resource "nebius_storage_v1_inventory" "i" {
  parent_id             = nebius_storage_v1_bucket.src.id
  name                  = "dup-fields"
  destination_bucket_id = nebius_storage_v1_bucket.dst.id
  schedule              = "DAILY"
  output_format         = "CSV_GZIP"
  optional_fields       = ["TAGS", "TAGS"]
}
`
	applyFails(dup, "duplicate of an earlier entry")
}

// TestInventoryQuota creates six inventories on one bucket in one apply
// (Terraform runs them in parallel): exactly five fit, and the sixth fails with
// the limit in the error.
func TestInventoryQuota(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("off"))},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	invs := storagesvc.NewInventoryService(inst.SDK(t))
	tf := newTerraform(t)
	tf.write(t, nebiusProvider(inst.Addr, inst.Token)+invBuckets(inst.ProjectID)+`
resource "nebius_storage_v1_inventory" "many" {
  count                 = 6
  parent_id             = nebius_storage_v1_bucket.src.id
  name                  = "inv-${count.index}"
  destination_bucket_id = nebius_storage_v1_bucket.dst.id
  schedule              = "DAILY"
  output_format         = "CSV_GZIP"
}
`)
	tf.init(t)
	if _, err := tf.tf.Plan(t.Context(), tfexec.Out(planFile)); err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	if err := tf.tf.Apply(t.Context(), tfexec.DirOrPlan(planFile)); err == nil {
		t.Fatalf("apply of 6 inventories on one bucket succeeded; want the limit of 5 to reject one")
	}
	if got := tf.out.String(); !strings.Contains(got, "at most 5 inventory") {
		t.Errorf("apply error does not mention the limit of 5:\n%s", got)
	}
	srcID, _ := tf.state(t)["nebius_storage_v1_bucket.src"]["id"].(string)
	n := 0
	for _, err := range invs.Filter(t.Context(), &storagepb.ListInventoriesRequest{ParentId: srcID}) {
		if err != nil {
			t.Fatalf("list inventories: %v", err)
		}
		n++
	}
	if n != 5 {
		t.Errorf("bucket has %d inventories after the apply, want exactly 5", n)
	}
}
