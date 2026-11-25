//go:build e2e

package vpc

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const netUsesPool = `ipv4_private_pools = { pools = [{ id = nebius_vpc_v1_pool.pool.id }] }`

func init() {
	pieceHCL["subb"] = `
resource "nebius_vpc_v1_subnet" "subb" {
  parent_id  = "{{.proj}}"
  name       = "e2e-subb"
  network_id = nebius_vpc_v1_network.net.id
  {{.subb_rt}}
  ipv4_private_pools = {
    pools = [{ cidrs = [{ cidr = cidrsubnet(nebius_vpc_v1_pool.pool.status.cidrs[0], 8, 5) }] }]
  }
  depends_on = [nebius_vpc_v1_subnet.sub]
}
`
	pieceHCL["puballoc"] = `
resource "nebius_vpc_v1_allocation" "puballoc" {
  parent_id    = "{{.proj}}"
  name         = "e2e-public-ip"
  ipv4_public  = { subnet_id = nebius_vpc_v1_subnet.sub.id }
}
`
	defaults["subb_rt"] = ""
}

// startVPC starts the mock with IAM and VPC under enforced authentication and
// authorization, returning the instance, the SDK clients and Terraform.
func startVPC(t *testing.T, opts ...testkit.Option) (*testkit.Instance, *clients, *tfRun) {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce)}, opts...)
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New()}, mockerOptions(opts...)...)
	return inst, newClients(t, inst), newTerraform(t, inst)
}

// rootPoolID returns the ID of the seeded default-network-pool.
func rootPoolID(t *testing.T, c *clients, inst *testkit.Instance) string {
	t.Helper()
	p, err := c.pools.GetByName(t.Context(), &vpcpb.GetPoolByNameRequest{ParentId: inst.ProjectID, Name: "default-network-pool"})
	if err != nil {
		t.Fatalf("GetByName default-network-pool: %v", err)
	}
	return p.GetMetadata().GetId()
}

// TestVPCLifecycle builds one realistic VPC stage by stage (pool, network,
// subnets, allocations, route table and route, security group and rule),
// checking the plan, Terraform state and the mock after every stage, then
// destroys everything in Terraform's dependency order.
func TestVPCLifecycle(t *testing.T) {
	// A non-zero delay makes every operation asynchronous, so the provider's
	// operation polling path is exercised, not just synchronous replies.
	inst, c, tf := startVPC(t, testkit.WithOperationDelay(300*time.Millisecond))
	s := stack{project: inst.ProjectID}.with("pool_source", rootPoolID(t, c, inst), "net_pools", netUsesPool)

	type stage struct {
		pieces  []string
		subbRT  string
		changed map[string]string
	}
	stages := []stage{
		{[]string{"pool", "net"}, "", map[string]string{"pool": "create", "net": "create"}},
		{[]string{"pool", "net", "sub", "subb"}, "", map[string]string{"sub": "create", "subb": "create"}},
		{[]string{"pool", "net", "sub", "subb", "alloc", "puballoc"}, "", map[string]string{"alloc": "create", "puballoc": "create"}},
		{
			[]string{"pool", "net", "sub", "subb", "alloc", "puballoc", "rt", "route"},
			"route_table_id = nebius_vpc_v1_route_table.rt.id",
			map[string]string{"rt": "create", "route": "create", "subb": "update"},
		},
		{
			[]string{"pool", "net", "sub", "subb", "alloc", "puballoc", "rt", "route", "sg", "rule"},
			"route_table_id = nebius_vpc_v1_route_table.rt.id",
			map[string]string{"sg": "create", "rule": "create"},
		},
	}
	var prev []string
	for i, st := range stages {
		tf.writeConfig(t, s.with("subb_rt", st.subbRT).render(t, st.pieces...))
		if i == 0 {
			tf.init(t)
		}
		tf.expectChanges(t, prev, st.changed)
		tf.apply(t)
		c.matchState(t, tf.state(t))
		tf.expectNoChanges(t)
		prev = st.pieces
	}

	st := tf.state(t)
	assertLifecycle(t, c, st)
	netID := st["net"].ID

	tf.destroy(t)
	if got := tf.state(t); len(got) != 0 {
		t.Fatalf("state after destroy = %v, want empty", got)
	}
	for _, r := range st {
		c.gone(t, r.Kind, r.ID)
	}
	assertDefaultsSurvive(t, c, inst)
	assertNoChildren(t, c, inst, netID)
}

