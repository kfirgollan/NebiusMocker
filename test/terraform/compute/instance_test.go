//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"strings"
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
)

// cpuVM are the knobs of the upstream nebius_compute_v1_instance example
// (examples/resources/nebius_compute_v1_instance/resource.tf) that the
// lifecycle walks through.
type cpuVM struct {
	family, platform, preset string
	labels                   map[string]string
	stopped                  bool
	secondaries              int // 0 none, 1 the existing "data" disk, 2 data + a managed scratch disk
	scratchGiB               int
}

// cpuVMConfig is the upstream example with real IDs, plus the private
// allocation it references and an extra disk to attach.
func cpuVMConfig(pid, subnet string, vm cpuVM, withInstance bool) string {
	b := fmt.Sprintf(`
resource "nebius_vpc_v1_allocation" "priv" {
  parent_id = %[1]q
  name      = "example-vm-private-ip"
  ipv4_private = {
    subnet_id = %[2]q
  }
}
`, pid, subnet) + diskCfg("data", pid, "example-vm-data", "NETWORK_SSD", 16, 0, nil, "")
	if !withInstance {
		return b
	}
	secondary := ""
	switch vm.secondaries {
	case 1:
		secondary = `
  secondary_disks = [
    {
      attach_mode   = "READ_WRITE"
      existing_disk = { id = nebius_compute_v1_disk.data.id }
    }
  ]
`
	case 2:
		secondary = fmt.Sprintf(`
  secondary_disks = [
    {
      attach_mode   = "READ_WRITE"
      existing_disk = { id = nebius_compute_v1_disk.data.id }
    },
    {
      attach_mode = "READ_WRITE"
      managed_disk = {
        name = "example-vm-scratch"
        spec = {
          type           = "NETWORK_SSD"
          size_gibibytes = %d
        }
      }
    }
  ]
`, vm.scratchGiB)
	}
	stopped := ""
	if vm.stopped {
		stopped = "  stopped = true\n"
	}
	return b + fmt.Sprintf(`
resource "nebius_compute_v1_instance" "vm" {
  parent_id = %[1]q
  name      = "example-vm"
%[2]s%[3]s
  resources = {
    platform = %[4]q
    preset   = %[5]q
  }

  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "example-vm-boot"
      spec = {
        type           = "NETWORK_SSD"
        size_gibibytes = 64
        source_image_family = {
          parent_id    = %[6]q
          image_family = %[7]q
        }
      }
    }
  }
%[8]s
  network_interfaces = [
    {
      name      = "eth0"
      subnet_id = %[9]q
      ip_address = {
        allocation_id = nebius_vpc_v1_allocation.priv.id
      }
    }
  ]
}
`, pid, hclLabels(vm.labels), stopped, vm.platform, vm.preset, publicImagesProject, vm.family, secondary, subnet)
}

