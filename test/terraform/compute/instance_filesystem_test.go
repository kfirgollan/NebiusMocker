//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
)

// instFSConfig is a CPU instance with a shared filesystem. attached renders
// the filesystems block, stopped the stopped flag.
func instFSConfig(pid, subnet string, attached, stopped bool) string {
	filesystems, stop := "", ""
	if attached {
		filesystems = `
  filesystems = [
    {
      attach_mode         = "READ_WRITE"
      mount_tag           = "data"
      existing_filesystem = { id = nebius_compute_v1_filesystem.x.id }
    }
  ]
`
	}
	if stopped {
		stop = "  stopped = true\n"
	}
	return fsCfg("x", pid, "shared-fs", "NETWORK_SSD", 16, 0, nil, "") + fmt.Sprintf(`
resource "nebius_compute_v1_instance" "vm" {
  parent_id = %[1]q
  name      = "fs-vm"
%[2]s
  resources = {
    platform = "cpu-e2"
    preset   = "2vcpu-8gb"
  }

  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "fs-vm-boot"
      spec = {
        type           = "NETWORK_SSD"
        size_gibibytes = 64
        source_image_family = {
          parent_id    = %[3]q
          image_family = "ubuntu24.04-driverless"
        }
      }
    }
  }
%[4]s
  network_interfaces = [
    {
      name       = "eth0"
      subnet_id  = %[5]q
      ip_address = {}
    }
  ]
}
`, pid, stop, publicImagesProject, filesystems, subnet)
}

// TestInstanceFilesystem: a shared filesystem attached to an instance
// through Terraform; detach and re-attach while stopped, then one destroy
// removes both in dependency order. The SDK checks the filesystem's
// attachments after every step and the plan must be empty after every apply.
func TestInstanceFilesystem(t *testing.T) {
	env := startInstanceEnv(t)
	pid := env.Inst.ProjectID
	filesystems := computesvc.NewFilesystemService(env.Inst.SDK(t))
	tf := env.editorTerraform(t)

	fsOf := func(id string) *computepb.Filesystem {
		t.Helper()
		f, err := filesystems.Get(t.Context(), &computepb.GetFilesystemRequest{Id: id})
		if err != nil {
			t.Fatalf("SDK Get filesystem %s: %v", id, err)
		}
		return f
	}
	wantAttached := func(fsID, vmID string, attached bool) {
		t.Helper()
		got := fsOf(fsID).GetStatus().GetReadWriteAttachments()
		want := []string(nil)
		if attached {
			want = []string{vmID}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("filesystem %s read_write_attachments = %v, want %v", fsID, got, want)
		}
		spec := env.instance(t, vmID).GetSpec().GetFilesystems()
		if attached != (len(spec) == 1 && spec[0].GetMountTag() == "data" && spec[0].GetExistingFilesystem().GetId() == fsID &&
			spec[0].GetAttachMode() == computepb.AttachedFilesystemSpec_READ_WRITE) {
			t.Fatalf("instance spec.filesystems = %v, attached = %v", spec, attached)
		}
	}

	// Create both: the instance mounts the filesystem from the start.
	tf.writeConfig(t, instFSConfig(pid, env.SubnetID, true, false))
	tf.init(t)
	tf.step(t, map[string]string{"x": "create", "vm": "create"})
	fsID, vmID := tf.stateID(t, fsAddr+"x"), tf.stateID(t, instAddr+"vm")
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), env.instance(t, vmID))
	checkFilesystem(t, tf.stateOf(t, fsAddr+"x"), fsOf(fsID))
	wantAttached(fsID, vmID, true)
	if st := env.instance(t, vmID).GetStatus().GetState(); st != computepb.InstanceStatus_RUNNING {
		t.Fatalf("created instance state = %v, want RUNNING", st)
	}

	// Detach (stopped = true in the same update).
	tf.writeConfig(t, instFSConfig(pid, env.SubnetID, false, true))
	tf.step(t, map[string]string{"x": "no-op", "vm": "update"})
	if got := tf.stateID(t, instAddr+"vm"); got != vmID {
		t.Fatalf("detach replaced the instance: %s -> %s", vmID, got)
	}
	inst := env.instance(t, vmID)
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), inst)
	checkFilesystem(t, tf.stateOf(t, fsAddr+"x"), fsOf(fsID))
	if inst.GetStatus().GetState() != computepb.InstanceStatus_STOPPED {
		t.Fatalf("instance state after detach = %v, want STOPPED", inst.GetStatus().GetState())
	}
	wantAttached(fsID, vmID, false)

	// Re-attach while stopped.
	tf.writeConfig(t, instFSConfig(pid, env.SubnetID, true, true))
	tf.step(t, map[string]string{"x": "no-op", "vm": "update"})
	if got := tf.stateID(t, instAddr+"vm"); got != vmID {
		t.Fatalf("re-attach replaced the instance: %s -> %s", vmID, got)
	}
	checkInstance(t, tf.stateOf(t, instAddr+"vm"), env.instance(t, vmID))
	checkFilesystem(t, tf.stateOf(t, fsAddr+"x"), fsOf(fsID))
	wantAttached(fsID, vmID, true)

	// One destroy: the instance first (detaching), then the filesystem.
	tf.destroy(t)
	env.assertInstanceGone(t, vmID)
	_, err := filesystems.Get(t.Context(), &computepb.GetFilesystemRequest{Id: fsID})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("filesystem %s after destroy: SDK Get code = %v (err %v), want NotFound", fsID, got, err)
	}
}
