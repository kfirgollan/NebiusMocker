package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// delOK deletes through f and fails the test if the delete is refused.
func delOK(t *testing.T, what string, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Fatalf("delete %s: %v", what, err)
	}
}

func (c vpcClients) delPool(t *testing.T, id string) error {
	t.Helper()
	return waitOK(t.Context())(c.pools.Delete(t.Context(), &vpcpb.DeletePoolRequest{Id: id}))
}

func (c vpcClients) delNetwork(t *testing.T, id string) error {
	t.Helper()
	return waitOK(t.Context())(c.networks.Delete(t.Context(), &vpcpb.DeleteNetworkRequest{Id: id}))
}

func (c vpcClients) delSubnet(t *testing.T, id string) error {
	t.Helper()
	return waitOK(t.Context())(c.subnets.Delete(t.Context(), &vpcpb.DeleteSubnetRequest{Id: id}))
}

func (c vpcClients) delAlloc(t *testing.T, id string) error {
	t.Helper()
	return waitOK(t.Context())(c.allocations.Delete(t.Context(), &vpcpb.DeleteAllocationRequest{Id: id}))
}

func (c vpcClients) delRouteTable(t *testing.T, id string) error {
	t.Helper()
	return waitOK(t.Context())(c.routeTables.Delete(t.Context(), &vpcpb.DeleteRouteTableRequest{Id: id}))
}

func (c vpcClients) delRoute(t *testing.T, id string) error {
	t.Helper()
	return waitOK(t.Context())(c.routes.Delete(t.Context(), &vpcpb.DeleteRouteRequest{Id: id}))
}

func (c vpcClients) delGroup(t *testing.T, id string) error {
	t.Helper()
	return waitOK(t.Context())(c.groups.Delete(t.Context(), &vpcpb.DeleteSecurityGroupRequest{Id: id}))
}

func (c vpcClients) delRule(t *testing.T, id string) error {
	t.Helper()
	return waitOK(t.Context())(c.rules.Delete(t.Context(), &vpcpb.DeleteSecurityRuleRequest{Id: id}))
}

func TestDeletePoolInUse(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	def := seedPoolID(t, c, seedPrivatePool)

	parent := newPrivatePool(t, c, "parent", def, "/16")
	child := newPrivatePool(t, c, "child", parent, "/20")
	assertCode(t, c.delPool(t, parent), codes.FailedPrecondition) // child pool
	delOK(t, "child pool", func() error { return c.delPool(t, child) })

	netID := newNetwork(t, c, "net", parent)
	assertCode(t, c.delPool(t, parent), codes.FailedPrecondition) // network
	sn := newSubnet(t, c, "sn", netID, "/24")

	p, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: parent})
	if err != nil {
		t.Fatal(err)
	}
	asg := p.GetStatus().GetAssignment()
	if len(asg.GetNetworks()) != 1 || asg.GetNetworks()[0] != netID || len(asg.GetSubnets()) != 1 || asg.GetSubnets()[0] != sn {
		t.Errorf("pool assignment = %v, want network %s and subnet %s", asg, netID, sn)
	}

	delOK(t, "subnet", func() error { return c.delSubnet(t, sn) })
	assertCode(t, c.delPool(t, parent), codes.FailedPrecondition) // still the network's pool
	delOK(t, "network", func() error { return c.delNetwork(t, netID) })

	// An allocation made straight from the pool pins it too.
	a := newAlloc(t, c, "alloc", privAllocSpec("", parent, ""))
	assertCode(t, c.delPool(t, parent), codes.FailedPrecondition)
	delOK(t, "allocation", func() error { return c.delAlloc(t, a) })

	delOK(t, "pool", func() error { return c.delPool(t, parent) })
	if _, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: parent}); codeOf(err) != codes.NotFound {
		t.Errorf("Get deleted pool code = %v, want NotFound", codeOf(err))
	}
	// The space is free again.
	again := newPrivatePool(t, c, "again", def, "/16")
	if got, want := poolCIDR(t, c, again).String(), nthBlock(t, mustPrefix(t, seedPrivateCIDR), 16, 0); got != want {
		t.Errorf("block after delete = %s, want %s", got, want)
	}
}

