//go:build e2e

package vpc

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"
	alphasvc "github.com/nebius/gosdk/services/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// alphaClients are the official Go SDK nebius.vpc.v1alpha1 services.
type alphaClients struct {
	allocs alphasvc.AllocationService
	pools  alphasvc.PoolService
	nets   alphasvc.NetworkService
	subs   alphasvc.SubnetService
	scopes alphasvc.ScopeService
}

func newAlphaClients(t *testing.T, inst *testkit.Instance) *alphaClients {
	t.Helper()
	sdk := inst.SDK(t)
	return &alphaClients{
		allocs: alphasvc.NewAllocationService(sdk), pools: alphasvc.NewPoolService(sdk),
		nets: alphasvc.NewNetworkService(sdk), subs: alphasvc.NewSubnetService(sdk), scopes: alphasvc.NewScopeService(sdk),
	}
}

func (a *alphaClients) allocation(t *testing.T, id string) *vpcv1alpha1pb.Allocation {
	t.Helper()
	got, err := a.allocs.Get(t.Context(), &vpcv1alpha1pb.GetAllocationRequest{Id: id})
	if err != nil {
		t.Fatalf("v1alpha1 Get allocation %s: %v", id, err)
	}
	return got
}

func (a *alphaClients) allocationGone(t *testing.T, id string) {
	t.Helper()
	if _, err := a.allocs.Get(t.Context(), &vpcv1alpha1pb.GetAllocationRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("v1alpha1 Get allocation %s after delete: code %v, want NotFound (err: %v)", id, status.Code(err), err)
	}
}

const alphaLookups = `
data "nebius_vpc_v1alpha1_subnet" "default" {
  parent_id = %[1]q
  name      = "default-subnet"
}

data "nebius_vpc_v1alpha1_pool" "private" {
  parent_id = %[1]q
  name      = "default-network-pool"
}
`

func alphaAllocHCL(project, name, labels, spec string) string {
	return fmt.Sprintf(`
resource "nebius_vpc_v1alpha1_allocation" "a" {
  parent_id = %q
  name      = %q
  labels    = { %s }
  %s
}
`, project, name, labels, spec)
}

// TestV1alpha1AllocationLifecycle drives nebius_vpc_v1alpha1_allocation through
// create, in-place update, replacement and destroy with the official provider,
// checking every stage with the v1alpha1 SDK and that v1 sees the same object.
func TestV1alpha1AllocationLifecycle(t *testing.T) {
	inst, c, tf := startVPC(t)
	a := newAlphaClients(t, inst)
	p := inst.ProjectID
	lookups := fmt.Sprintf(alphaLookups, p)
	all := []string{"a"}

	privSubnet := `ipv4_private = { subnet_id = data.nebius_vpc_v1alpha1_subnet.default.id }`
	tf.writeConfig(t, lookups+alphaAllocHCL(p, "alpha-alloc", `team = "platform"`, privSubnet))
	tf.init(t)
	tf.expectChanges(t, nil, map[string]string{"a": "create"})
	tf.apply(t)
	tf.expectNoChanges(t)

	id := tf.state(t)["a"].ID
	got := a.allocation(t, id)
	if got.GetMetadata().GetId() != id || got.GetMetadata().GetName() != "alpha-alloc" || got.GetMetadata().GetParentId() != p {
		t.Fatalf("v1alpha1 allocation metadata = %v, want id %s name alpha-alloc parent %s", got.GetMetadata(), id, p)
	}
	if got.GetStatus().GetState() != vpcv1alpha1pb.AllocationStatus_ALLOCATED {
		t.Errorf("status.state = %v, want ALLOCATED", got.GetStatus().GetState())
	}
	if pre, err := netip.ParsePrefix(got.GetStatus().GetDetails().GetAllocatedCidr()); err != nil || pre.Bits() != 32 || !netip.MustParsePrefix("10.0.0.0/13").Contains(pre.Addr()) {
		t.Errorf("status.details.allocated_cidr = %q (%v), want a /32 inside 10.0.0.0/13", got.GetStatus().GetDetails().GetAllocatedCidr(), err)
	}
	if got.GetSpec().GetIpv4Private().GetSubnetId() == "" {
		t.Errorf("spec.ipv4_private.subnet_id is empty, want the default subnet")
	}
	v1, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: id})
	if err != nil {
		t.Fatalf("v1 Get of the v1alpha1 allocation %s: %v", id, err)
	}
	if v1.GetMetadata().GetId() != id || v1.GetStatus().GetDetails().GetAllocatedCidr() != got.GetStatus().GetDetails().GetAllocatedCidr() {
		t.Errorf("v1 view = %v, want the same object as v1alpha1 %v", v1, got)
	}
	version := got.GetMetadata().GetResourceVersion()

	// In-place update of name and labels.
	tf.writeConfig(t, lookups+alphaAllocHCL(p, "alpha-alloc-renamed", `team = "net", tier = "gold"`, privSubnet))
	tf.expectChanges(t, all, map[string]string{"a": "update"})
	tf.apply(t)
	tf.expectNoChanges(t)
	if now := tf.state(t)["a"].ID; now != id {
		t.Fatalf("in-place update changed the id: %s -> %s", id, now)
	}
	got = a.allocation(t, id)
	if got.GetMetadata().GetName() != "alpha-alloc-renamed" || got.GetMetadata().GetLabels()["tier"] != "gold" || got.GetMetadata().GetLabels()["team"] != "net" {
		t.Errorf("after update metadata = %v, want name alpha-alloc-renamed labels team=net tier=gold", got.GetMetadata())
	}
	if got.GetMetadata().GetResourceVersion() <= version {
		t.Errorf("resource_version = %d, want > %d after update", got.GetMetadata().GetResourceVersion(), version)
	}
	if n, _ := c.nameLabels(t, "allocation", id); n != "alpha-alloc-renamed" {
		t.Errorf("v1 name = %q, want alpha-alloc-renamed", n)
	}

	// RequiresReplace: switch to an explicit address from the private pool.
	byPool := `ipv4_private = {
    pool_id = data.nebius_vpc_v1alpha1_pool.private.id
    cidr    = "10.7.0.10"
  }`
	tf.writeConfig(t, lookups+alphaAllocHCL(p, "alpha-alloc-renamed", `team = "net", tier = "gold"`, byPool))
	tf.expectChanges(t, all, map[string]string{"a": "replace"})
	tf.apply(t)
	tf.expectNoChanges(t)
	newID := tf.state(t)["a"].ID
	if newID == id {
		t.Fatalf("replacement kept id %s", id)
	}
	a.allocationGone(t, id)
	c.gone(t, "allocation", id)
	got = a.allocation(t, newID)
	if got.GetStatus().GetDetails().GetAllocatedCidr() != "10.7.0.10/32" {
		t.Errorf("replacement allocated_cidr = %q, want 10.7.0.10/32", got.GetStatus().GetDetails().GetAllocatedCidr())
	}
	c.mustGet(t, "allocation", newID)

	tf.destroy(t)
	if st := tf.state(t); len(st) != 0 {
		t.Fatalf("state after destroy = %v, want empty", st)
	}
	a.allocationGone(t, newID)
	c.gone(t, "allocation", newID)
}

