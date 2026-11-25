//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

const (
	diskAddr = "nebius_compute_v1_disk."
	snapAddr = "nebius_compute_v1_disk_snapshot."
)

// diskCfg renders a nebius_compute_v1_disk resource; block 0 is omitted.
func diskCfg(res, parent, name, typ string, sizeGiB, block int, labels map[string]string, extra string) string {
	b := ""
	if block > 0 {
		b = fmt.Sprintf("  block_size_bytes = %d\n", block)
	}
	return fmt.Sprintf(`
resource "nebius_compute_v1_disk" %q {
  parent_id      = %q
  name           = %q
  type           = %q
  size_gibibytes = %d
%s%s%s}
`, res, parent, name, typ, sizeGiB, b, hclLabels(labels), extra)
}

func snapCfg(res, parent, name, sourceDisk, desc string, labels map[string]string) string {
	d := ""
	if desc != "" {
		d = fmt.Sprintf("  description = %q\n", desc)
	}
	return fmt.Sprintf(`
resource "nebius_compute_v1_disk_snapshot" %q {
  parent_id      = %q
  name           = %q
  source_disk_id = %s
%s%s}
`, res, parent, name, sourceDisk, d, hclLabels(labels))
}

// TestDiskLifecycle: create, in-place grow/relabel, replace on type change and
// on block size change, destroy; every step is followed by an empty plan and
// checked against the mock with the SDK.
func TestDiskLifecycle(t *testing.T) {
	env := startDiskEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	cfg := func(typ string, size, block int, labels map[string]string) string {
		// "plain" omits block_size_bytes: status says 4096, the spec stays unset.
		return diskCfg("main", pid, "tf-disk-main", typ, size, block, labels, "") +
			diskCfg("plain", pid, "tf-disk-plain", "NETWORK_SSD", 10, 0, nil, "")
	}
	tf.writeConfig(t, cfg("NETWORK_SSD", 10, 4096, map[string]string{"env": "dev"}))
	tf.init(t)

	tf.step(t, map[string]string{"main": "create", "plain": "create"})
	mainID := tf.stateID(t, diskAddr+"main")
	plainID := tf.stateID(t, diskAddr+"plain")
	d := env.disk(t, mainID)
	checkDisk(t, tf.stateOf(t, diskAddr+"main"), d)
	if d.GetStatus().GetSizeBytes() != 10<<30 || d.GetSpec().GetType() != computepb.DiskSpec_NETWORK_SSD || d.GetMetadata().GetLabels()["env"] != "dev" ||
		d.GetStatus().GetState() != computepb.DiskStatus_READY {
		t.Fatalf("created disk = %v", d)
	}
	plain := env.disk(t, plainID)
	checkDisk(t, tf.stateOf(t, diskAddr+"plain"), plain)
	if plain.GetStatus().GetBlockSizeBytes() != 4096 || plain.GetSpec().GetBlockSizeBytes() != 0 {
		t.Fatalf("plain disk block size: status %d spec %d, want 4096 / 0", plain.GetStatus().GetBlockSizeBytes(), plain.GetSpec().GetBlockSizeBytes())
	}

	// In-place update: new labels and a bigger size; same id.
	tf.writeConfig(t, cfg("NETWORK_SSD", 20, 4096, map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "update", "plain": "no-op"})
	if got := tf.stateID(t, diskAddr+"main"); got != mainID {
		t.Fatalf("in-place update changed the id: %s -> %s", mainID, got)
	}
	d = env.disk(t, mainID)
	checkDisk(t, tf.stateOf(t, diskAddr+"main"), d)
	if d.GetStatus().GetSizeBytes() != 20<<30 || d.GetMetadata().GetLabels()["env"] != "prod" || d.GetMetadata().GetLabels()["team"] != "ml" ||
		d.GetStatus().GetState() != computepb.DiskStatus_READY {
		t.Fatalf("updated disk = %v", d)
	}

	// Type change: replace with a new id; the old disk is gone.
	tf.writeConfig(t, cfg("NETWORK_SSD_IO_M3", 93, 4096, map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "replace", "plain": "no-op"})
	typeID := tf.stateID(t, diskAddr+"main")
	if typeID == mainID {
		t.Fatal("type change did not replace the disk")
	}
	env.assertDiskGone(t, mainID)
	d = env.disk(t, typeID)
	checkDisk(t, tf.stateOf(t, diskAddr+"main"), d)
	if d.GetSpec().GetType() != computepb.DiskSpec_NETWORK_SSD_IO_M3 || d.GetStatus().GetSizeBytes() != 93<<30 {
		t.Fatalf("replaced disk = %v", d)
	}

	// Block size change: replace again.
	tf.writeConfig(t, cfg("NETWORK_SSD_IO_M3", 93, 8192, map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "replace", "plain": "no-op"})
	blockID := tf.stateID(t, diskAddr+"main")
	if blockID == typeID {
		t.Fatal("block size change did not replace the disk")
	}
	env.assertDiskGone(t, typeID)
	d = env.disk(t, blockID)
	checkDisk(t, tf.stateOf(t, diskAddr+"main"), d)
	if d.GetStatus().GetBlockSizeBytes() != 8192 || d.GetSpec().GetBlockSizeBytes() != 8192 {
		t.Fatalf("block-size disk = %v", d)
	}
	if got := tf.stateID(t, diskAddr+"plain"); got != plainID {
		t.Fatalf("untouched disk changed id: %s -> %s", plainID, got)
	}

	tf.destroy(t)
	env.assertDiskGone(t, blockID)
	env.assertDiskGone(t, plainID)
}

