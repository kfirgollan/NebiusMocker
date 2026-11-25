//go:build e2e

// Package computeinstance_test runs the VM workflows from
// https://docs.nebius.com/compute/virtual-machines/manage, .../stop-start,
// .../update, .../delete and .../types with the official nebius CLI against an
// in-process mocker. Instances are control plane only, so no Docker is needed.
package computeinstance_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type env struct {
	inst *testkit.Instance
	cli  *nebiuscli.CLI
}

func newEnv(t *testing.T) env {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), vpc.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(50*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	return env{inst: inst, cli: cli}
}

func run(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	res, err := cli.Run(t.Context(), args...)
	if err != nil {
		t.Fatalf("nebius %s: %v\nstderr: %s", strings.Join(args, " "), err, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

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

func idOf(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	return run(t, cli, append(args, "--format", "jsonpath={.metadata.id}")...)
}

type address struct {
	Address string `json:"address"`
}

type instance struct {
	Spec struct {
		Resources struct {
			Platform string `json:"platform"`
			Preset   string `json:"preset"`
		} `json:"resources"`
	} `json:"spec"`
	Status struct {
		State             string `json:"state"`
		NetworkInterfaces []struct {
			IPAddress       address `json:"ip_address"`
			PublicIPAddress address `json:"public_ip_address"`
		} `json:"network_interfaces"`
	} `json:"status"`
}

func getInstance(t *testing.T, cli *nebiuscli.CLI, id string) instance {
	t.Helper()
	var in instance
	cli.JSON(t, &in, "compute", "instance", "get", "--id", id)
	return in
}

func subnetID(t *testing.T, cli *nebiuscli.CLI) string {
	t.Helper()
	return idOf(t, cli, "vpc", "subnet", "get-by-name", "--name", "default-subnet")
}

func nics(t *testing.T, subnet string) string {
	t.Helper()
	b, err := json.Marshal([]map[string]any{{"name": "eth0", "subnet_id": subnet, "ip_address": map[string]any{}, "public_ip_address": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPlatforms follows docs.nebius.com/compute/virtual-machines/types.
func TestPlatforms(t *testing.T) {
	e := newEnv(t)
	out := run(t, e.cli, "compute", "platform", "list", "--parent-id", e.inst.ProjectID, "--format", "json")
	wantContains(t, "platform list", out, "cpu-e2", "gpu-h100-sxm", "2vcpu-8gb", "8gpu-128vcpu-1600gb")
	out = run(t, e.cli, "compute", "platform", "get-by-name", "--parent-id", e.inst.ProjectID, "--name", "gpu-h100-sxm", "--format", "json")
	wantContains(t, "platform get-by-name", out, "gpu-h100-sxm", "1gpu-16vcpu-200gb", "8gpu-128vcpu-1600gb")
}

// TestVMLifecycle follows manage, stop-start, update and delete: an existing
// boot disk, create, get, list, stop, preset update, start, delete.
func TestVMLifecycle(t *testing.T) {
	e := newEnv(t)
	cli := e.cli
	subnet := subnetID(t, cli)
	disk := idOf(t, cli, "compute", "disk", "create", "--name", "vm-boot", "--type", "network_ssd",
		"--size-gibibytes", "32", "--source-image-family-image-family", "ubuntu24.04-driverless")

	id := idOf(t, cli, "compute", "instance", "create", "--name", "vm1",
		"--resources-platform", "cpu-e2", "--resources-preset", "2vcpu-8gb",
		"--boot-disk-existing-disk-id", disk, "--boot-disk-attach-mode", "READ_WRITE",
		"--network-interfaces", nics(t, subnet))
	if !strings.HasPrefix(id, "computeinstance-") {
		t.Fatalf("instance id = %q", id)
	}

	in := getInstance(t, cli, id)
	if in.Status.State != "RUNNING" || in.Spec.Resources.Preset != "2vcpu-8gb" {
		t.Fatalf("instance = %+v, want RUNNING 2vcpu-8gb", in)
	}
	if len(in.Status.NetworkInterfaces) != 1 || in.Status.NetworkInterfaces[0].IPAddress.Address == "" || in.Status.NetworkInterfaces[0].PublicIPAddress.Address == "" {
		t.Fatalf("network interfaces = %+v, want private and public addresses", in.Status.NetworkInterfaces)
	}
	publicAddr := in.Status.NetworkInterfaces[0].PublicIPAddress.Address
	if got := idOf(t, cli, "compute", "instance", "get-by-name", "--name", "vm1"); got != id {
		t.Fatalf("get-by-name = %q, want %q", got, id)
	}
	wantContains(t, "instance list", run(t, cli, "compute", "instance", "list", "--format", "json"), id, "vm1")
	wantContains(t, "attached disk", run(t, cli, "compute", "disk", "get", "--id", disk, "--format", "json"), id)
	fails(t, cli, "compute", "disk", "delete", "--id", disk)

	out := fails(t, cli, "compute", "instance", "start", "--id", id)
	wantContains(t, "start running error", out, "RUNNING")

	run(t, cli, "compute", "instance", "stop", "--id", id)
	if got := getInstance(t, cli, id).Status.State; got != "STOPPED" {
		t.Fatalf("state after stop = %q", got)
	}
	out = fails(t, cli, "compute", "instance", "stop", "--id", id)
	wantContains(t, "stop stopped error", out, "STOPPED")

	run(t, cli, "compute", "instance", "update", "--id", id, "--parent-id", e.inst.ProjectID, "--resources-preset", "4vcpu-16gb")
	if got := getInstance(t, cli, id).Spec.Resources.Preset; got != "4vcpu-16gb" {
		t.Fatalf("preset after update = %q", got)
	}
	run(t, cli, "compute", "instance", "start", "--id", id)
	in = getInstance(t, cli, id)
	if in.Status.State != "RUNNING" || in.Spec.Resources.Preset != "4vcpu-16gb" {
		t.Fatalf("instance after start = %+v", in)
	}
	// A start within the hour keeps the dynamic public address
	// (docs compute/virtual-machines/network).
	if got, want := in.Status.NetworkInterfaces[0].PublicIPAddress.Address, publicAddr; got != want {
		t.Fatalf("public address after stop/start = %q, want %q kept", got, want)
	}
	out = fails(t, cli, "compute", "instance", "update", "--id", id, "--resources-preset", "2vcpu-8gb")
	wantContains(t, "running preset change error", out, "STOPPED")

	run(t, cli, "compute", "instance", "delete", "--id", id)
	fails(t, cli, "compute", "instance", "get", "--id", id)
	out = run(t, cli, "compute", "disk", "get", "--id", disk, "--format", "json")
	if strings.Contains(out, id) {
		t.Fatalf("disk still references deleted instance: %s", out)
	}
	run(t, cli, "compute", "disk", "delete", "--id", disk)
}

// TestManagedBootDiskGPU creates a GPU VM with a managed boot disk, as in the
// manage page, and deletes it together with the disk.
func TestManagedBootDiskGPU(t *testing.T) {
	e := newEnv(t)
	cli := e.cli
	subnet := subnetID(t, cli)
	id := idOf(t, cli, "compute", "instance", "create", "--name", "gpu-vm",
		"--resources-platform", "gpu-h100-sxm", "--resources-preset", "8gpu-128vcpu-1600gb",
		"--boot-disk-managed-disk-name", "gpu-boot", "--boot-disk-managed-disk-type", "network_ssd",
		"--boot-disk-managed-disk-size-gibibytes", "100",
		"--boot-disk-managed-disk-source-image-family-image-family", "ubuntu24.04-cuda12",
		"--boot-disk-attach-mode", "READ_WRITE", "--boot-disk-device-id", "boot",
		"--network-interfaces", nics(t, subnet))
	if got := getInstance(t, cli, id); got.Status.State != "RUNNING" || got.Spec.Resources.Platform != "gpu-h100-sxm" {
		t.Fatalf("gpu instance = %+v", got)
	}
	wantContains(t, "managed disk", run(t, cli, "compute", "disk", "list", "--format", "json"), "gpu-boot")
	out := fails(t, cli, "compute", "instance", "create", "--name", "bad",
		"--resources-platform", "cpu-e2", "--resources-preset", "nope",
		"--boot-disk-managed-disk-name", "b", "--boot-disk-managed-disk-type", "network_ssd",
		"--boot-disk-managed-disk-size-gibibytes", "20", "--boot-disk-attach-mode", "READ_WRITE",
		"--network-interfaces", nics(t, subnet))
	wantContains(t, "bad preset error", out, "preset")
	run(t, cli, "compute", "instance", "delete", "--id", id)
	if out := run(t, cli, "compute", "disk", "list", "--format", "json"); strings.Contains(out, "gpu-boot") {
		t.Fatalf("managed boot disk survived instance delete: %s", out)
	}
}

// TestViewerCannotCreate checks authorization with the seeded viewer token.
func TestViewerCannotCreate(t *testing.T) {
	e := newEnv(t)
	subnet := subnetID(t, e.cli)
	viewer := nebiuscli.New(t, nebiuscli.Profile{Addr: e.inst.Addr, Token: config.DefaultViewerToken, ParentID: e.inst.ProjectID, TenantID: e.inst.TenantID})
	out := fails(t, viewer, "compute", "instance", "create", "--name", "nope",
		"--resources-platform", "cpu-e2", "--resources-preset", "2vcpu-8gb",
		"--boot-disk-managed-disk-name", "b", "--boot-disk-managed-disk-type", "network_ssd",
		"--boot-disk-managed-disk-size-gibibytes", "20", "--boot-disk-attach-mode", "READ_WRITE",
		"--network-interfaces", nics(t, subnet))
	wantContains(t, "viewer create error", strings.ToLower(out), "permission")
	run(t, viewer, "compute", "instance", "list")
}
