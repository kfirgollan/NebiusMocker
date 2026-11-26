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

const nvlAddr = "nebius_compute_v1_nvl_instance_group."

func nvlGroupCfg(parent, name, typ string, size int, labels map[string]string) string {
	return fmt.Sprintf(`
resource "nebius_compute_v1_nvl_instance_group" "main" {
  parent_id = %q
  name      = %q
  type      = %q
  size      = %d
%s}
`, parent, name, typ, size, hclLabels(labels))
}

func nvlGroupGet(t *testing.T, c computesvc.NVLInstanceGroupService, id string) *computepb.NVLInstanceGroup {
	t.Helper()
	g, err := c.Get(t.Context(), &computepb.GetNVLInstanceGroupRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get nvl instance group %s: %v", id, err)
	}
	return g
}

func assertNVLGroupGone(t *testing.T, c computesvc.NVLInstanceGroupService, id string) {
	t.Helper()
	_, err := c.Get(t.Context(), &computepb.GetNVLInstanceGroupRequest{Id: id})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("nvl instance group %s: SDK Get code = %v (err %v), want NotFound", id, got, err)
	}
}

// checkNVLGroup compares Terraform's view (state attributes or data source
// output) with the mock's, read with the SDK.
func checkNVLGroup(t *testing.T, tfAttrs map[string]any, want *computepb.NVLInstanceGroup) {
	t.Helper()
	md := want.GetMetadata()
	checks := map[string][2]string{
		"id":        {str(tfAttrs, "id"), md.GetId()},
		"name":      {str(tfAttrs, "name"), md.GetName()},
		"parent_id": {str(tfAttrs, "parent_id"), md.GetParentId()},
		"type":      {str(tfAttrs, "type"), want.GetSpec().GetType().String()},
		"size":      {str(tfAttrs, "size"), strconv.FormatInt(want.GetSpec().GetSize(), 10)},
		// the provider renders a false (zero) bool as unset
		"status.reconciling": {strconv.FormatBool(str(tfAttrs, "status", "reconciling") == "true"), strconv.FormatBool(want.GetStatus().GetReconciling())},
	}
	for _, field := range slices.Sorted(maps.Keys(checks)) {
		c := checks[field]
		if c[0] != c[1] {
			t.Errorf("nvl group %s: terraform %s = %q, mock has %q", md.GetId(), field, c[0], c[1])
		}
	}
	if got := labelsOf(tfAttrs); !maps.Equal(got, md.GetLabels()) && (len(got) > 0 || len(md.GetLabels()) > 0) {
		t.Errorf("nvl group %s: terraform labels = %v, mock has %v", md.GetId(), got, md.GetLabels())
	}
	if st, _ := tfAttrs["status"].(map[string]any); st != nil {
		tfInst, _ := st["instances"].(map[string]any)
		if len(tfInst) != len(want.GetStatus().GetInstances()) {
			t.Errorf("nvl group %s: terraform status.instances = %v, mock has %v", md.GetId(), tfInst, want.GetStatus().GetInstances())
		}
		for id, m := range want.GetStatus().GetInstances() {
			if got := at(tfInst, id, "instance_state"); got != m.GetInstanceState().String() {
				t.Errorf("nvl group %s: terraform status.instances[%s].instance_state = %q, mock has %v", md.GetId(), id, got, m.GetInstanceState())
			}
		}
	}
}

