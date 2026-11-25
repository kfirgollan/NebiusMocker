//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// publicVMConfig is a CPU instance with a dynamic public address
// (public_ip_address = {}), stopped or running.
func publicVMConfig(pid, subnet string, stopped bool) string {
	return fmt.Sprintf(`
resource "nebius_compute_v1_instance" "pub" {
  parent_id = %[1]q
  name      = "tf-public-vm"
  stopped   = %[4]t
  resources = {
    platform = "cpu-e2"
    preset   = "2vcpu-8gb"
  }
  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "tf-public-vm-boot"
      spec = {
        type           = "NETWORK_SSD"
        size_gibibytes = 20
        source_image_family = {
          parent_id    = %[2]q
          image_family = "ubuntu24.04-driverless"
        }
      }
    }
  }
  network_interfaces = [
    {
      name              = "eth0"
      subnet_id         = %[3]q
      ip_address        = {}
      public_ip_address = {}
    }
  ]
}
`, pid, publicImagesProject, subnet, stopped)
}

// TestInstanceDynamicPublicIPAcrossStop: a dynamic public address survives a
// stop/start within an hour and is released after an hour STOPPED (docs
// compute/virtual-machines/network), on the mock clock. Only status moves:
// the plan stays empty at every step.
func TestInstanceDynamicPublicIPAcrossStop(t *testing.T) {
	clk := clock.NewManual(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	env := startInstanceEnv(t, testkit.WithClock(clk), testkit.WithOperationDelay(0)) // inline ops: the clock is manual
	pid := env.Inst.ProjectID
	tf := env.editorTerraform(t)
	addr := instAddr + "pub"
	publicOf := func(id string) *computepb.PublicIPAddressStatus {
		return env.instance(t, id).GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
	}

	tf.writeConfig(t, publicVMConfig(pid, env.SubnetID, false))
	tf.init(t)
	tf.step(t, map[string]string{"pub": "create"})
	id := tf.stateID(t, addr)
	first := publicOf(id)
	if first.GetAddress() == "" || first.GetStatic() {
		t.Fatalf("public ip = %v, want a dynamic address", first)
	}

	// Stop, start again within the hour: same address.
	tf.writeConfig(t, publicVMConfig(pid, env.SubnetID, true))
	tf.step(t, map[string]string{"pub": "update"})
	if got := publicOf(id); got.GetAddress() != first.GetAddress() {
		t.Fatalf("public ip after stop = %v, want %s kept", got, first.GetAddress())
	}
	if a := env.allocation(t, first.GetAllocationId()); a.GetStatus().GetState() != vpcpb.AllocationStatus_ASSIGNED {
		t.Fatalf("allocation while stopped = %v, want ASSIGNED", a.GetStatus())
	}
	clk.Advance(30 * time.Minute)
	tf.requireEmptyPlan(t)
	tf.writeConfig(t, publicVMConfig(pid, env.SubnetID, false))
	tf.step(t, map[string]string{"pub": "update"})
	if got := publicOf(id); got.GetAddress() != first.GetAddress() {
		t.Fatalf("public ip after a start within the hour = %v, want %s", got, first.GetAddress())
	}
	checkInstance(t, tf.stateOf(t, addr), env.instance(t, id))

	// Stop for more than an hour: the address is released.
	tf.writeConfig(t, publicVMConfig(pid, env.SubnetID, true))
	tf.step(t, map[string]string{"pub": "update"})
	clk.Advance(time.Hour)
	deadline := clock.Real{}.Now().Add(30 * time.Second)
	for {
		clk.Advance(time.Minute) // the sweep runs every minute of the mock clock
		_, err := env.Allocations.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: first.GetAllocationId()})
		if status.Code(err) == codes.NotFound {
			break
		}
		if err != nil {
			t.Fatalf("get allocation: %v", err)
		}
		if (clock.Real{}).Now().After(deadline) {
			t.Fatal("dynamic public allocation not released after an hour stopped")
		}
		<-(clock.Real{}).After(20 * time.Millisecond)
	}
	if got := publicOf(id); got != nil {
		t.Fatalf("public ip after release = %v, want none", got)
	}
	tf.requireEmptyPlan(t) // status-only change: no drift

	// Start: a new address, still no drift.
	tf.writeConfig(t, publicVMConfig(pid, env.SubnetID, false))
	tf.step(t, map[string]string{"pub": "update"})
	again := publicOf(id)
	if again.GetAddress() == "" || again.GetAllocationId() == first.GetAllocationId() {
		t.Fatalf("public ip after start = %v, want a new allocation (old %v)", again, first)
	}
	checkInstance(t, tf.stateOf(t, addr), env.instance(t, id))

	tf.destroy(t)
	env.assertInstanceGone(t, id)
	env.assertAllocationGone(t, again.GetAllocationId())
}
