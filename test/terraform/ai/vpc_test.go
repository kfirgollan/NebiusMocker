//go:build e2e

package ai

import (
	"fmt"
	"strings"
	"testing"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	aisvc "github.com/nebius/gosdk/services/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

func startMockWithVPC(t *testing.T) *mock {
	t.Helper()
	return startMocker(t, vpc.New())
}

// subnetEndpointConfig renders an endpoint attached to the given subnet
// expression (a quoted ID or a resource reference).
func subnetEndpointConfig(projectID, subnetExpr string) string {
	return fmt.Sprintf(`
resource "nebius_ai_v1_endpoint" "e" {
  parent_id  = %q
  name       = "web"
  image      = "nginx:alpine"
  platform   = "cpu-d3"
  preset     = "4vcpu-16gb"
  disk       = { type = "NETWORK_SSD", size_bytes = 21474836480 }
  ports      = [{ container_port = 80, protocol = "HTTP" }]
  auth_token = %q
  subnet_id  = %s
}
`, projectID, endpointToken, subnetExpr)
}

// TestEndpointOnRealSubnet attaches an endpoint to a subnet created by the
// official provider in the same configuration.
func TestEndpointOnRealSubnet(t *testing.T) {
	t.Parallel()
	inst := startMockWithVPC(t)
	endpoints := aisvc.NewEndpointService(inst.SDK(t))
	tf := newTerraform(t, inst.Instance)

	tf.writeConfig(t, fmt.Sprintf(`
resource "nebius_vpc_v1_network" "net" {
  parent_id = %[1]q
  name      = "ai-net"
}

resource "nebius_vpc_v1_subnet" "sub" {
  parent_id          = %[1]q
  name               = "ai-sub"
  network_id         = nebius_vpc_v1_network.net.id
  ipv4_private_pools = { pools = [{ cidrs = [{ cidr = "/24" }] }] }
}
`, inst.ProjectID)+subnetEndpointConfig(inst.ProjectID, "nebius_vpc_v1_subnet.sub.id"))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"net": "create", "sub": "create", "e": "create"})
	tf.apply(t)

	ids := tf.stateIDs(t)
	if ids["sub"] == "" || ids["e"] == "" {
		t.Fatalf("state IDs = %v, want subnet and endpoint", ids)
	}
	got, err := endpoints.Get(t.Context(), &aipb.GetEndpointRequest{Id: ids["e"]})
	if err != nil {
		t.Fatalf("Get endpoint in mock: %v", err)
	}
	if got.GetSpec().GetSubnetId() != ids["sub"] {
		t.Errorf("spec.subnet_id = %q, want the real subnet %q", got.GetSpec().GetSubnetId(), ids["sub"])
	}
	if got.GetStatus().GetState() != aipb.EndpointStatus_RUNNING {
		t.Errorf("state = %v, want RUNNING", got.GetStatus().GetState())
	}
	tf.expectNoChanges(t)
	tf.destroy(t)
}

// TestEndpointOnMissingSubnet applies an endpoint whose well-formed subnet ID
// names nothing: the mock must refuse it and name the subnet.
func TestEndpointOnMissingSubnet(t *testing.T) {
	t.Parallel()
	const missing = "vpcsubnet-e00doesnotexist00"
	inst := startMockWithVPC(t)
	tf := newTerraform(t, inst.Instance)

	tf.writeConfig(t, subnetEndpointConfig(inst.ProjectID, fmt.Sprintf("%q", missing)))
	tf.init(t)
	if err := tf.tf.Apply(t.Context()); err == nil {
		t.Fatal("apply succeeded, want a not-found error for the missing subnet")
	}
	out := tf.out.String()
	if !strings.Contains(out, missing) || !strings.Contains(strings.ToLower(out), "not found") {
		t.Errorf("terraform output does not report %s as not found:\n%s", missing, out)
	}
}
