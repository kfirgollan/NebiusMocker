//go:build e2e

// Package computenvl_test runs the NVLink instance group workflows with the
// official nebius CLI against an in-process mocker. NVL instance groups are
// control plane only, so no Docker is needed.
package computenvl_test

import (
	"encoding/json"
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
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

// group is the part of an NVLInstanceGroup the CLI prints that these tests check.
type group struct {
	Metadata struct {
		ID              string            `json:"id"`
		ParentID        string            `json:"parent_id"`
		Name            string            `json:"name"`
		Labels          map[string]string `json:"labels"`
		ResourceVersion string            `json:"resource_version"`
	} `json:"metadata"`
	Spec struct {
		Type string `json:"type"`
		Size string `json:"size"`
	} `json:"spec"`
	Status struct {
		Reconciling bool `json:"reconciling"`
		Instances   map[string]struct {
			InstanceState string `json:"instance_state"`
		} `json:"instances"`
	} `json:"status"`
}

type groupList struct {
	Items []group `json:"items"`
}

type instanceList struct {
	Items []struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	} `json:"items"`
}

func newCLI(t *testing.T) (cli *nebiuscli.CLI, projectID string) {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), vpc.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(50*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	return nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID}), inst.ProjectID
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

func decode[T any](t *testing.T, out string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return v
}

func getGroup(t *testing.T, cli *nebiuscli.CLI, args ...string) group {
	t.Helper()
	return decode[group](t, run(t, cli, append([]string{"compute", "nvl-instance-group"}, append(args, "--format", "json")...)...))
}

func wantContains(t *testing.T, what, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(got, s) {
			t.Fatalf("%s = %q, want it to contain %q", what, got, s)
		}
	}
}

// TestNVLInstanceGroupLifecycle covers create, get, get-by-name, list, update
// and delete.
func TestNVLInstanceGroupLifecycle(t *testing.T) {
	cli, project := newCLI(t)

	created := getGroup(t, cli, "create", "--name", "nvl-a", "--type", "GB200", "--size", "2")
	id := created.Metadata.ID
	if id == "" || created.Metadata.Name != "nvl-a" || created.Metadata.ParentID != project ||
		created.Spec.Type != "GB200" || created.Spec.Size != "2" {
		t.Fatalf("created = %+v", created)
	}

	got := getGroup(t, cli, "get", "--id", id)
	if got.Metadata.ID != id || got.Spec.Type != "GB200" || got.Status.Reconciling || len(got.Status.Instances) != 0 {
		t.Fatalf("get = %+v", got)
	}
	if byName := getGroup(t, cli, "get-by-name", "--name", "nvl-a"); byName.Metadata.ID != id {
		t.Fatalf("get-by-name id = %q, want %q", byName.Metadata.ID, id)
	}
	list := decode[groupList](t, run(t, cli, "compute", "nvl-instance-group", "list", "--format", "json"))
	if len(list.Items) != 1 || list.Items[0].Metadata.ID != id {
		t.Fatalf("list = %+v", list)
	}

	updated := getGroup(t, cli, "update", "--id", id, "--name", "nvl-renamed", "--labels", "env=dev")
	if updated.Metadata.Name != "nvl-renamed" || updated.Metadata.Labels["env"] != "dev" || updated.Spec.Size != "2" ||
		updated.Metadata.ResourceVersion == created.Metadata.ResourceVersion {
		t.Fatalf("updated = %+v (created version %s)", updated, created.Metadata.ResourceVersion)
	}
	if byName := getGroup(t, cli, "get-by-name", "--name", "nvl-renamed"); byName.Metadata.ID != id {
		t.Fatalf("get-by-name after rename = %+v", byName)
	}
	fails(t, cli, "compute", "nvl-instance-group", "get-by-name", "--name", "nvl-a")

	run(t, cli, "compute", "nvl-instance-group", "delete", "--id", id)
	wantContains(t, "get after delete", fails(t, cli, "compute", "nvl-instance-group", "get", "--id", id), "not found")
	if list := decode[groupList](t, run(t, cli, "compute", "nvl-instance-group", "list", "--format", "json")); len(list.Items) != 0 {
		t.Fatalf("list after delete = %+v", list)
	}
}

// TestNVLInstanceGroupMembers creates an instance in the group with the CLI,
// lists the group's instances and checks a non-empty group cannot be deleted.
func TestNVLInstanceGroupMembers(t *testing.T) {
	cli, _ := newCLI(t)
	subnet := run(t, cli, "vpc", "subnet", "get-by-name", "--name", "default-subnet", "--format", "jsonpath={.metadata.id}")
	nics, err := json.Marshal([]map[string]any{{"name": "eth0", "subnet_id": subnet, "ip_address": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}

	gid := run(t, cli, "compute", "nvl-instance-group", "create", "--name", "nvl-b", "--type", "GB200", "--size", "2",
		"--format", "jsonpath={.metadata.id}")
	vm := run(t, cli, "compute", "instance", "create", "--name", "nvl-vm",
		"--resources-platform", "gpu-gb200", "--resources-preset", "4gpu-128vcpu-1600gb",
		"--nvl-instance-group-id", gid, "--recovery-policy", "ALWAYS",
		"--boot-disk-managed-disk-name", "nvl-boot", "--boot-disk-managed-disk-type", "network_ssd",
		"--boot-disk-managed-disk-size-gibibytes", "50",
		"--boot-disk-managed-disk-source-image-family-image-family", "ubuntu24.04-cuda12",
		"--boot-disk-attach-mode", "READ_WRITE", "--boot-disk-device-id", "boot",
		"--network-interfaces", string(nics), "--format", "jsonpath={.metadata.id}")

	members := decode[instanceList](t, run(t, cli, "compute", "instance", "list-instances-by-nvl-instance-group",
		"--nvl-instance-group-id", gid, "--format", "json"))
	if len(members.Items) != 1 || members.Items[0].Metadata.ID != vm {
		t.Fatalf("list-instances-by-nvl-instance-group = %+v, want [%s]", members, vm)
	}
	g := getGroup(t, cli, "get", "--id", gid)
	if g.Status.Instances[vm].InstanceState != "RUNNING" {
		t.Fatalf("group status.instances = %+v, want %s RUNNING", g.Status.Instances, vm)
	}

	out := fails(t, cli, "compute", "nvl-instance-group", "delete", "--id", gid)
	wantContains(t, "delete non-empty group error", out, vm)

	run(t, cli, "compute", "instance", "delete", "--id", vm)
	run(t, cli, "compute", "nvl-instance-group", "delete", "--id", gid)
}