// assertLifecycle checks the mock's view of the fully built stack.
func assertLifecycle(t *testing.T, c *clients, st map[string]stateRes) {
	t.Helper()
	ctx := t.Context()
	pool, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: st["pool"].ID})
	if err != nil {
		t.Fatalf("Get pool: %v", err)
	}
	if got := pool.GetSpec().GetCidrs(); len(got) != 1 || got[0].GetCidr() != "/16" {
		t.Errorf("pool spec cidrs = %v, want the user's /16 kept", got)
	}
	if len(pool.GetStatus().GetCidrs()) != 1 {
		t.Fatalf("pool status cidrs = %v, want one resolved block", pool.GetStatus().GetCidrs())
	}
	block := netip.MustParsePrefix(pool.GetStatus().GetCidrs()[0])
	if block.Bits() != 16 || !netip.MustParsePrefix("10.0.0.0/13").Contains(block.Addr()) {
		t.Errorf("pool status cidr = %s, want a /16 inside 10.0.0.0/13", block)
	}

	subA, err := c.subs.Get(ctx, &vpcpb.GetSubnetRequest{Id: st["sub"].ID})
	if err != nil {
		t.Fatalf("Get subnet A: %v", err)
	}
	subB, err := c.subs.Get(ctx, &vpcpb.GetSubnetRequest{Id: st["subb"].ID})
	if err != nil {
		t.Fatalf("Get subnet B: %v", err)
	}
	cidrA := subnetCIDRs(subA)
	cidrB := subnetCIDRs(subB)
	if len(cidrA) != 1 || len(cidrB) != 1 {
		t.Fatalf("subnet cidrs A=%v B=%v, want one each", cidrA, cidrB)
	}
	if cidrA[0].Bits() != 24 || !block.Contains(cidrA[0].Addr()) {
		t.Errorf("subnet A cidr = %s, want a /24 inside %s", cidrA[0], block)
	}
	if cidrB[0].Bits() != 24 || !block.Contains(cidrB[0].Addr()) || cidrA[0] == cidrB[0] {
		t.Errorf("subnet B cidr = %s, want a /24 inside %s distinct from A (%s)", cidrB[0], block, cidrA[0])
	}
	if got := subB.GetStatus().GetRouteTable().GetId(); got != st["rt"].ID || subB.GetStatus().GetRouteTable().GetDefault() {
		t.Errorf("subnet B status.route_table = %v, want custom %s", subB.GetStatus().GetRouteTable(), st["rt"].ID)
	}
	net, err := c.nets.Get(ctx, &vpcpb.GetNetworkRequest{Id: st["net"].ID})
	if err != nil {
		t.Fatalf("Get network: %v", err)
	}
	if got := subA.GetStatus().GetRouteTable().GetId(); got != net.GetStatus().GetDefaultRouteTableId() || !subA.GetStatus().GetRouteTable().GetDefault() {
		t.Errorf("subnet A status.route_table = %v, want the network default %s", subA.GetStatus().GetRouteTable(), net.GetStatus().GetDefaultRouteTableId())
	}

	alloc, err := c.allocs.Get(ctx, &vpcpb.GetAllocationRequest{Id: st["alloc"].ID})
	if err != nil {
		t.Fatalf("Get allocation: %v", err)
	}
	ip, err := netip.ParsePrefix(alloc.GetStatus().GetDetails().GetAllocatedCidr())
	if err != nil || ip.Bits() != 32 || !cidrA[0].Contains(ip.Addr()) {
		t.Errorf("allocation cidr = %q (%v), want a /32 inside subnet A %s", alloc.GetStatus().GetDetails().GetAllocatedCidr(), err, cidrA[0])
	}
	if alloc.GetStatus().GetDetails().GetSubnetId() != st["sub"].ID {
		t.Errorf("allocation subnet = %q, want %s", alloc.GetStatus().GetDetails().GetSubnetId(), st["sub"].ID)
	}
	pub, err := c.allocs.Get(ctx, &vpcpb.GetAllocationRequest{Id: st["puballoc"].ID})
	if err != nil {
		t.Fatalf("Get public allocation: %v", err)
	}
	if p, err := netip.ParsePrefix(pub.GetStatus().GetDetails().GetAllocatedCidr()); err != nil || !netip.MustParsePrefix("203.0.113.0/24").Contains(p.Addr()) {
		t.Errorf("public allocation cidr = %q (%v), want inside 203.0.113.0/24", pub.GetStatus().GetDetails().GetAllocatedCidr(), err)
	}

	route, err := c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: st["route"].ID})
	if err != nil {
		t.Fatalf("Get route: %v", err)
	}
	if route.GetMetadata().GetParentId() != st["rt"].ID || route.GetSpec().GetDestination().GetCidr() != "192.168.0.0/16" ||
		route.GetSpec().GetNextHop().GetAllocation().GetId() != st["alloc"].ID {
		t.Errorf("route = %v, want 192.168.0.0/16 via allocation %s in %s", route, st["alloc"].ID, st["rt"].ID)
	}
	rule, err := c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: st["rule"].ID})
	if err != nil {
		t.Fatalf("Get security rule: %v", err)
	}
	if rule.GetMetadata().GetParentId() != st["sg"].ID || rule.GetSpec().GetProtocol() != vpcpb.RuleProtocol_TCP ||
		!slices.Equal(rule.GetSpec().GetIngress().GetSourceCidrs(), []string{"0.0.0.0/0"}) ||
		!slices.Equal(rule.GetSpec().GetIngress().GetDestinationPorts(), []int32{443}) {
		t.Errorf("security rule = %v, want TCP 443 from 0.0.0.0/0 in %s", rule, st["sg"].ID)
	}
	if got := rule.GetSpec().GetPriority(); got != 100 {
		t.Errorf("security rule priority = %d, want 100", got)
	}
}

