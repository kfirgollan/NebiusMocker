package contract

import (
	"testing"

	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// vpcClients are gosdk clients for every VPC service.
type vpcClients struct {
	inst        *testkit.Instance
	pools       vpcsvc.PoolService
	networks    vpcsvc.NetworkService
	subnets     vpcsvc.SubnetService
	allocations vpcsvc.AllocationService
	routeTables vpcsvc.RouteTableService
	routes      vpcsvc.RouteService
	groups      vpcsvc.SecurityGroupService
	rules       vpcsvc.SecurityRuleService
}

func startVPC(t *testing.T, opts ...testkit.Option) vpcClients {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New()}, opts...)
	return vpcClientsFor(t, inst, inst.Token)
}

func vpcClientsFor(t *testing.T, inst *testkit.Instance, token string) vpcClients {
	t.Helper()
	sdk := inst.SDKAs(t, token)
	return vpcClients{
		inst:        inst,
		pools:       vpcsvc.NewPoolService(sdk),
		networks:    vpcsvc.NewNetworkService(sdk),
		subnets:     vpcsvc.NewSubnetService(sdk),
		allocations: vpcsvc.NewAllocationService(sdk),
		routeTables: vpcsvc.NewRouteTableService(sdk),
		routes:      vpcsvc.NewRouteService(sdk),
		groups:      vpcsvc.NewSecurityGroupService(sdk),
		rules:       vpcsvc.NewSecurityRuleService(sdk),
	}
}

// wait returns a function that waits for a started operation and returns its
// resource ID: wait(t)(svc.Create(...)).
func wait(t *testing.T) func(operations.Operation, error) string {
	t.Helper()
	return func(op operations.Operation, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("start operation: %v", err)
		}
		if _, err := op.Wait(t.Context()); err != nil {
			t.Fatalf("operation %s: %v", op.ID(), err)
		}
		return op.ResourceID()
	}
}

func md(parent, name string) *commonpb.ResourceMetadata {
	return &commonpb.ResourceMetadata{ParentId: parent, Name: name}
}