// TestNVLInstanceGroupLifecycle: every user attribute of the resource forces
// replacement in the provider, so "update" is a replacement; every step is
// followed by an empty plan.
func TestNVLInstanceGroupLifecycle(t *testing.T) {
	env := startDiskEnv(t)
	pid := env.Inst.ProjectID
	groups := computesvc.NewNVLInstanceGroupService(env.Inst.SDK(t))
	tf := newTerraform(t, env.Inst.Addr, env.Inst.Token)

	tf.writeConfig(t, nvlGroupCfg(pid, "tf-nvl", "GB200", 4, map[string]string{"env": "dev"}))
	tf.init(t)
	tf.step(t, map[string]string{"main": "create"})
	id := tf.stateID(t, nvlAddr+"main")
	g := nvlGroupGet(t, groups, id)
	checkNVLGroup(t, tf.stateOf(t, nvlAddr+"main"), g)
	if g.GetMetadata().GetName() != "tf-nvl" || g.GetSpec().GetType() != computepb.NVLInstanceGroupSpec_GB200 || g.GetSpec().GetSize() != 4 ||
		g.GetMetadata().GetLabels()["env"] != "dev" || g.GetStatus().GetReconciling() || len(g.GetStatus().GetInstances()) != 0 {
		t.Fatalf("created nvl group = %v", g)
	}

	// Labels force replacement: new id, old group gone.
	tf.writeConfig(t, nvlGroupCfg(pid, "tf-nvl", "GB200", 4, map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "replace"})
	labelID := tf.stateID(t, nvlAddr+"main")
	if labelID == id {
		t.Fatal("label change did not replace the nvl group")
	}
	assertNVLGroupGone(t, groups, id)
	g = nvlGroupGet(t, groups, labelID)
	checkNVLGroup(t, tf.stateOf(t, nvlAddr+"main"), g)
	if g.GetMetadata().GetLabels()["env"] != "prod" || g.GetMetadata().GetLabels()["team"] != "ml" {
		t.Fatalf("replaced nvl group = %v", g)
	}

	// Size forces replacement too.
	tf.writeConfig(t, nvlGroupCfg(pid, "tf-nvl", "GB200", 2, map[string]string{"env": "prod", "team": "ml"}))
	tf.step(t, map[string]string{"main": "replace"})
	newID := tf.stateID(t, nvlAddr+"main")
	if newID == labelID {
		t.Fatal("size change did not replace the nvl group")
	}
	assertNVLGroupGone(t, groups, labelID)
	g = nvlGroupGet(t, groups, newID)
	checkNVLGroup(t, tf.stateOf(t, nvlAddr+"main"), g)
	if g.GetSpec().GetSize() != 2 {
		t.Fatalf("resized nvl group = %v", g)
	}

	// Data sources by id and by parent_id + name.
	tf.writeConfig(t, nvlGroupCfg(pid, "tf-nvl", "GB200", 2, map[string]string{"env": "prod", "team": "ml"})+fmt.Sprintf(`
data "nebius_compute_v1_nvl_instance_group" "by_id" { id = nebius_compute_v1_nvl_instance_group.main.id }
data "nebius_compute_v1_nvl_instance_group" "by_name" {
  parent_id  = %q
  name       = "tf-nvl"
  depends_on = [nebius_compute_v1_nvl_instance_group.main]
}
output "by_id"   { value = data.nebius_compute_v1_nvl_instance_group.by_id }
output "by_name" { value = data.nebius_compute_v1_nvl_instance_group.by_name }
`, pid))
	tf.apply(t)
	tf.requireEmptyPlan(t)
	out := tf.outputs(t)
	for _, name := range []string{"by_id", "by_name"} {
		checkNVLGroup(t, out[name], g)
		if got := str(out[name], "type"); got != "GB200" {
			t.Errorf("%s: type = %q, want GB200", name, got)
		}
	}
	if a, b := str(out["by_id"], "id"), str(tf.stateOf(t, nvlAddr+"main"), "id"); a != b {
		t.Errorf("data source id %q vs resource id %q", a, b)
	}

	tf.destroy(t)
	assertNVLGroupGone(t, groups, newID)
}

func nvlInstanceCfg(name, platform, recovery string, env *instanceEnv) string {
	if recovery != "" {
		recovery = fmt.Sprintf("  recovery_policy       = %q\n", recovery)
	}
	return fmt.Sprintf(`
resource "nebius_compute_v1_instance" %[1]q {
  parent_id = %[2]q
  name      = %[3]q
  resources = {
    platform = %[4]q
    preset   = "4gpu-128vcpu-1600gb"
  }
  nvl_instance_group_id = nebius_compute_v1_nvl_instance_group.main.id
%[7]s  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "%[3]s-boot"
      spec = {
        type           = "NETWORK_SSD"
        size_gibibytes = 50
        source_image_family = {
          parent_id    = %[5]q
          image_family = "ubuntu24.04-cuda12"
        }
      }
    }
  }
  network_interfaces = [
    {
      name       = "eth0"
      subnet_id  = %[6]q
      ip_address = {}
    }
  ]
}
`, name, env.Inst.ProjectID, "tf-"+name, platform, publicImagesProject, env.SubnetID, recovery)
}