// TestDiskSnapshotLifecycle: snapshot create, in-place description/labels
// update, replace when the source disk changes, destroy.
func TestDiskSnapshotLifecycle(t *testing.T) {
	env := startDiskEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	cfg := func(source, desc string, labels map[string]string) string {
		return diskCfg("a", pid, "tf-snap-src-a", "NETWORK_SSD", 10, 0, nil, "") +
			diskCfg("b", pid, "tf-snap-src-b", "NETWORK_SSD", 16, 0, nil, "") +
			snapCfg("snap", pid, "tf-snap", "nebius_compute_v1_disk."+source+".id", desc, labels)
	}
	tf.writeConfig(t, cfg("a", "first", map[string]string{"k": "v1"}))
	tf.init(t)
	tf.step(t, map[string]string{"a": "create", "b": "create", "snap": "create"})
	aID, bID := tf.stateID(t, diskAddr+"a"), tf.stateID(t, diskAddr+"b")
	snapID := tf.stateID(t, snapAddr+"snap")
	s := env.snapshot(t, snapID)
	checkSnapshot(t, tf.stateOf(t, snapAddr+"snap"), s)
	if s.GetSpec().GetSourceDiskId() != aID || s.GetSpec().GetDescription() != "first" || s.GetStatus().GetContentSizeBytes() != 10<<30 ||
		s.GetStatus().GetState() != computepb.DiskSnapshotStatus_READY {
		t.Fatalf("created snapshot = %v", s)
	}

	tf.writeConfig(t, cfg("a", "second", map[string]string{"k": "v2", "x": "y"}))
	tf.step(t, map[string]string{"a": "no-op", "b": "no-op", "snap": "update"})
	if got := tf.stateID(t, snapAddr+"snap"); got != snapID {
		t.Fatalf("in-place snapshot update changed the id: %s -> %s", snapID, got)
	}
	s = env.snapshot(t, snapID)
	checkSnapshot(t, tf.stateOf(t, snapAddr+"snap"), s)
	if s.GetSpec().GetDescription() != "second" || s.GetMetadata().GetLabels()["k"] != "v2" || s.GetMetadata().GetLabels()["x"] != "y" {
		t.Fatalf("updated snapshot = %v", s)
	}

	tf.writeConfig(t, cfg("b", "second", map[string]string{"k": "v2", "x": "y"}))
	tf.step(t, map[string]string{"a": "no-op", "b": "no-op", "snap": "replace"})
	newSnap := tf.stateID(t, snapAddr+"snap")
	if newSnap == snapID {
		t.Fatal("changing source_disk_id did not replace the snapshot")
	}
	env.assertSnapshotGone(t, snapID)
	s = env.snapshot(t, newSnap)
	checkSnapshot(t, tf.stateOf(t, snapAddr+"snap"), s)
	if s.GetSpec().GetSourceDiskId() != bID || s.GetStatus().GetContentSizeBytes() != 16<<30 {
		t.Fatalf("replaced snapshot = %v", s)
	}

	tf.destroy(t)
	env.assertSnapshotGone(t, newSnap)
	env.assertDiskGone(t, aID)
	env.assertDiskGone(t, bID)
}

// TestDiskDataSources reads disks and snapshots by id and by parent_id + name.
func TestDiskDataSources(t *testing.T) {
	env := startDiskEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	tf.writeConfig(t, diskCfg("d", pid, "ds-disk", "NETWORK_SSD", 12, 0, map[string]string{"role": "data"}, "")+
		snapCfg("s", pid, "ds-snap", "nebius_compute_v1_disk.d.id", "for data sources", map[string]string{"role": "backup"})+fmt.Sprintf(`
data "nebius_compute_v1_disk" "by_id" { id = nebius_compute_v1_disk.d.id }
data "nebius_compute_v1_disk" "by_name" {
  parent_id  = %[1]q
  name       = "ds-disk"
  depends_on = [nebius_compute_v1_disk.d]
}
data "nebius_compute_v1_disk_snapshot" "by_id" { id = nebius_compute_v1_disk_snapshot.s.id }
data "nebius_compute_v1_disk_snapshot" "by_name" {
  parent_id  = %[1]q
  name       = "ds-snap"
  depends_on = [nebius_compute_v1_disk_snapshot.s]
}
output "disk_by_id"       { value = data.nebius_compute_v1_disk.by_id }
output "disk_by_name"     { value = data.nebius_compute_v1_disk.by_name }
output "snapshot_by_id"   { value = data.nebius_compute_v1_disk_snapshot.by_id }
output "snapshot_by_name" { value = data.nebius_compute_v1_disk_snapshot.by_name }
`, pid))
	tf.init(t)
	tf.apply(t)
	tf.requireEmptyPlan(t)

	diskID, snapID := tf.stateID(t, diskAddr+"d"), tf.stateID(t, snapAddr+"s")
	disk, snap := env.disk(t, diskID), env.snapshot(t, snapID)
	out := tf.outputs(t)
	for _, name := range []string{"disk_by_id", "disk_by_name"} {
		checkDisk(t, out[name], disk)
		if got := str(out[name], "size_gibibytes"); got != "12" {
			t.Errorf("%s: size_gibibytes = %q, want 12", name, got)
		}
	}
	for _, name := range []string{"snapshot_by_id", "snapshot_by_name"} {
		checkSnapshot(t, out[name], snap)
	}
	if disk.GetStatus().GetSizeBytes() != 12<<30 || snap.GetStatus().GetContentSizeBytes() != 12<<30 {
		t.Fatalf("sizes: disk %d snapshot %d", disk.GetStatus().GetSizeBytes(), snap.GetStatus().GetContentSizeBytes())
	}
	tf.destroy(t)
	env.assertSnapshotGone(t, snapID)
	env.assertDiskGone(t, diskID)
}

