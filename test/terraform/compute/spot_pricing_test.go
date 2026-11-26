//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	billingsvc "github.com/nebius/gosdk/services/nebius/billing/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/billing"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// spotConfig renders a pricing policy plus a preemptible H100 VM that
// references it with spot_pricing_policy.
func spotConfig(pid, subnet, policyName, maxPrice string, stopped bool) string {
	stop := ""
	if stopped {
		stop = "  stopped = true\n"
	}
	return fmt.Sprintf(`
resource "nebius_billing_v1_pricing_policy" "p" {
  parent_id             = %[1]q
  name                  = %[3]q
  compute_instance_spec = { v1 = { platform = "gpu-h100-sxm" } }
  pricing               = { max_price_v1 = { max_price = %[4]q } }
}

resource "nebius_compute_v1_instance" "spot" {
  parent_id = %[1]q
  name      = "spot-vm"
%[5]s  resources = {
    platform = "gpu-h100-sxm"
    preset   = "1gpu-16vcpu-200gb"
  }
  preemptible = {
    on_preemption = "STOP"
  }
  recovery_policy = "FAIL"
  spot_pricing_policy = {
    id = nebius_billing_v1_pricing_policy.p.id
  }
  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "spot-vm-boot"
      spec = {
        type           = "NETWORK_SSD"
        size_gibibytes = 50
        source_image_family = {
          parent_id    = %[6]q
          image_family = "ubuntu24.04-cuda12"
        }
      }
    }
  }
  network_interfaces = [
    {
      name       = "eth0"
      subnet_id  = %[2]q
      ip_address = {}
    }
  ]
}
`, pid, subnet, policyName, maxPrice, stop, publicImagesProject)
}

// TestSpotPricingPolicyInstance walks a preemptible VM that references a
// billing pricing policy: the policy counts it while it runs, and Terraform
// plans stay empty through rename, stop and a max_price change.
func TestSpotPricingPolicyInstance(t *testing.T) {
	opts := []testkit.Option{testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce)}
	scn := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{vpc.New(), billing.New()}, opts...)
	sdk := scn.Inst.SDK(t)
	subnet, err := vpcsvc.NewSubnetService(sdk).GetByName(t.Context(),
		&vpcpb.GetSubnetByNameRequest{ParentId: scn.Inst.ProjectID, Name: "default-subnet"})
	if err != nil {
		t.Fatalf("SDK GetByName default-subnet: %v", err)
	}
	pid, subnetID := scn.Inst.ProjectID, subnet.GetMetadata().GetId()
	instances := computesvc.NewInstanceService(sdk)
	policies := billingsvc.NewPricingPolicyService(sdk)

	// check reads both resources with the official SDK.
	check := func(polID, vmID, name, maxPrice string, running int64, state computepb.InstanceStatus_InstanceState) {
		t.Helper()
		pol, err := policies.Get(t.Context(), &billingpb.GetPricingPolicyRequest{Id: polID})
		if err != nil {
			t.Fatalf("SDK Get pricing policy %s: %v", polID, err)
		}
		if got := pol.GetStatus().GetRunningVmCount(); got != running {
			t.Fatalf("policy running_vm_count = %d, want %d", got, running)
		}
		if got := pol.GetMetadata().GetName(); got != name {
			t.Fatalf("policy name = %q, want %q", got, name)
		}
		if got := pol.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice(); got != maxPrice {
			t.Fatalf("policy max_price = %q, want %q", got, maxPrice)
		}
		inst, err := instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: vmID})
		if err != nil {
			t.Fatalf("SDK Get instance %s: %v", vmID, err)
		}
		if got := inst.GetStatus().GetState(); got != state {
			t.Fatalf("instance state = %v, want %v", got, state)
		}
		if got := inst.GetSpec().GetSpotPricingPolicy().GetId(); got != polID {
			t.Fatalf("instance spec.spot_pricing_policy.id = %q, want %s", got, polID)
		}
	}

	tf := newTerraform(t, scn.Inst.Addr, config.DefaultEditorToken)
	tf.writeConfig(t, spotConfig(pid, subnetID, "spotpolicy", "2.000", false))
	tf.init(t)

	// 1. create both: the policy counts the running VM.
	tf.step(t, map[string]string{"p": "create", "spot": "create"})
	polID, vmID := tf.stateID(t, "nebius_billing_v1_pricing_policy.p"), tf.stateID(t, instAddr+"spot")
	check(polID, vmID, "spotpolicy", "2.000", 1, computepb.InstanceStatus_RUNNING)

	// 2. rename the policy in place while a VM runs on it.
	tf.writeConfig(t, spotConfig(pid, subnetID, "spotpolicyrenamed", "2.000", false))
	tf.step(t, map[string]string{"p": "update", "spot": "no-op"})
	check(polID, vmID, "spotpolicyrenamed", "2.000", 1, computepb.InstanceStatus_RUNNING)

	// 3. stop the VM, then change max_price (refused while a VM runs).
	tf.writeConfig(t, spotConfig(pid, subnetID, "spotpolicyrenamed", "2.000", true))
	tf.step(t, map[string]string{"p": "no-op", "spot": "update"})
	check(polID, vmID, "spotpolicyrenamed", "2.000", 0, computepb.InstanceStatus_STOPPED)
	tf.writeConfig(t, spotConfig(pid, subnetID, "spotpolicyrenamed", "2.500", true))
	tf.step(t, map[string]string{"p": "update", "spot": "no-op"})
	check(polID, vmID, "spotpolicyrenamed", "2.500", 0, computepb.InstanceStatus_STOPPED)

	// 4. destroy: the instance goes first, then the policy.
	tf.destroy(t)
	if _, err := instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: vmID}); status.Code(err) != codes.NotFound {
		t.Fatalf("instance after destroy: %v, want NotFound", err)
	}
	if _, err := policies.Get(t.Context(), &billingpb.GetPricingPolicyRequest{Id: polID}); status.Code(err) != codes.NotFound {
		t.Fatalf("pricing policy after destroy: %v, want NotFound", err)
	}
}