// TestInstanceInNVLGroup: two GB200 instances join an NVL instance group
// created in the same configuration; one destroy removes everything.
func TestInstanceInNVLGroup(t *testing.T) {
	env := startInstanceEnv(t)
	pid := env.Inst.ProjectID
	groups := computesvc.NewNVLInstanceGroupService(env.Inst.SDK(t))
	tf := env.editorTerraform(t)
	base := nvlGroupCfg(pid, "tf-nvl-members", "GB200", 2, nil)
	tf.writeConfig(t, base+nvlInstanceCfg("node0", "gpu-gb200", "ALWAYS", env)+nvlInstanceCfg("node1", "gpu-gb200", "", env))
	tf.init(t)
	tf.step(t, map[string]string{"main": "create", "node0": "create", "node1": "create"})

	groupID := tf.stateID(t, nvlAddr+"main")
	nodes := map[string]string{"node0": tf.stateID(t, instAddr+"node0"), "node1": tf.stateID(t, instAddr+"node1")}
	for name, id := range nodes {
		inst := env.instance(t, id)
		checkInstance(t, tf.stateOf(t, instAddr+name), inst)
		if inst.GetSpec().GetNvlInstanceGroupId() != groupID || inst.GetSpec().GetResources().GetPlatform() != "gpu-gb200" {
			t.Fatalf("%s: spec nvl_instance_group_id %q platform %q, want group %s on gpu-gb200",
				name, inst.GetSpec().GetNvlInstanceGroupId(), inst.GetSpec().GetResources().GetPlatform(), groupID)
		}
	}
	if got := env.instance(t, nodes["node0"]).GetSpec().GetRecoveryPolicy(); got != computepb.InstanceRecoveryPolicy_ALWAYS {
		t.Fatalf("node0 recovery_policy = %v, want ALWAYS", got)
	}

	// The group was read before the instances joined; a refresh shows the members.
	if err := tf.tf.Refresh(t.Context()); err != nil {
		t.Fatalf("terraform refresh: %v", err)
	}
	tf.requireEmptyPlan(t)
	group := nvlGroupGet(t, groups, groupID)
	checkNVLGroup(t, tf.stateOf(t, nvlAddr+"main"), group)
	if len(group.GetStatus().GetInstances()) != 2 {
		t.Fatalf("group status.instances = %v, want both nodes", group.GetStatus().GetInstances())
	}
	tfInst, _ := tf.stateOf(t, nvlAddr+"main")["status"].(map[string]any)["instances"].(map[string]any)
	for name, id := range nodes {
		if got := env.instance(t, id); got.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
			t.Fatalf("%s state = %v, want RUNNING", name, got.GetStatus().GetState())
		}
		if got := group.GetStatus().GetInstances()[id].GetInstanceState().String(); got != "RUNNING" {
			t.Fatalf("group member %s state = %v, want RUNNING", id, got)
		}
		if got := at(tfInst, id, "instance_state"); got != "RUNNING" {
			t.Fatalf("terraform group status.instances[%s].instance_state = %q, want RUNNING", id, got)
		}
	}
	listed, err := computesvc.NewInstanceService(env.Inst.SDK(t)).ListInstancesByNVLInstanceGroup(t.Context(),
		&computepb.ListInstancesByNVLInstanceGroupRequest{NvlInstanceGroupId: groupID})
	if err != nil {
		t.Fatalf("SDK ListInstancesByNVLInstanceGroup: %v", err)
	}
	var gotIDs []string
	for _, i := range listed.GetItems() {
		gotIDs = append(gotIDs, i.GetMetadata().GetId())
	}
	slices.Sort(gotIDs)
	if want := slices.Sorted(maps.Values(nodes)); !slices.Equal(gotIDs, want) {
		t.Fatalf("ListInstancesByNVLInstanceGroup = %v, want %v", gotIDs, want)
	}

	// A GB300 instance does not fit a GB200 group.
	tf.writeConfig(t, base+nvlInstanceCfg("node0", "gpu-gb200", "ALWAYS", env)+nvlInstanceCfg("node1", "gpu-gb200", "", env)+
		nvlInstanceCfg("wrong", "gpu-gb300", "", env))
	err = tf.tf.Apply(t.Context())
	if err == nil {
		t.Fatal("terraform apply of a gpu-gb300 instance in a GB200 group succeeded")
	}
	if msg := err.Error() + tf.out.String(); !strings.Contains(msg, "nvl_instance_group_id") {
		t.Fatalf("error does not mention nvl_instance_group_id:\n%s", msg)
	}
	tf.writeConfig(t, base+nvlInstanceCfg("node0", "gpu-gb200", "ALWAYS", env)+nvlInstanceCfg("node1", "gpu-gb200", "", env))

	tf.destroy(t)
	for _, id := range nodes {
		env.assertInstanceGone(t, id)
	}
	assertNVLGroupGone(t, groups, groupID)
}
