//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
)

const gpuAddr = "nebius_compute_v1_gpu_cluster."

func gpuClusterCfg(parent, name, fabric string, labels map[string]string) string {
	return fmt.Sprintf(`
resource "nebius_compute_v1_gpu_cluster" "main" {
  parent_id         = %q
  name              = %q
  infiniband_fabric = %q
%s}
`, parent, name, fabric, hclLabels(labels))
}

func gpuClusterGet(t *testing.T, c computesvc.GpuClusterService, id string) *computepb.GpuCluster {
	t.Helper()
	g, err := c.Get(t.Context(), &computepb.GetGpuClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get gpu cluster %s: %v", id, err)
	}
	return g
}

func assertGpuClusterGone(t *testing.T, c computesvc.GpuClusterService, id string) {
	t.Helper()
	_, err := c.Get(t.Context(), &computepb.GetGpuClusterRequest{Id: id})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("gpu cluster %s: SDK Get code = %v (err %v), want NotFound", id, got, err)
	}
}

// checkGpuCluster compares Terraform's view (state attributes or data source
// output) with the mock's, read with the SDK.
func checkGpuCluster(t *testing.T, tfAttrs map[string]any, want *computepb.GpuCluster) {
	t.Helper()
	md := want.GetMetadata()
	checks := map[string][2]string{
		"id":                {str(tfAttrs, "id"), md.GetId()},
		"name":              {str(tfAttrs, "name"), md.GetName()},
		"parent_id":         {str(tfAttrs, "parent_id"), md.GetParentId()},
		"infiniband_fabric": {str(tfAttrs, "infiniband_fabric"), want.GetSpec().GetInfinibandFabric()},
		"resource_version":  {str(tfAttrs, "resource_version"), strconv.FormatInt(md.GetResourceVersion(), 10)},
		// the provider renders a false (zero) bool as unset
		"status.reconciling": {strconv.FormatBool(str(tfAttrs, "status", "reconciling") == "true"), strconv.FormatBool(want.GetStatus().GetReconciling())},
	}
	for _, field := range slices.Sorted(maps.Keys(checks)) {
		c := checks[field]
		if c[0] != c[1] {
			t.Errorf("gpu cluster %s: terraform %s = %q, mock has %q", md.GetId(), field, c[0], c[1])
		}
	}
	if got := labelsOf(tfAttrs); !maps.Equal(got, md.GetLabels()) && (len(got) > 0 || len(md.GetLabels()) > 0) {
		t.Errorf("gpu cluster %s: terraform labels = %v, mock has %v", md.GetId(), got, md.GetLabels())
	}
	if st, _ := tfAttrs["status"].(map[string]any); st != nil {
		if l, _ := st["instances"].([]any); len(l) != len(want.GetStatus().GetInstances()) {
			t.Errorf("gpu cluster %s: terraform instances = %v, mock has %v", md.GetId(), l, want.GetStatus().GetInstances())
		}
	}
}