// TestInstanceLifecycle walks the upstream example through create, in-place
// updates (labels, stop, preset change while stopped, secondary disks
// attached / grown / detached, start), a replace on an immutable change and
// destroy, checking the mock with the SDK after every step and requiring an
// empty plan after every apply.
func TestInstanceLifecycle(t *testing.T) {
	env := startInstanceEnv(t)
	pid := env.Inst.ProjectID
	tf := env.editorTerraform(t)
	vm := cpuVM{family: "ubuntu22.04", platform: "cpu-e2", preset: "2vcpu-8gb", scratchGiB: 32}
	cfg := func() string { return cpuVMConfig(pid, env.SubnetID, vm, true) }

	// The example's family "ubuntu22.04" is not a Nebius image family: the
	// create fails with NotFound naming it, and nothing is left behind.
	tf.writeConfig(t, cfg())
	tf.init(t)
	before := tf.out.Len()
	err := tf.tf.Apply(t.Context())
	if err == nil {
		t.Fatal("apply with image family ubuntu22.04 succeeded")
	}
	if out := tf.out.String()[before:] + err.Error(); !strings.Contains(out, "NotFound") || !strings.Contains(out, "ubuntu22.04") {
		t.Fatalf("apply with an unknown image family failed without a NotFound naming it:\n%s", out)
	}
	list, err := env.Instances.List(t.Context(), &computepb.ListInstancesRequest{ParentId: pid})
	if err != nil || len(list.GetItems()) != 0 {
		t.Fatalf("instances after the failed create = %v (%v), want none", list.GetItems(), err)
	}
	allocID := tf.stateID(t, allocAddr+"priv")
	if a := env.allocation(t, allocID); a.GetStatus().GetState() != vpcpb.AllocationStatus_ALLOCATED {
		t.Fatalf("allocation after the failed create: state %v, want ALLOCATED", a.GetStatus().GetState())
	}

	// A real family from the public-images project.
	vm.family = "ubuntu24.04-driverless"
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "create"})
	vmID := tf.stateID(t, instAddr+"vm")
	inst := env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if inst.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
		t.Fatalf("created instance state = %v, want RUNNING", inst.GetStatus().GetState())
	}
	nic := inst.GetStatus().GetNetworkInterfaces()[0]
	priv := env.allocation(t, allocID)
	if nic.GetIpAddress().GetAllocationId() != allocID || priv.GetStatus().GetState() != vpcpb.AllocationStatus_ASSIGNED ||
		priv.GetStatus().GetAssignment().GetNetworkInterface().GetInstanceId() != vmID ||
		!strings.HasPrefix(priv.GetStatus().GetDetails().GetAllocatedCidr(), nic.GetIpAddress().GetAddress()+"/") {
		t.Fatalf("nic %v / allocation %v: want the allocation's address, assigned to %s", nic, priv.GetStatus(), vmID)
	}
	if want := vmID + "." + env.NetworkID + ".compute.internal."; nic.GetFqdn() != want {
		t.Fatalf("fqdn = %q, want %q", nic.GetFqdn(), want)
	}
	if n := len(inst.GetStatus().GetDiskAttachments()); n != 1 {
		t.Fatalf("disk attachments = %v, want the boot disk only", inst.GetStatus().GetDiskAttachments())
	}
	bootID := inst.GetStatus().GetDiskAttachments()[0].GetId()
	boot := env.disk(t, bootID)
	if boot.GetMetadata().GetName() != "example-vm-boot" || boot.GetStatus().GetManagedBy() != vmID ||
		boot.GetStatus().GetReadWriteAttachment() != vmID || boot.GetStatus().GetSourceImageId() == "" ||
		boot.GetStatus().GetSizeBytes() != 64<<30 || !inst.GetStatus().GetDiskAttachments()[0].GetIsManaged() {
		t.Fatalf("boot disk = %v", boot)
	}

	// In-place: labels only.
	vm.labels = map[string]string{"env": "dev", "team": "ml"}
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "update"})
	inst = env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if inst.GetMetadata().GetLabels()["team"] != "ml" || inst.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
		t.Fatalf("after the label update: labels %v state %v", inst.GetMetadata().GetLabels(), inst.GetStatus().GetState())
	}

	// stopped = true stops the instance.
	vm.stopped = true
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "update"})
	inst = env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if inst.GetStatus().GetState() != computepb.InstanceStatus_STOPPED || !inst.GetSpec().GetStopped() {
		t.Fatalf("after stopped = true: state %v, spec.stopped %v", inst.GetStatus().GetState(), inst.GetSpec().GetStopped())
	}

	// Preset change while stopped: in place.
	vm.preset = "4vcpu-16gb"
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "update"})
	inst = env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if inst.GetSpec().GetResources().GetPreset() != "4vcpu-16gb" || inst.GetStatus().GetState() != computepb.InstanceStatus_STOPPED {
		t.Fatalf("after the preset change: preset %q state %v", inst.GetSpec().GetResources().GetPreset(), inst.GetStatus().GetState())
	}

	// Attach the existing "data" disk in place.
	vm.secondaries = 1
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "update"})
	inst = env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	dataID := tf.stateID(t, diskAddr+"data")
	if att := attachedDisks(inst); len(att) != 2 || att[dataID] == nil || att[dataID].GetIsManaged() || att[bootID] == nil {
		t.Fatalf("disk attachments = %v, want boot and data (existing)", inst.GetStatus().GetDiskAttachments())
	}
	if d := env.disk(t, dataID); d.GetStatus().GetReadWriteAttachment() != vmID || d.GetStatus().GetManagedBy() != "" {
		t.Fatalf("existing data disk status = %v, want read-write attached to %s, not managed", d.GetStatus(), vmID)
	}

	// Add a managed scratch disk. Provider 0.6.91 marks
	// secondary_disks[*].managed_disk.spec.type RequiresReplace, so a new
	// managed element (null -> "NETWORK_SSD") replaces the instance: the old
	// one and its boot disk go, the data disk moves to the new instance.
	vm.secondaries = 2
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "replace"})
	oldID, oldBoot := vmID, bootID
	vmID = tf.stateID(t, instAddr+"vm")
	if vmID == oldID {
		t.Fatal("adding a managed secondary disk did not replace the instance")
	}
	env.assertInstanceGone(t, oldID)
	env.assertDiskGone(t, oldBoot)
	inst = env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if inst.GetStatus().GetState() != computepb.InstanceStatus_STOPPED || inst.GetSpec().GetResources().GetPreset() != "4vcpu-16gb" {
		t.Fatalf("replacement instance: state %v preset %q, want STOPPED 4vcpu-16gb", inst.GetStatus().GetState(), inst.GetSpec().GetResources().GetPreset())
	}
	bootID = inst.GetStatus().GetDiskAttachments()[0].GetId()
	att := attachedDisks(inst)
	if len(att) != 3 || att[dataID] == nil || att[dataID].GetIsManaged() || att[bootID] == nil {
		t.Fatalf("disk attachments = %v, want boot, data (existing) and a managed scratch disk", inst.GetStatus().GetDiskAttachments())
	}
	var scratchID string
	for id, a := range att {
		if id != bootID && id != dataID {
			scratchID = id
			if !a.GetIsManaged() || a.GetName() != "example-vm-scratch" {
				t.Fatalf("scratch attachment = %v", a)
			}
		}
	}
	data := env.disk(t, dataID)
	checkDisk(t, tf.stateOf(t, diskAddr+"data"), data)
	if data.GetStatus().GetReadWriteAttachment() != vmID || data.GetStatus().GetManagedBy() != "" {
		t.Fatalf("existing data disk status = %v, want read-write attached to %s, not managed", data.GetStatus(), vmID)
	}
	scratch := env.disk(t, scratchID)
	if scratch.GetStatus().GetReadWriteAttachment() != vmID || scratch.GetStatus().GetManagedBy() != vmID || scratch.GetStatus().GetSizeBytes() != 32<<30 {
		t.Fatalf("managed scratch disk status = %v", scratch.GetStatus())
	}

	// Grow the managed scratch disk in place.
	vm.scratchGiB = 48
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "update"})
	inst = env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if got := env.disk(t, scratchID).GetStatus().GetSizeBytes(); got != 48<<30 {
		t.Fatalf("scratch disk size after the grow = %d, want %d", got, int64(48<<30))
	}

	// Remove both: the existing disk is detached and kept, the managed one deleted.
	vm.secondaries = 0
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "update"})
	inst = env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if len(inst.GetStatus().GetDiskAttachments()) != 1 {
		t.Fatalf("disk attachments after removing the secondaries = %v", inst.GetStatus().GetDiskAttachments())
	}
	data = env.disk(t, dataID)
	checkDisk(t, tf.stateOf(t, diskAddr+"data"), data)
	if data.GetStatus().GetReadWriteAttachment() != "" {
		t.Fatalf("data disk still attached: %v", data.GetStatus())
	}
	env.assertDiskGone(t, scratchID)

	// stopped = false starts it again.
	vm.stopped = false
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "update"})
	inst = env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if inst.GetStatus().GetState() != computepb.InstanceStatus_RUNNING || inst.GetSpec().GetStopped() {
		t.Fatalf("after stopped = false: state %v", inst.GetStatus().GetState())
	}

	// The platform is immutable: changing it replaces the instance (and its
	// managed boot disk); the allocation moves to the new instance.
	vm.platform, vm.preset = "cpu-d3", "4vcpu-16gb"
	tf.writeConfig(t, cfg())
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "replace"})
	newID := tf.stateID(t, instAddr+"vm")
	if newID == vmID {
		t.Fatal("platform change did not replace the instance")
	}
	env.assertInstanceGone(t, vmID)
	env.assertDiskGone(t, bootID)
	inst = env.instance(t, newID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	if inst.GetSpec().GetResources().GetPlatform() != "cpu-d3" || inst.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
		t.Fatalf("replaced instance: platform %q state %v", inst.GetSpec().GetResources().GetPlatform(), inst.GetStatus().GetState())
	}
	if a := env.allocation(t, allocID); a.GetStatus().GetAssignment().GetNetworkInterface().GetInstanceId() != newID {
		t.Fatalf("allocation assignment after the replace = %v, want %s", a.GetStatus().GetAssignment(), newID)
	}
	newBootID := inst.GetStatus().GetDiskAttachments()[0].GetId()

	// Remove the instance: its managed boot disk goes with it, the
	// allocation is unassigned and stays until Terraform destroys it.
	tf.writeConfig(t, cpuVMConfig(pid, env.SubnetID, vm, false))
	tf.step(t, map[string]string{"priv": "no-op", "data": "no-op", "vm": "delete"})
	env.assertInstanceGone(t, newID)
	env.assertDiskGone(t, newBootID)
	if a := env.allocation(t, allocID); a.GetStatus().GetState() != vpcpb.AllocationStatus_ALLOCATED || a.GetStatus().GetAssignment() != nil {
		t.Fatalf("allocation after the instance is destroyed = %v, want ALLOCATED and unassigned", a.GetStatus())
	}
	tf.destroy(t)
	env.assertAllocationGone(t, allocID)
	env.assertDiskGone(t, dataID)
}