// TestV1alpha1PublicAllocation creates a public allocation with the empty
// ipv4_public object, which allocates a random address from the public pool.
func TestV1alpha1PublicAllocation(t *testing.T) {
	inst, c, tf := startVPC(t)
	a := newAlphaClients(t, inst)

	tf.writeConfig(t, fmt.Sprintf(`
resource "nebius_vpc_v1alpha1_allocation" "a" {
  parent_id   = %q
  name        = "alpha-public"
  ipv4_public = {}
}
`, inst.ProjectID))
	tf.init(t)
	tf.expectChanges(t, nil, map[string]string{"a": "create"})
	tf.apply(t)
	tf.expectNoChanges(t)

	id := tf.state(t)["a"].ID
	got := a.allocation(t, id)
	if got.GetSpec().GetIpv4Public() == nil {
		t.Errorf("spec = %v, want ipv4_public", got.GetSpec())
	}
	if pre, err := netip.ParsePrefix(got.GetStatus().GetDetails().GetAllocatedCidr()); err != nil || !netip.MustParsePrefix("203.0.113.0/24").Contains(pre.Addr()) {
		t.Errorf("allocated_cidr = %q (%v), want inside 203.0.113.0/24", got.GetStatus().GetDetails().GetAllocatedCidr(), err)
	}
	c.mustGet(t, "allocation", id)

	tf.destroy(t)
	a.allocationGone(t, id)
}

// TestV1alpha1ViewerCannotCreate checks authorization: the seeded viewer token
// is denied by the v1alpha1 AllocationService.Create.
func TestV1alpha1ViewerCannotCreate(t *testing.T) {
	inst, _, _ := startVPC(t)
	tf := newTerraformAs(t, inst, config.DefaultViewerToken)
	tf.writeConfig(t, fmt.Sprintf(`
resource "nebius_vpc_v1alpha1_allocation" "a" {
  parent_id   = %q
  name        = "alpha-viewer"
  ipv4_public = {}
}
`, inst.ProjectID))
	tf.init(t)
	if err := tf.tf.Apply(t.Context()); err == nil {
		t.Fatal("terraform apply as viewer succeeded, want PermissionDenied")
	}
	out := strings.ToLower(tf.out.String())
	if !strings.Contains(out, "permissiondenied") && !strings.Contains(out, "permission denied") && !strings.Contains(out, "permission_denied") {
		t.Errorf("apply output does not mention PermissionDenied:\n%s", tf.out.String())
	}
	a := newAlphaClients(t, inst)
	if _, err := a.allocs.GetByName(t.Context(), &vpcv1alpha1pb.GetAllocationByNameRequest{ParentId: inst.ProjectID, Name: "alpha-viewer"}); status.Code(err) != codes.NotFound {
		t.Errorf("allocation exists after denied apply: %v", err)
	}
}