func subnetCIDRs(s *vpcpb.Subnet) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range s.GetStatus().GetIpv4PrivatePools() {
		for _, c := range p.GetCidrs() {
			if pf, err := netip.ParsePrefix(c); err == nil {
				out = append(out, pf)
			}
		}
	}
	return out
}

// assertDefaultsSurvive checks that destroying the stack left the seeded
// defaults alone.
func assertDefaultsSurvive(t *testing.T, c *clients, inst *testkit.Instance) {
	t.Helper()
	ctx := t.Context()
	if _, err := c.nets.GetByName(ctx, &vpcpb.GetNetworkByNameRequest{ParentId: inst.ProjectID, Name: "default-network"}); err != nil {
		t.Errorf("default-network after destroy: %v", err)
	}
	if _, err := c.subs.GetByName(ctx, &vpcpb.GetSubnetByNameRequest{ParentId: inst.ProjectID, Name: "default-subnet"}); err != nil {
		t.Errorf("default-subnet after destroy: %v", err)
	}
	for _, n := range []string{"default-network-pool", "default-public-pool"} {
		if _, err := c.pools.GetByName(ctx, &vpcpb.GetPoolByNameRequest{ParentId: inst.ProjectID, Name: n}); err != nil {
			t.Errorf("%s after destroy: %v", n, err)
		}
	}
}

// assertNoChildren checks that the default route table and security group of
// the destroyed network went with it.
func assertNoChildren(t *testing.T, c *clients, inst *testkit.Instance, networkID string) {
	t.Helper()
	ctx := t.Context()
	rts, err := c.rts.List(ctx, &vpcpb.ListRouteTablesRequest{ParentId: inst.ProjectID})
	if err != nil {
		t.Fatalf("List route tables: %v", err)
	}
	for _, rt := range rts.GetItems() {
		if rt.GetSpec().GetNetworkId() == networkID {
			t.Errorf("route table %s of the destroyed network still exists", rt.GetMetadata().GetName())
		}
	}
	sgs, err := c.sgs.List(ctx, &vpcpb.ListSecurityGroupsRequest{ParentId: inst.ProjectID})
	if err != nil {
		t.Fatalf("List security groups: %v", err)
	}
	for _, sg := range sgs.GetItems() {
		if sg.GetSpec().GetNetworkId() == networkID {
			t.Errorf("security group %s of the destroyed network still exists", sg.GetMetadata().GetName())
		}
	}
}