// TestGpuClusterLifecycle: create, in-place rename/relabel, replace on fabric
// change, data sources, destroy; every step is followed by an empty plan.
func TestGpuClusterLifecycle(t *testing.T) {
	env := startDiskEnv(t)
	pid := env.Inst.ProjectID
	clusters := computesvc.NewGpuClusterService(env.Inst.SDK(t))
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)

	tf.writeConfig(t, gpuClusterCfg(pid, "tf-gpu", "fabric-3", map[string]string{"env": "dev"}))
	tf.init(t)
	tf.step(t, map[string]string{"main": "create"})
	id := tf.stateID(t, gpuAddr+"main")
	g := gpuClusterGet(t, clusters, id)
	checkGpuCluster(t, tf.stateOf(t, gpuAddr+"main"), g)
	if g.GetMetadata().GetName() != "tf-gpu" || g.GetSpec().GetInfinibandFabric() != "fabric-3" || g.GetMetadata().GetLabels()["env"] != "dev" ||
		g.GetStatus().GetReconciling() || len(g.GetStatus().GetInstances()) != 0 {
		t.Fatalf("created gpu cluster = %v", g)
	}
	version := g.GetMetadata().GetResourceVersion()

	// In-place update: name and labels; same id, bumped resource version.
	tf.writeConfig(t, gpuClusterCfg(pid, "tf-gpu-renamed", "fabric-3", map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "update"})
	if got := tf.stateID(t, gpuAddr+"main"); got != id {
		t.Fatalf("in-place update changed the id: %s -> %s", id, got)
	}
	g = gpuClusterGet(t, clusters, id)
	checkGpuCluster(t, tf.stateOf(t, gpuAddr+"main"), g)
	if g.GetMetadata().GetName() != "tf-gpu-renamed" || g.GetMetadata().GetLabels()["env"] != "prod" || g.GetMetadata().GetLabels()["team"] != "ml" ||
		g.GetMetadata().GetResourceVersion() <= version {
		t.Fatalf("updated gpu cluster = %v (version before %d)", g, version)
	}

	// Fabric change: replace with a new id; the old cluster is gone.
	tf.writeConfig(t, gpuClusterCfg(pid, "tf-gpu-renamed", "fabric-4", map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "replace"})
	newID := tf.stateID(t, gpuAddr+"main")
	if newID == id {
		t.Fatal("fabric change did not replace the gpu cluster")
	}
	assertGpuClusterGone(t, clusters, id)
	g = gpuClusterGet(t, clusters, newID)
	checkGpuCluster(t, tf.stateOf(t, gpuAddr+"main"), g)
	if g.GetSpec().GetInfinibandFabric() != "fabric-4" {
		t.Fatalf("replaced gpu cluster = %v", g)
	}

	// Data sources by id and by parent_id + name.
	tf.writeConfig(t, gpuClusterCfg(pid, "tf-gpu-renamed", "fabric-4", map[string]string{"env": "prod", "team": "ml"})+fmt.Sprintf(`
data "nebius_compute_v1_gpu_cluster" "by_id" { id = nebius_compute_v1_gpu_cluster.main.id }
data "nebius_compute_v1_gpu_cluster" "by_name" {
  parent_id  = %q
  name       = "tf-gpu-renamed"
  depends_on = [nebius_compute_v1_gpu_cluster.main]
}
output "by_id"   { value = data.nebius_compute_v1_gpu_cluster.by_id }
output "by_name" { value = data.nebius_compute_v1_gpu_cluster.by_name }
`, pid))
	tf.apply(t)
	tf.requireEmptyPlan(t)
	out := tf.outputs(t)
	for _, name := range []string{"by_id", "by_name"} {
		checkGpuCluster(t, out[name], g)
		if got := str(out[name], "infiniband_fabric"); got != "fabric-4" {
			t.Errorf("%s: infiniband_fabric = %q, want fabric-4", name, got)
		}
	}
	if a, b := str(out["by_id"], "id"), str(tf.stateOf(t, gpuAddr+"main"), "id"); a != b {
		t.Errorf("data source id %q vs resource id %q", a, b)
	}

	tf.destroy(t)
	assertGpuClusterGone(t, clusters, newID)
}

// TestGpuClusterBadFabric: a fabric of another region is rejected. The provider
// prints only the status message, not the field violation, so the message
// itself must name the fabric and the regions.
func TestGpuClusterBadFabric(t *testing.T) {
	env := startDiskEnv(t)
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)
	tf.writeConfig(t, gpuClusterCfg(env.Inst.ProjectID, "tf-gpu-bad", "fabric-5", nil))
	tf.init(t)
	err := tf.tf.Apply(t.Context())
	if err == nil {
		t.Fatal("terraform apply with a fabric of another region succeeded")
	}
	if msg := err.Error() + tf.out.String(); !strings.Contains(msg, "spec.infiniband_fabric: fabric fabric-5 is in region eu-west1") {
		t.Fatalf("error does not name the fabric and its region:\n%s", msg)
	}
}