// gpuVMConfig is an 8-GPU H100 instance with a CUDA boot image, dynamic
// private and public addresses, cloud-init user data and a hostname, plus
// the instance data source read by id and by parent_id + name.
func gpuVMConfig(pid, subnet string) string {
	return fmt.Sprintf(`
resource "nebius_compute_v1_instance" "gpu" {
  parent_id = %[1]q
  name      = "tf-gpu-vm"
  hostname  = "trainer-0"
  resources = {
    platform = "gpu-h100-sxm"
    preset   = "8gpu-128vcpu-1600gb"
  }
  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "tf-gpu-vm-boot"
      spec = {
        type           = "NETWORK_SSD"
        size_gibibytes = 50
        source_image_family = {
          parent_id    = %[2]q
          image_family = "ubuntu24.04-cuda12"
        }
      }
    }
  }
  cloud_init_user_data = <<-EOT
    #cloud-config
    users:
      - name: trainer
        sudo: ALL=(ALL) NOPASSWD:ALL
  EOT
  network_interfaces = [
    {
      name              = "eth0"
      subnet_id         = %[3]q
      ip_address        = {}
      public_ip_address = {}
    }
  ]
}

data "nebius_compute_v1_instance" "by_id" { id = nebius_compute_v1_instance.gpu.id }
data "nebius_compute_v1_instance" "by_name" {
  parent_id  = %[1]q
  name       = "tf-gpu-vm"
  depends_on = [nebius_compute_v1_instance.gpu]
}
# cloud_init_user_data is sensitive in the provider schema.
output "by_id" {
  value     = data.nebius_compute_v1_instance.by_id
  sensitive = true
}
output "by_name" {
  value     = data.nebius_compute_v1_instance.by_name
  sensitive = true
}
`, pid, publicImagesProject, subnet)
}

