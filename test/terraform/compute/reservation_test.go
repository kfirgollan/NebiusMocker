//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	capacitysvc "github.com/nebius/gosdk/services/nebius/capacity/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/capacity"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// reservationVM renders one 8-GPU H100 VM with the given reservation_policy.
func reservationVM(name, pid, subnet, policy string) string {
	return fmt.Sprintf(`
resource "nebius_compute_v1_instance" %[1]q {
  parent_id = %[2]q
  name      = %[1]q
  resources = {
    platform = "gpu-h100-sxm"
    preset   = "8gpu-128vcpu-1600gb"
  }
  reservation_policy = %[4]s
  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "%[1]s-boot"
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
      subnet_id  = %[3]q
      ip_address = {}
    }
  ]
}
`, name, pid, subnet, policy, publicImagesProject)
}

func reservationOf(t *testing.T, c computesvc.InstanceService, id string) string {
	t.Helper()
	inst, err := c.Get(t.Context(), &computepb.GetInstanceRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get instance %s: %v", id, err)
	}
	return inst.GetStatus().GetReservationId()
}

// TestInstanceReservations: instances draw GPUs from the H100 capacity block
// group through reservation_policy, visible through the capacity SDK.
func TestInstanceReservations(t *testing.T) {
	opts := []testkit.Option{testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce)}
	scn := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{vpc.New(), capacity.New()}, opts...)
	sdk := scn.Inst.SDK(t)
	subnet, err := vpcsvc.NewSubnetService(sdk).GetByName(t.Context(),
		&vpcpb.GetSubnetByNameRequest{ParentId: scn.Inst.ProjectID, Name: "default-subnet"})
	if err != nil {
		t.Fatalf("SDK GetByName default-subnet: %v", err)
	}
	pid, subnetID := scn.Inst.ProjectID, subnet.GetMetadata().GetId()
	instances := computesvc.NewInstanceService(sdk)
	groups := capacitysvc.NewCapacityBlockGroupService(sdk)

	group, err := groups.GetByResourceAffinity(t.Context(), &capacitypb.GetCapacityBlockGroupByResourceAffinityRequest{
		ParentId: scn.Inst.TenantID, Region: "eu-north1",
		ResourceAffinity: &capacitypb.ResourceAffinity{Versions: &capacitypb.ResourceAffinity_ComputeV1{
			ComputeV1: &capacitypb.ResourceAffinityComputeV1{Platform: "gpu-h100-sxm", Fabric: "fabric-2"},
		}},
	})
	if err != nil {
		t.Fatalf("SDK GetByResourceAffinity h100: %v", err)
	}
	gid := group.GetMetadata().GetId()
	usage := func(want uint64, resources int) {
		t.Helper()
		g, err := groups.Get(t.Context(), &capacitypb.GetCapacityBlockGroupRequest{Id: gid})
		if err != nil {
			t.Fatalf("SDK Get block group: %v", err)
		}
		if got := g.GetStatus().GetUsage(); got != want {
			t.Fatalf("block group %s usage = %d, want %d", gid, got, want)
		}
		res, err := groups.ListResources(t.Context(), &capacitypb.ListCapacityBlockGroupResourcesRequest{Id: gid})
		if err != nil {
			t.Fatalf("SDK ListResources: %v", err)
		}
		if len(res.GetResourceIds()) != resources {
			t.Fatalf("block group resources = %v, want %d", res.GetResourceIds(), resources)
		}
	}
	usage(0, 0)

	tf := newTerraform(t, scn.Inst.Addr, config.DefaultEditorToken)
	auto := fmt.Sprintf(`{ policy = "AUTO", reservation_ids = [%q] }`, gid)
	tf.writeConfig(t, reservationVM("auto", pid, subnetID, auto))
	tf.init(t)
	tf.step(t, map[string]string{"auto": "create"})
	autoID := tf.stateID(t, instAddr+"auto")
	inst, err := instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: autoID})
	if err != nil {
		t.Fatal(err)
	}
	if inst.GetStatus().GetReservationId() != gid ||
		inst.GetSpec().GetReservationPolicy().GetPolicy() != computepb.ReservationPolicy_AUTO ||
		len(inst.GetSpec().GetReservationPolicy().GetReservationIds()) != 1 {
		t.Fatalf("AUTO instance: status.reservation_id = %q, policy = %v; want %s", inst.GetStatus().GetReservationId(),
			inst.GetSpec().GetReservationPolicy(), gid)
	}
	if got := at(tf.stateOf(t, instAddr+"auto"), "status", "reservation_id"); got != gid {
		t.Fatalf("terraform status.reservation_id = %q, want %s", got, gid)
	}
	usage(8, 1)

	both := reservationVM("auto", pid, subnetID, auto) + reservationVM("strict", pid, subnetID, `{ policy = "STRICT" }`)
	tf.writeConfig(t, both)
	tf.step(t, map[string]string{"auto": "no-op", "strict": "create"})
	strictID := tf.stateID(t, instAddr+"strict")
	if got := reservationOf(t, instances, strictID); got != gid {
		t.Fatalf("STRICT instance status.reservation_id = %q, want %s", got, gid)
	}
	usage(16, 2)

	// The group is full: a third STRICT VM cannot be placed.
	tf.writeConfig(t, both+reservationVM("third", pid, subnetID, `{ policy = "STRICT" }`))
	if err := tf.tf.Apply(t.Context()); err == nil {
		t.Fatal("apply of a third STRICT VM succeeded on a full block group")
	} else if msg := strings.ToLower(tf.out.String() + err.Error()); !strings.Contains(msg, "exhausted") && !strings.Contains(msg, "capacity") && !strings.Contains(msg, "quota") {
		t.Fatalf("third STRICT apply error does not mention capacity: %v\n%s", err, tf.out.String())
	}
	usage(16, 2)
	tf.writeConfig(t, both)
	tf.requireEmptyPlan(t)

	tf.destroy(t)
	usage(0, 0)
	if _, err := instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: strictID}); status.Code(err) != codes.NotFound {
		t.Fatalf("instance after destroy: %v, want NotFound", err)
	}
}