func TestDeleteNetworkInUse(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	netID := newNetwork(t, c, "net")
	n, err := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: netID})
	if err != nil {
		t.Fatal(err)
	}
	defRT := n.GetStatus().GetDefaultRouteTableId()
	routes, err := c.routes.List(ctx, &vpcpb.ListRoutesRequest{ParentId: defRT})
	if err != nil || len(routes.GetItems()) != 1 {
		t.Fatalf("default routes = %v, err %v", routes.GetItems(), err)
	}
	defRoute := routes.GetItems()[0].GetMetadata().GetId()
	groups, err := c.groups.ListByNetwork(ctx, &vpcpb.ListSecurityGroupsByNetworkRequest{NetworkId: netID})
	if err != nil || len(groups.GetItems()) != 1 {
		t.Fatalf("default groups = %v, err %v", groups.GetItems(), err)
	}
	defSG := groups.GetItems()[0].GetMetadata().GetId()
	defRule := newRule(t, c, defSG, "in-ssh", ingressRule([]string{"10.0.0.0/8"}, 22))

	sn := newSubnet(t, c, "sn", netID, "/24")
	assertCode(t, c.delNetwork(t, netID), codes.FailedPrecondition)
	delOK(t, "subnet", func() error { return c.delSubnet(t, sn) })

	rt := newRouteTable(t, c, "rt", netID)
	assertCode(t, c.delNetwork(t, netID), codes.FailedPrecondition)
	delOK(t, "route table", func() error { return c.delRouteTable(t, rt) })

	sg := newGroup(t, c, "sg", netID)
	assertCode(t, c.delNetwork(t, netID), codes.FailedPrecondition)
	delOK(t, "security group", func() error { return c.delGroup(t, sg) })

	// Only the defaults are left: they go with the network.
	delOK(t, "network", func() error { return c.delNetwork(t, netID) })
	if _, err := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: netID}); codeOf(err) != codes.NotFound {
		t.Errorf("Get deleted network code = %v, want NotFound", codeOf(err))
	}
	if _, err := c.routeTables.Get(ctx, &vpcpb.GetRouteTableRequest{Id: defRT}); codeOf(err) != codes.NotFound {
		t.Errorf("default route table survived the network: %v", err)
	}
	if _, err := c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: defRoute}); codeOf(err) != codes.NotFound {
		t.Errorf("default route survived the network: %v", err)
	}
	if _, err := c.groups.Get(ctx, &vpcpb.GetSecurityGroupRequest{Id: defSG}); codeOf(err) != codes.NotFound {
		t.Errorf("default security group survived the network: %v", err)
	}
	if _, err := c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: defRule}); codeOf(err) != codes.NotFound {
		t.Errorf("default group's rule survived the network: %v", err)
	}
	// Nothing of the network is listed any more.
	rts, err := c.routeTables.ListByNetwork(ctx, &vpcpb.ListRouteTablesByNetworkRequest{NetworkId: netID})
	if err != nil || len(rts.GetItems()) != 0 {
		t.Errorf("route tables of the deleted network = %v, err %v", rts.GetItems(), err)
	}
}

func TestDeleteSubnetAndAllocation(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	_, netID := netWithPool(t, c, "net", "/16")
	sn := newSubnet(t, c, "sn", netID, "/24")
	a := newAlloc(t, c, "alloc-1", privAllocSpec(sn, "", ""))

	assertCode(t, c.delSubnet(t, sn), codes.FailedPrecondition) // holds an allocation

	rt := newRouteTable(t, c, "rt", netID)
	route := newRoute(t, c, rt, "via-a", "192.168.0.0/16", allocHop(a))
	assertCode(t, c.delAlloc(t, a), codes.FailedPrecondition) // next hop of a route
	delOK(t, "route", func() error { return c.delRoute(t, route) })
	delOK(t, "allocation", func() error { return c.delAlloc(t, a) })
	delOK(t, "subnet", func() error { return c.delSubnet(t, sn) })
	if _, err := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: sn}); codeOf(err) != codes.NotFound {
		t.Errorf("Get deleted subnet code = %v, want NotFound", codeOf(err))
	}
}

func TestDeleteRouteTableAssigned(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	_, netID := netWithPool(t, c, "net", "/16")
	rt := newRouteTable(t, c, "rt", netID)
	other := newRouteTable(t, c, "other", netID)
	route := newRoute(t, c, rt, "default", "0.0.0.0/0", egressHop())

	spec := subnetSpec(netID, "/24")
	spec.RouteTableId = rt
	sn := wait(t)(c.subnets.Create(ctx, createSubnetReq(c, "sn", spec)))

	got, err := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: sn})
	if err != nil {
		t.Fatal(err)
	}
	if st := got.GetStatus().GetRouteTable(); st.GetId() != rt || st.GetDefault() {
		t.Errorf("subnet route table = %v, want %s (not default)", st, rt)
	}
	r, err := c.routeTables.Get(ctx, &vpcpb.GetRouteTableRequest{Id: rt})
	if err != nil {
		t.Fatal(err)
	}
	if asg := r.GetStatus().GetAssignment().GetSubnets(); len(asg) != 1 || asg[0] != sn {
		t.Errorf("route table assignment = %v, want [%s]", asg, sn)
	}
	assertCode(t, c.delRouteTable(t, rt), codes.FailedPrecondition)

	// Move the subnet to another route table, then the first one can go.
	got.GetSpec().RouteTableId = other
	delOK(t, "reassign", func() error {
		return waitOK(ctx)(c.subnets.Update(ctx, &vpcpb.UpdateSubnetRequest{Metadata: got.GetMetadata(), Spec: got.GetSpec()}))
	})
	moved, err := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: sn})
	if err != nil {
		t.Fatal(err)
	}
	if moved.GetStatus().GetRouteTable().GetId() != other || moved.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("after reassignment route table %v version %d, want %s version 2", moved.GetStatus().GetRouteTable(), moved.GetMetadata().GetResourceVersion(), other)
	}
	assertCode(t, c.delRouteTable(t, other), codes.FailedPrecondition)
	delOK(t, "route table", func() error { return c.delRouteTable(t, rt) })
	// The route went with its table.
	if _, err := c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: route}); codeOf(err) != codes.NotFound {
		t.Errorf("route survived its route table: %v", err)
	}

	delOK(t, "subnet", func() error { return c.delSubnet(t, sn) })
	delOK(t, "other route table", func() error { return c.delRouteTable(t, other) })
}