// TestInstanceGPU: a GPU instance with dynamic addresses, read back through
// the instance data source by id and by parent_id + name.
func TestInstanceGPU(t *testing.T) {
	env := startInstanceEnv(t)
	pid := env.Inst.ProjectID
	tf := env.editorTerraform(t)
	tf.writeConfig(t, gpuVMConfig(pid, env.SubnetID))
	tf.init(t)
	tf.step(t, map[string]string{"gpu": "create"})

	id := tf.stateID(t, instAddr+"gpu")
	inst := env.instance(t, id)
	checkInstance(t, tf.stateOf(t, instAddr+"gpu"), inst)
	st := inst.GetStatus()
	if st.GetState() != computepb.InstanceStatus_RUNNING || inst.GetSpec().GetResources().GetPreset() != "8gpu-128vcpu-1600gb" ||
		!strings.HasPrefix(inst.GetSpec().GetCloudInitUserData(), "#cloud-config") {
		t.Fatalf("GPU instance = %v", inst)
	}
	nic := st.GetNetworkInterfaces()[0]
	if nic.GetIpAddress().GetAddress() == "" || nic.GetPublicIpAddress().GetAddress() == "" || nic.GetPublicIpAddress().GetStatic() {
		t.Fatalf("nic = %v, want a dynamic private and public address", nic)
	}
	if want := "trainer-0." + env.NetworkID + ".compute.internal."; nic.GetFqdn() != want {
		t.Fatalf("fqdn = %q, want %q", nic.GetFqdn(), want)
	}
	privAlloc, pubAlloc := nic.GetIpAddress().GetAllocationId(), nic.GetPublicIpAddress().GetAllocationId()
	for _, a := range []string{privAlloc, pubAlloc} {
		if got := env.allocation(t, a); got.GetStatus().GetState() != vpcpb.AllocationStatus_ASSIGNED || got.GetStatus().GetStatic() {
			t.Fatalf("dynamic allocation %s = %v", a, got.GetStatus())
		}
	}
	bootID := st.GetDiskAttachments()[0].GetId()
	if boot := env.disk(t, bootID); boot.GetStatus().GetManagedBy() != id || boot.GetStatus().GetSizeBytes() != 50<<30 {
		t.Fatalf("boot disk = %v", boot)
	}

	out := tf.outputs(t)
	for _, name := range []string{"by_id", "by_name"} {
		checkInstance(t, out[name], inst)
		if got := at(out[name], "status", "network_interfaces", 0, "public_ip_address", "allocation_id"); got != pubAlloc {
			t.Errorf("%s: public allocation = %q, want %q", name, got, pubAlloc)
		}
		if got := at(out[name], "cloud_init_user_data"); got != inst.GetSpec().GetCloudInitUserData() {
			t.Errorf("%s: cloud_init_user_data = %q, want %q", name, got, inst.GetSpec().GetCloudInitUserData())
		}
	}

	tf.destroy(t)
	env.assertInstanceGone(t, id)
	env.assertDiskGone(t, bootID)
	env.assertAllocationGone(t, privAlloc)
	env.assertAllocationGone(t, pubAlloc)
}

