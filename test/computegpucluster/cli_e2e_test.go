//go:build e2e

// Package computegpucluster_test runs the GPU cluster workflows from
// https://docs.nebius.com/compute/clusters/gpu with the official nebius CLI
// against an in-process mocker. GPU clusters are control plane only, so no
// Docker is needed.
package computegpucluster_test

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
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

// cluster is the part of a GpuCluster the CLI prints that these tests check.
type cluster struct {
	Metadata struct {
		ID              string            `json:"id"`
		ParentID        string            `json:"parent_id"`
		Name            string            `json:"name"`
		Labels          map[string]string `json:"labels"`
		ResourceVersion string            `json:"resource_version"`
	} `json:"metadata"`
	Spec struct {
		InfinibandFabric string `json:"infiniband_fabric"`
	} `json:"spec"`
	Status struct {
		Reconciling bool     `json:"reconciling"`
		Instances   []string `json:"instances"`
	} `json:"status"`
}

type clusterList struct {
	Items []cluster `json:"items"`
}

func newCLI(t *testing.T) (cli *nebiuscli.CLI, projectID string) {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New()},
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

func getCluster(t *testing.T, cli *nebiuscli.CLI, args ...string) cluster {
	t.Helper()
	return decode[cluster](t, run(t, cli, append([]string{"compute", "gpu-cluster"}, append(args, "--format", "json")...)...))
}

func wantContains(t *testing.T, what, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(got, s) {
			t.Fatalf("%s = %q, want it to contain %q", what, got, s)
		}
	}
}

// TestGpuClusterLifecycle follows "Create a cluster" and "Delete a cluster"
// of the GPU clusters docs, plus get, list and update.
func TestGpuClusterLifecycle(t *testing.T) {
	cli, project := newCLI(t)

	created := getCluster(t, cli, "create", "--name", "gpu-cluster-name", "--infiniband-fabric", "fabric-3")
	id := created.Metadata.ID
	if !strings.HasPrefix(id, "computegpucluster-") || created.Metadata.Name != "gpu-cluster-name" ||
		created.Metadata.ParentID != project || created.Spec.InfinibandFabric != "fabric-3" {
		t.Fatalf("created = %+v", created)
	}

	got := getCluster(t, cli, "get", "--id", id)
	if got.Metadata.ID != id || got.Spec.InfinibandFabric != "fabric-3" || got.Status.Reconciling || len(got.Status.Instances) != 0 {
		t.Fatalf("get = %+v", got)
	}
	if byName := getCluster(t, cli, "get-by-name", "--name", "gpu-cluster-name"); byName.Metadata.ID != id {
		t.Fatalf("get-by-name id = %q, want %q", byName.Metadata.ID, id)
	}
	list := decode[clusterList](t, run(t, cli, "compute", "gpu-cluster", "list", "--format", "json"))
	if len(list.Items) != 1 || list.Items[0].Metadata.ID != id {
		t.Fatalf("list = %+v", list)
	}

	updated := getCluster(t, cli, "update", "--id", id, "--name", "renamed", "--labels", "env=dev")
	if updated.Metadata.Name != "renamed" || updated.Metadata.Labels["env"] != "dev" || updated.Spec.InfinibandFabric != "fabric-3" ||
		updated.Metadata.ResourceVersion == created.Metadata.ResourceVersion {
		t.Fatalf("updated = %+v (created version %s)", updated, created.Metadata.ResourceVersion)
	}
	if byName := getCluster(t, cli, "get-by-name", "--name", "renamed"); byName.Metadata.ID != id {
		t.Fatalf("get-by-name after rename = %+v", byName)
	}
	fails(t, cli, "compute", "gpu-cluster", "get-by-name", "--name", "gpu-cluster-name")

	run(t, cli, "compute", "gpu-cluster", "delete", "--id", id)
	wantContains(t, "get after delete", fails(t, cli, "compute", "gpu-cluster", "get", "--id", id), "not found")
	if list := decode[clusterList](t, run(t, cli, "compute", "gpu-cluster", "list", "--format", "json")); len(list.Items) != 0 {
		t.Fatalf("list after delete = %+v", list)
	}
}

// TestGpuClusterInvalidFabric checks the errors for a missing, unknown and
// foreign-region InfiniBand fabric.
func TestGpuClusterInvalidFabric(t *testing.T) {
	cli, _ := newCLI(t)
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown", []string{"--infiniband-fabric", "fabric-99"}, "unknown fabric"},
		{"other region", []string{"--infiniband-fabric", "fabric-5"}, "eu-west1"},
		{"missing", nil, "required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := fails(t, cli, append([]string{"compute", "gpu-cluster", "create", "--name", "bad-" + tt.name}, tt.args...)...)
			wantContains(t, "error", out, "infiniband_fabric", tt.want)
		})
	}
	if list := decode[clusterList](t, run(t, cli, "compute", "gpu-cluster", "list", "--format", "json")); len(list.Items) != 0 {
		t.Fatalf("a failed create left clusters behind: %+v", list)
	}
}