func TestDeleteDefaults(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	def, err := c.networks.GetDefault(ctx, &vpcpb.GetDefaultNetworkRequest{ParentId: c.inst.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	netID := def.GetNetwork().GetMetadata().GetId()

	assertCode(t, c.delRouteTable(t, def.GetNetwork().GetStatus().GetDefaultRouteTableId()), codes.FailedPrecondition)
	groups, err := c.groups.ListByNetwork(ctx, &vpcpb.ListSecurityGroupsByNetworkRequest{NetworkId: netID})
	if err != nil || len(groups.GetItems()) != 1 {
		t.Fatalf("default groups = %v, err %v", groups.GetItems(), err)
	}
	assertCode(t, c.delGroup(t, groups.GetItems()[0].GetMetadata().GetId()), codes.FailedPrecondition)

	pub := seedPoolID(t, c, seedPublicPool)
	assertCode(t, c.delPool(t, pub), codes.FailedPrecondition)
	assertCode(t, c.delPool(t, seedPoolID(t, c, seedPrivatePool)), codes.FailedPrecondition) // used by the default network
	assertCode(t, c.delNetwork(t, netID), codes.FailedPrecondition)                          // has the default subnet

	// The default public pool is frozen even without users.
	p, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: pub})
	if err != nil {
		t.Fatal(err)
	}
	p.GetSpec().Cidrs = append(p.GetSpec().Cidrs, &vpcpb.PoolCidr{Cidr: "198.51.100.0/24"})
	err = waitOK(ctx)(c.pools.Update(ctx, &vpcpb.UpdatePoolRequest{Metadata: p.GetMetadata(), Spec: p.GetSpec()}))
	assertCode(t, err, codes.FailedPrecondition)
}

func TestDeleteSecurityGroupReferenced(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	netID := newNetwork(t, c, "net")
	other := newNetwork(t, c, "other")
	web := newGroup(t, c, "web", netID)
	db := newGroup(t, c, "db", netID)
	foreign := newGroup(t, c, "foreign", other)

	fromWeb := ingressRule(nil, 5432)
	fromWeb.Match = &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceSecurityGroupId: web, DestinationPorts: []int32{5432}}}
	ingress := newRule(t, c, db, "from-web", fromWeb)
	toWeb := &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP,
		Match: &vpcpb.SecurityRuleSpec_Egress{Egress: &vpcpb.RuleEgress{DestinationSecurityGroupId: web}},
	}
	egress := newRule(t, c, db, "to-web", toWeb)

	assertCode(t, c.delGroup(t, web), codes.FailedPrecondition)
	delOK(t, "ingress rule", func() error { return c.delRule(t, ingress) })
	assertCode(t, c.delGroup(t, web), codes.FailedPrecondition) // the egress rule still points at it
	delOK(t, "egress rule", func() error { return c.delRule(t, egress) })
	delOK(t, "referenced group", func() error { return c.delGroup(t, web) })

	// References must resolve, and stay inside the network.
	bad := ingressRule(nil, 80)
	bad.Match = &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceSecurityGroupId: web, DestinationPorts: []int32{80}}}
	_, err := c.rules.Create(ctx, ruleReq(db, "dangling", bad))
	assertCode(t, err, codes.NotFound)
	bad.Match = &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceSecurityGroupId: foreign, DestinationPorts: []int32{80}}}
	_, err = c.rules.Create(ctx, ruleReq(db, "cross-network", bad))
	assertCode(t, err, codes.InvalidArgument)

	// A group with its own rules deletes them with it.
	own := newRule(t, c, db, "own", ingressRule([]string{"10.0.0.0/8"}, 22))
	delOK(t, "group with rules", func() error { return c.delGroup(t, db) })
	if _, err := c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: own}); codeOf(err) != codes.NotFound {
		t.Errorf("rule survived its group: %v", err)
	}
	// A rule cannot be added to a deleted group.
	_, err = c.rules.Create(ctx, ruleReq(db, "late", ingressRule([]string{"10.0.0.0/8"}, 22)))
	assertCode(t, err, codes.NotFound)
}