// TestDiskRealisticFlow: boot disk from a public image family, snapshot it,
// restore a bigger disk from the snapshot, then destroy everything.
func TestDiskRealisticFlow(t *testing.T) {
	env := startDiskEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	tf.writeConfig(t, diskCfg("boot", pid, "flow-boot", "NETWORK_SSD", 32, 0, map[string]string{"role": "boot"},
		"  source_image_family = {\n    image_family = \"ubuntu24.04-driverless\"\n  }\n")+
		snapCfg("snap", pid, "flow-snap", "nebius_compute_v1_disk.boot.id", "", nil)+
		diskCfg("restored", pid, "flow-restored", "NETWORK_SSD", 64, 0, nil,
			"  source_snapshot_id = nebius_compute_v1_disk_snapshot.snap.id\n"))
	tf.init(t)
	tf.step(t, map[string]string{"boot": "create", "snap": "create", "restored": "create"})
	bootID, snapID, restoredID := tf.stateID(t, diskAddr+"boot"), tf.stateID(t, snapAddr+"snap"), tf.stateID(t, diskAddr+"restored")

	boot := env.disk(t, bootID)
	checkDisk(t, tf.stateOf(t, diskAddr+"boot"), boot)
	if boot.GetStatus().GetSourceImageId() == "" || boot.GetStatus().GetSizeBytes() != 32<<30 {
		t.Fatalf("boot disk = %v", boot)
	}
	snap := env.snapshot(t, snapID)
	checkSnapshot(t, tf.stateOf(t, snapAddr+"snap"), snap)
	if snap.GetSpec().GetSourceDiskId() != bootID || snap.GetStatus().GetContentSizeBytes() != 32<<30 {
		t.Fatalf("snapshot = %v", snap)
	}
	restored := env.disk(t, restoredID)
	checkDisk(t, tf.stateOf(t, diskAddr+"restored"), restored)
	if restored.GetSpec().GetSourceSnapshotId() != snapID || restored.GetStatus().GetSizeBytes() != 64<<30 {
		t.Fatalf("restored disk = %v", restored)
	}
	tf.requireEmptyPlan(t)

	tf.destroy(t)
	env.assertDiskGone(t, restoredID)
	env.assertSnapshotGone(t, snapID)
	env.assertDiskGone(t, bootID)
}

// TestDiskForbidDeletion: destroy of a disk with forbid_deletion fails with
// FailedPrecondition; after clearing the flag the destroy succeeds.
func TestDiskForbidDeletion(t *testing.T) {
	env := startDiskEnv(t)
	pid := env.Inst.ProjectID
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	cfg := func(forbid bool) string {
		return diskCfg("keep", pid, "tf-keep", "NETWORK_SSD", 10, 0, nil, "  forbid_deletion = "+strconv.FormatBool(forbid)+"\n")
	}
	tf.writeConfig(t, cfg(true))
	tf.init(t)
	tf.step(t, map[string]string{"keep": "create"})
	id := tf.stateID(t, diskAddr+"keep")
	if !env.disk(t, id).GetSpec().GetForbidDeletion() {
		t.Fatal("forbid_deletion was not applied")
	}

	before := tf.out.Len()
	err := tf.tf.Destroy(t.Context())
	if err == nil {
		t.Fatal("terraform destroy of a forbid_deletion disk succeeded")
	}
	if out := tf.out.String()[before:] + err.Error(); !strings.Contains(out, "FailedPrecondition") {
		t.Fatalf("destroy failed without FailedPrecondition:\n%s", out)
	}
	if !env.disk(t, id).GetSpec().GetForbidDeletion() {
		t.Fatal("disk changed after the failed destroy")
	}

	tf.writeConfig(t, cfg(false))
	tf.step(t, map[string]string{"keep": "update"})
	if env.disk(t, id).GetSpec().GetForbidDeletion() {
		t.Fatal("forbid_deletion = false was not applied")
	}
	tf.destroy(t)
	env.assertDiskGone(t, id)
}
