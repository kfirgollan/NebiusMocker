//go:build e2e

// Package computedisk_test runs the disk and disk snapshot workflows from
// https://docs.nebius.com/compute/storage/manage and
// https://docs.nebius.com/compute/storage/disk-snapshots with the official
// nebius CLI against an in-process mocker. Disks are control plane only, so
// no Docker is needed.
package computedisk_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

const gib = int64(1) << 30

func newCLI(t *testing.T) *nebiuscli.CLI {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(50*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	return nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
}

// run runs a CLI command and returns its trimmed stdout.
func run(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	res, err := cli.Run(t.Context(), args...)
	if err != nil {
		t.Fatalf("nebius %s: %v\nstderr: %s", strings.Join(args, " "), err, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// fails runs a CLI command that must fail and returns its output.
func fails(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	res, err := cli.Run(t.Context(), args...)
	if err == nil {
		t.Fatalf("nebius %s succeeded, want an error; stdout: %s", strings.Join(args, " "), res.Stdout)
	}
	return res.Stderr + res.Stdout
}

func wantContains(t *testing.T, what, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(got, s) {
			t.Fatalf("%s = %q, want it to contain %q", what, got, s)
		}
	}
}

func sizeOf(t *testing.T, cli *nebiuscli.CLI, diskID string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(run(t, cli, "compute", "disk", "get", "--id", diskID, "--format", "jsonpath={.status.size_bytes}"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAdditionalDisk follows "Create an additional disk", "Resize a disk",
// "Enable or disable deletion protection" and "Delete a disk".
func TestAdditionalDisk(t *testing.T) {
	cli := newCLI(t)
	id := run(t, cli, "compute", "disk", "create",
		"--name", "data-disk", "--size-gibibytes", "10", "--type", "network_ssd", "--block-size-bytes", "4096",
		"--format", "jsonpath={.metadata.id}")
	if !strings.HasPrefix(id, "computedisk-") {
		t.Fatalf("disk id = %q", id)
	}
	if got := run(t, cli, "compute", "disk", "get-by-name", "--name", "data-disk", "--format", "jsonpath={.metadata.id}"); got != id {
		t.Fatalf("get-by-name id = %q, want %q", got, id)
	}
	if got := run(t, cli, "compute", "disk", "get", "--id", id, "--format", "jsonpath={.status.state}"); got != "READY" {
		t.Fatalf("state = %q, want READY", got)
	}
	wantContains(t, "disk list", run(t, cli, "compute", "disk", "list", "--format", "json"), id, "data-disk")

	run(t, cli, "compute", "disk", "update", "--id", id, "--size-gibibytes", "20")
	if got := sizeOf(t, cli, id); got != 20*gib {
		t.Fatalf("size after resize = %d, want %d", got, 20*gib)
	}
	out := fails(t, cli, "compute", "disk", "update", "--id", id, "--size-gibibytes", "15")
	wantContains(t, "shrink error", out, "cannot be reduced")

	run(t, cli, "compute", "disk", "update", "--id", id, "--forbid-deletion=true")
	out = fails(t, cli, "compute", "disk", "delete", "--id", id)
	wantContains(t, "protected delete error", out, "forbid_deletion")
	run(t, cli, "compute", "disk", "update", "--id", id, "--forbid-deletion=false")
	run(t, cli, "compute", "disk", "delete", "--id", id)
	fails(t, cli, "compute", "disk", "get", "--id", id)
}

// TestEncryptedDisk creates an IO M3 disk with managed encryption, which the
// docs allow only for NRD and IO M3 disks.
func TestEncryptedDisk(t *testing.T) {
	cli := newCLI(t)
	id := run(t, cli, "compute", "disk", "create", "--name", "secret", "--type", "network_ssd_io_m3",
		"--size-gibibytes", "93", "--disk-encryption-type", "disk_encryption_managed", "--format", "jsonpath={.metadata.id}")
	if got := run(t, cli, "compute", "disk", "get", "--id", id, "--format", "jsonpath={.spec.disk_encryption.type}"); got != "DISK_ENCRYPTION_MANAGED" {
		t.Fatalf("encryption = %q", got)
	}
	out := fails(t, cli, "compute", "disk", "create", "--name", "plain", "--type", "network_ssd",
		"--size-gibibytes", "10", "--disk-encryption-type", "disk_encryption_managed")
	wantContains(t, "SSD encryption error", out, "DISK_ENCRYPTION_MANAGED")
	out = fails(t, cli, "compute", "disk", "create", "--name", "odd", "--type", "network_ssd_non_replicated", "--size-gibibytes", "100")
	wantContains(t, "NRD size error", out, "multiple of 93 GiB")
	run(t, cli, "compute", "disk", "delete", "--id", id)
}

// TestBootDiskSnapshotRestore creates a boot disk from a public image family,
// snapshots it, restores a disk from the snapshot and tears everything down.
func TestBootDiskSnapshotRestore(t *testing.T) {
	cli := newCLI(t)
	boot := run(t, cli, "compute", "disk", "create", "--name", "boot", "--type", "network_ssd", "--size-gibibytes", "32",
		"--source-image-family-image-family", "ubuntu24.04-driverless", "--format", "jsonpath={.metadata.id}")
	if got := run(t, cli, "compute", "disk", "get", "--id", boot, "--format", "jsonpath={.status.source_image_id}"); !strings.HasPrefix(got, "computeimage-") {
		t.Fatalf("source_image_id = %q", got)
	}
	out := fails(t, cli, "compute", "disk", "create", "--name", "tiny", "--type", "network_ssd", "--size-gibibytes", "10",
		"--source-image-family-image-family", "ubuntu24.04-driverless")
	wantContains(t, "too-small boot disk error", out, "smaller than its source")

	snap := run(t, cli, "compute", "disk-snapshot", "create", "--name", "boot-snap",
		"--description", "before upgrade", "--source-disk-id", boot, "--format", "jsonpath={.metadata.id}")
	if !strings.HasPrefix(snap, "computedisksnapshot-") {
		t.Fatalf("snapshot id = %q", snap)
	}
	if got := run(t, cli, "compute", "disk-snapshot", "get", "--id", snap, "--format", "jsonpath={.status.content_size_bytes}"); got != strconv.FormatInt(32*gib, 10) {
		t.Fatalf("content_size_bytes = %q, want %d", got, 32*gib)
	}
	wantContains(t, "snapshot list", run(t, cli, "compute", "disk-snapshot", "list", "--format", "json"), snap)
	wantContains(t, "list-by-disk", run(t, cli, "compute", "disk-snapshot", "list-by-disk", "--disk-id", boot, "--format", "json"), snap)
	out = fails(t, cli, "compute", "disk-snapshot", "create", "--name", "orphan", "--source-disk-id", "computedisk-e00doesnotexist00000")
	wantContains(t, "snapshot of a missing disk", out, "not found")

	run(t, cli, "compute", "disk-snapshot", "update", "--id", snap, "--name", "boot-snap-v1", "--description", "kernel 6.8")
	if got := run(t, cli, "compute", "disk-snapshot", "get-by-name", "--name", "boot-snap-v1", "--format", "jsonpath={.spec.description}"); got != "kernel 6.8" {
		t.Fatalf("description = %q", got)
	}

	restored := run(t, cli, "compute", "disk", "create", "--name", "restored", "--type", "network_ssd",
		"--size-gibibytes", "40", "--block-size-bytes", "4096", "--source-snapshot-id", snap, "--format", "jsonpath={.metadata.id}")
	if got := sizeOf(t, cli, restored); got != 40*gib {
		t.Fatalf("restored size = %d", got)
	}

	// Deleting the source disk keeps its snapshot (its source_disk_id goes stale).
	run(t, cli, "compute", "disk", "delete", "--id", boot)
	if got := run(t, cli, "compute", "disk-snapshot", "get", "--id", snap, "--format", "jsonpath={.status.state}"); got != "READY" {
		t.Fatalf("snapshot state after disk delete = %q", got)
	}
	run(t, cli, "compute", "disk", "delete", "--id", restored)
	run(t, cli, "compute", "disk-snapshot", "delete", "--id", snap)
	fails(t, cli, "compute", "disk-snapshot", "get", "--id", snap)
}
