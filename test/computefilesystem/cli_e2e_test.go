//go:build e2e

// Package computefilesystem_test runs the shared filesystem workflow from
// https://docs.nebius.com/compute/storage/manage with the official nebius CLI
// against an in-process mocker. Filesystems are control plane only, so no
// Docker is needed.
package computefilesystem_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

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

func start(t *testing.T) (*nebiuscli.CLI, computesvc.FilesystemService) {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(50*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	return cli, computesvc.NewFilesystemService(inst.SDK(t))
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

// TestSharedFilesystem follows "Create a shared filesystem", "Resize a
// filesystem", "Enable or disable deletion protection" and "Delete a
// filesystem"; every step is verified with the CLI's JSON and the Go SDK.
func TestSharedFilesystem(t *testing.T) {
	cli, sdk := start(t)
	get := func(id string) *computepb.Filesystem {
		t.Helper()
		f, err := sdk.Get(t.Context(), &computepb.GetFilesystemRequest{Id: id})
		if err != nil {
			t.Fatalf("SDK Get filesystem %s: %v", id, err)
		}
		return f
	}

	id := run(t, cli, "compute", "filesystem", "create",
		"--name", "shared-fs", "--type", "network_ssd", "--size-gibibytes", "1", "--block-size-bytes", "4096",
		"--format", "jsonpath={.metadata.id}")
	if !strings.HasPrefix(id, "computefilesystem-") {
		t.Fatalf("filesystem id = %q", id)
	}
	f := get(id)
	if f.GetMetadata().GetName() != "shared-fs" || f.GetSpec().GetType() != computepb.FilesystemSpec_NETWORK_SSD ||
		f.GetSpec().GetBlockSizeBytes() != 4096 || f.GetStatus().GetSizeBytes() != gib || f.GetStatus().GetState() != computepb.FilesystemStatus_READY {
		t.Fatalf("created filesystem = %v", f)
	}

	var viaCLI struct {
		Metadata struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Type string `json:"type"`
		} `json:"spec"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(run(t, cli, "compute", "filesystem", "get", "--id", id, "--format", "json")), &viaCLI); err != nil {
		t.Fatal(err)
	}
	if viaCLI.Metadata.ID != id || viaCLI.Metadata.Name != "shared-fs" || viaCLI.Spec.Type != "NETWORK_SSD" || viaCLI.Status.State != "READY" {
		t.Fatalf("CLI get = %+v", viaCLI)
	}
	if got := run(t, cli, "compute", "filesystem", "get-by-name", "--name", "shared-fs", "--format", "jsonpath={.metadata.id}"); got != id {
		t.Fatalf("get-by-name id = %q, want %q", got, id)
	}
	wantContains(t, "filesystem list", run(t, cli, "compute", "filesystem", "list", "--format", "json"), id, "shared-fs")

	// Hot grow; shrinking is refused.
	run(t, cli, "compute", "filesystem", "update", "--id", id, "--size-gibibytes", "2")
	if got := get(id).GetStatus().GetSizeBytes(); got != 2*gib {
		t.Fatalf("size after grow = %d, want %d", got, 2*gib)
	}
	if got := run(t, cli, "compute", "filesystem", "get", "--id", id, "--format", "jsonpath={.status.size_bytes}"); got != "2147483648" {
		t.Fatalf("CLI size_bytes = %q", got)
	}
	out := fails(t, cli, "compute", "filesystem", "update", "--id", id, "--size-gibibytes", "1")
	wantContains(t, "shrink error", out, "cannot be reduced")
	if got := get(id).GetStatus().GetSizeBytes(); got != 2*gib {
		t.Fatalf("size after refused shrink = %d", got)
	}

	// Deletion protection.
	run(t, cli, "compute", "filesystem", "update", "--id", id, "--forbid-deletion=true")
	if !get(id).GetSpec().GetForbidDeletion() {
		t.Fatal("forbid_deletion was not set")
	}
	out = fails(t, cli, "compute", "filesystem", "delete", "--id", id)
	wantContains(t, "protected delete error", out, "forbid_deletion")
	get(id)
	run(t, cli, "compute", "filesystem", "update", "--id", id, "--forbid-deletion=false")
	if get(id).GetSpec().GetForbidDeletion() {
		t.Fatal("forbid_deletion was not cleared")
	}

	run(t, cli, "compute", "filesystem", "delete", "--id", id)
	out = fails(t, cli, "compute", "filesystem", "get", "--id", id)
	wantContains(t, "get after delete", out, "not found")
	if _, err := sdk.Get(t.Context(), &computepb.GetFilesystemRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("SDK Get after delete: %v, want NotFound", err)
	}
}

// TestFilesystemTypeAndSizeValidation: only NETWORK_SSD is available and the
// block size limits are enforced.
func TestFilesystemTypeAndSizeValidation(t *testing.T) {
	cli, sdk := start(t)
	out := fails(t, cli, "compute", "filesystem", "create", "--name", "hdd", "--type", "network_hdd", "--size-gibibytes", "1", "--block-size-bytes", "4096")
	wantContains(t, "unavailable type error", out, "not available")
	out = fails(t, cli, "compute", "filesystem", "create", "--name", "odd-block", "--type", "network_ssd", "--size-gibibytes", "1", "--block-size-bytes", "5000")
	wantContains(t, "block size error", out, "block_size_bytes")
	l, err := sdk.List(t.Context(), &computepb.ListFilesystemsRequest{ParentId: "project-e00mock000000000default"})
	if err != nil {
		t.Fatalf("SDK List filesystems: %v", err)
	}
	if n := len(l.GetItems()); n != 0 {
		t.Fatalf("%d filesystems exist after refused creates, want 0", n)
	}
}