// TestInstanceInGpuCluster: an 8-GPU H100 instance joins a GPU cluster
// created in the same configuration; one destroy removes both.
func TestInstanceInGpuCluster(t *testing.T) {
	env := startInstanceEnv(t)
	pid := env.Inst.ProjectID
	clusters := computesvc.NewGpuClusterService(env.Inst.SDK(t))
	tf := env.editorTerraform(t)
	tf.writeConfig(t, gpuClusterCfg(pid, "tf-ib-cluster", "fabric-3", nil)+fmt.Sprintf(`
resource "nebius_compute_v1_instance" "node" {
  parent_id = %[1]q
  name      = "tf-ib-node-0"
  resources = {
    platform = "gpu-h100-sxm"
    preset   = "8gpu-128vcpu-1600gb"
  }
  gpu_cluster = { id = nebius_compute_v1_gpu_cluster.main.id }
  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "tf-ib-node-0-boot"
      spec = {
        type           = "NETWORK_SSD"
        size_gibibytes = 50
        source_image_family = {
          parent_id    = %[2]q
          image_family = "ubuntu24.04-cuda12"
        }
      }
    }
  }
  network_interfaces = [
    {
      name       = "eth0"
      subnet_id  = %[3]q
      ip_address = {}
    }
  ]
}
`, pid, publicImagesProject, env.SubnetID))
	tf.init(t)
	tf.step(t, map[string]string{"main": "create", "node": "create"})

	clusterID, nodeID := tf.stateID(t, gpuAddr+"main"), tf.stateID(t, instAddr+"node")
	inst := env.instance(t, nodeID)
	checkInstance(t, tf.stateOf(t, instAddr+"node"), inst)
	if inst.GetSpec().GetGpuCluster().GetId() != clusterID || len(inst.GetStatus().GetInfinibandTopologyPath().GetPath()) == 0 {
		t.Fatalf("instance gpu_cluster %v, infiniband_topology_path %v: want membership of %s with a topology path",
			inst.GetSpec().GetGpuCluster(), inst.GetStatus().GetInfinibandTopologyPath(), clusterID)
	}
	if got := at(tf.stateOf(t, instAddr+"node"), "status", "infiniband_topology_path", "path", 0); got != inst.GetStatus().GetInfinibandTopologyPath().GetPath()[0] {
		t.Fatalf("terraform status.infiniband_topology_path.path[0] = %q, mock has %v", got, inst.GetStatus().GetInfinibandTopologyPath().GetPath())
	}
	// The cluster was read before the instance joined; a refresh shows the member.
	if err := tf.tf.Refresh(t.Context()); err != nil {
		t.Fatalf("terraform refresh: %v", err)
	}
	tf.requireEmptyPlan(t)
	cluster := gpuClusterGet(t, clusters, clusterID)
	checkGpuCluster(t, tf.stateOf(t, gpuAddr+"main"), cluster)
	if got := at(tf.stateOf(t, gpuAddr+"main"), "status", "instances", 0); got != nodeID {
		t.Fatalf("terraform gpu cluster status.instances[0] = %q, want %s", got, nodeID)
	}
	if got := cluster.GetStatus().GetInstances(); len(got) != 1 || got[0] != nodeID {
		t.Fatalf("gpu cluster status.instances = %v, want [%s]", got, nodeID)
	}

	tf.destroy(t)
	env.assertInstanceGone(t, nodeID)
	assertGpuClusterGone(t, clusters, clusterID)
}