// TestVPCFlow walks the documented setup: pool → network → subnets →
// allocations → route table + route → security group + rule, then tears it
// down in dependency order.
func TestVPCFlow(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	project := c.inst.ProjectID

	def, err := c.networks.GetDefault(ctx, &vpcpb.GetDefaultNetworkRequest{ParentId: project})
	if err != nil {
		t.Fatalf("GetDefault: %v", err)
	}
	if def.GetNetwork().GetMetadata().GetName() != "default-network" || def.GetSubnet().GetMetadata().GetName() != "default-subnet" {
		t.Fatalf("default network = %v / %v", def.GetNetwork().GetMetadata(), def.GetSubnet().GetMetadata())
	}
	defPool, err := c.pools.GetByName(ctx, &vpcpb.GetPoolByNameRequest{ParentId: project, Name: "default-network-pool"})
	if err != nil {
		t.Fatalf("default pool: %v", err)
	}

	poolID := wait(t)(c.pools.Create(ctx, &vpcpb.CreatePoolRequest{Metadata: md(project, "app-pool"), Spec: &vpcpb.PoolSpec{
		SourcePoolId: defPool.GetMetadata().GetId(), Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: "/16"}},
	}}))
	pool, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: poolID})
	if err != nil {
		t.Fatalf("Get pool: %v", err)
	}
	t.Logf("pool cidrs %v", pool.GetStatus().GetCidrs())

	netID := wait(t)(c.networks.Create(ctx, &vpcpb.CreateNetworkRequest{Metadata: md(project, "app-net"), Spec: &vpcpb.NetworkSpec{
		Ipv4PrivatePools: &vpcpb.IPv4PrivateNetworkPools{Pools: []*vpcpb.NetworkPool{{Id: poolID}}},
	}}))
	subnetSpec := func(cidr string) *vpcpb.SubnetSpec {
		return &vpcpb.SubnetSpec{NetworkId: netID, Ipv4PrivatePools: &vpcpb.IPv4PrivateSubnetPools{
			Pools: []*vpcpb.SubnetPool{{Cidrs: []*vpcpb.SubnetCidr{{Cidr: cidr}}}},
		}}
	}
	autoID := wait(t)(c.subnets.Create(ctx, &vpcpb.CreateSubnetRequest{Metadata: md(project, "auto"), Spec: subnetSpec("/24")}))
	auto, err := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: autoID})
	if err != nil {
		t.Fatalf("Get subnet: %v", err)
	}
	t.Logf("auto subnet status %v", auto.GetStatus())
	explicitCIDR := "10.0.200.0/24"
	if p := pool.GetStatus().GetCidrs(); len(p) > 0 {
		explicitCIDR = p[0][:len(p[0])-len("0.0/16")] + "200.0/24"
	}
	wait(t)(c.subnets.Create(ctx, &vpcpb.CreateSubnetRequest{Metadata: md(project, "explicit"), Spec: subnetSpec(explicitCIDR)}))
	if _, err := c.subnets.Create(ctx, &vpcpb.CreateSubnetRequest{Metadata: md(project, "overlap"), Spec: subnetSpec(explicitCIDR)}); err == nil {
		t.Fatalf("overlapping subnet accepted")
	}

	privID := wait(t)(c.allocations.Create(ctx, &vpcpb.CreateAllocationRequest{Metadata: md(project, "priv"), Spec: &vpcpb.AllocationSpec{
		IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{Pool: &vpcpb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: autoID}}},
	}}))
	pubID := wait(t)(c.allocations.Create(ctx, &vpcpb.CreateAllocationRequest{Metadata: md(project, "pub"), Spec: &vpcpb.AllocationSpec{
		IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{Pool: &vpcpb.IPv4PublicAllocationSpec_SubnetId{SubnetId: autoID}}},
	}}))
	for _, id := range []string{privID, pubID} {
		a, err := c.allocations.Get(ctx, &vpcpb.GetAllocationRequest{Id: id})
		if err != nil {
			t.Fatalf("Get allocation: %v", err)
		}
		t.Logf("allocation %s: %v", a.GetMetadata().GetName(), a.GetStatus())
	}

	rtID := wait(t)(c.routeTables.Create(ctx, &vpcpb.CreateRouteTableRequest{Metadata: md(project, "rt"), Spec: &vpcpb.RouteTableSpec{NetworkId: netID}}))
	routeID := wait(t)(c.routes.Create(ctx, &vpcpb.CreateRouteRequest{Metadata: md(rtID, "to-nat"), Spec: &vpcpb.RouteSpec{
		Destination: &vpcpb.DestinationMatch{Cidr: "192.168.0.0/16"},
		NextHop:     &vpcpb.NextHop{NextHop: &vpcpb.NextHop_Allocation{Allocation: &vpcpb.AllocationNextHop{Id: privID}}},
	}}))
	sgID := wait(t)(c.groups.Create(ctx, &vpcpb.CreateSecurityGroupRequest{Metadata: md(project, "web"), Spec: &vpcpb.SecurityGroupSpec{NetworkId: netID}}))
	ruleID := wait(t)(c.rules.Create(ctx, &vpcpb.CreateSecurityRuleRequest{Metadata: md(sgID, "https"), Spec: &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP,
		Match: &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceCidrs: []string{"0.0.0.0/0"}, DestinationPorts: []int32{443}}},
	}}))
	rule, err := c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: ruleID})
	if err != nil {
		t.Fatalf("Get rule: %v", err)
	}
	t.Logf("rule %v", rule)

	if _, err := c.allocations.Delete(ctx, &vpcpb.DeleteAllocationRequest{Id: privID}); err == nil {
		t.Fatalf("deleting an allocation used as a next hop succeeded")
	}
	wait(t)(c.rules.Delete(ctx, &vpcpb.DeleteSecurityRuleRequest{Id: ruleID}))
	wait(t)(c.groups.Delete(ctx, &vpcpb.DeleteSecurityGroupRequest{Id: sgID}))
	wait(t)(c.routes.Delete(ctx, &vpcpb.DeleteRouteRequest{Id: routeID}))
	wait(t)(c.routeTables.Delete(ctx, &vpcpb.DeleteRouteTableRequest{Id: rtID}))
	wait(t)(c.allocations.Delete(ctx, &vpcpb.DeleteAllocationRequest{Id: privID}))
	wait(t)(c.allocations.Delete(ctx, &vpcpb.DeleteAllocationRequest{Id: pubID}))
	if _, err := c.networks.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: netID}); err == nil {
		t.Fatalf("deleting a network with subnets succeeded")
	}
	subs, err := c.subnets.ListByNetwork(ctx, &vpcpb.ListSubnetsByNetworkRequest{NetworkId: netID})
	if err != nil {
		t.Fatalf("ListByNetwork: %v", err)
	}
	for _, s := range subs.GetItems() {
		wait(t)(c.subnets.Delete(ctx, &vpcpb.DeleteSubnetRequest{Id: s.GetMetadata().GetId()}))
	}
	wait(t)(c.networks.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: netID}))
	wait(t)(c.pools.Delete(ctx, &vpcpb.DeletePoolRequest{Id: poolID}))
}
