package contract

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// TestImmutableFields tries to change every immutable field in an update.
func TestImmutableFields(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	def := seedPoolID(t, c, seedPrivatePool)
	pubDef := seedPoolID(t, c, seedPublicPool)

	srcA := newPrivatePool(t, c, "src-a", def, "/16")
	srcB := newPrivatePool(t, c, "src-b", def, "/16")
	poolID := newPrivatePool(t, c, "mut-pool", srcA, "/24")
	netID := newNetwork(t, c, "mut-net", srcA)
	otherNet := newNetwork(t, c, "mut-other")
	sn := newSubnet(t, c, "mut-sn", netID, "/28")
	alloc := newAlloc(t, c, "mut-alloc", privAllocSpec(sn, "", ""))
	rt := newRouteTable(t, c, "mut-rt", netID)
	route := newRoute(t, c, rt, "mut-route", "192.168.0.0/16", allocHop(alloc))
	sg := newGroup(t, c, "mut-sg", netID)
	rule := newRule(t, c, sg, "mut-rule", ingressRule([]string{"10.0.0.0/8"}, 22))

	meta := func(id, parent, name string) *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name}
	}
	tests := []struct {
		name string
		do   func() error
	}{
		{"pool source", func() error {
			p, _ := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: poolID})
			p.Spec.SourcePoolId = srcB
			return waitOK(ctx)(c.pools.Update(ctx, &vpcpb.UpdatePoolRequest{Metadata: meta(poolID, project, "mut-pool"), Spec: p.GetSpec()}))
		}},
		{"pool source cleared by reset mask", func() error {
			p, _ := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: poolID})
			p.Spec.SourcePoolId = ""
			p.Spec.Cidrs = []*vpcpb.PoolCidr{{Cidr: "10.0.0.0/24"}}
			return waitOK(ctx)(c.pools.Update(resetCtx(ctx, t, "spec.source_pool_id"), &vpcpb.UpdatePoolRequest{Metadata: meta(poolID, project, "mut-pool"), Spec: p.GetSpec()}))
		}},
		{"pool visibility", func() error {
			p, _ := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: poolID})
			p.Spec.Visibility = vpcpb.IpVisibility_PUBLIC
			return waitOK(ctx)(c.pools.Update(ctx, &vpcpb.UpdatePoolRequest{Metadata: meta(poolID, project, "mut-pool"), Spec: p.GetSpec()}))
		}},
		{"subnet network", func() error {
			s, _ := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: sn})
			s.Spec.NetworkId = otherNet
			return waitOK(ctx)(c.subnets.Update(ctx, &vpcpb.UpdateSubnetRequest{Metadata: meta(sn, project, "mut-sn"), Spec: s.GetSpec()}))
		}},
		{"allocation spec", func() error {
			return waitOK(ctx)(c.allocations.Update(ctx, &vpcpb.UpdateAllocationRequest{
				Metadata: meta(alloc, project, "mut-alloc"), Spec: pubAllocSpec("", pubDef, ""),
			}))
		}},
		{"allocation cidr", func() error {
			a, _ := c.allocations.Get(ctx, &vpcpb.GetAllocationRequest{Id: alloc})
			a.Spec.GetIpv4Private().Cidr = "10.9.9.9/32"
			return waitOK(ctx)(c.allocations.Update(ctx, &vpcpb.UpdateAllocationRequest{Metadata: meta(alloc, project, "mut-alloc"), Spec: a.GetSpec()}))
		}},
		{"route table network", func() error {
			return waitOK(ctx)(c.routeTables.Update(ctx, &vpcpb.UpdateRouteTableRequest{
				Metadata: meta(rt, project, "mut-rt"), Spec: &vpcpb.RouteTableSpec{NetworkId: otherNet},
			}))
		}},
		{"route destination", func() error {
			return waitOK(ctx)(c.routes.Update(ctx, &vpcpb.UpdateRouteRequest{
				Metadata: meta(route, rt, "mut-route"),
				Spec:     &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "172.16.0.0/12"}, NextHop: allocHop(alloc)},
			}))
		}},
		{"route next hop", func() error {
			return waitOK(ctx)(c.routes.Update(ctx, &vpcpb.UpdateRouteRequest{
				Metadata: meta(route, rt, "mut-route"),
				Spec:     &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "192.168.0.0/16"}, NextHop: egressHop()},
			}))
		}},
		{"security group network", func() error {
			return waitOK(ctx)(c.groups.Update(ctx, &vpcpb.UpdateSecurityGroupRequest{
				Metadata: meta(sg, project, "mut-sg"), Spec: &vpcpb.SecurityGroupSpec{NetworkId: otherNet},
			}))
		}},
		{"rule ports", func() error {
			return waitOK(ctx)(c.rules.Update(ctx, &vpcpb.UpdateSecurityRuleRequest{
				Metadata: meta(rule, sg, "mut-rule"), Spec: ingressRule([]string{"10.0.0.0/8"}, 23),
			}))
		}},
		{"rule access", func() error {
			spec := ingressRule([]string{"10.0.0.0/8"}, 22)
			spec.Access = vpcpb.RuleAccessAction_DENY
			return waitOK(ctx)(c.rules.Update(ctx, &vpcpb.UpdateSecurityRuleRequest{Metadata: meta(rule, sg, "mut-rule"), Spec: spec}))
		}},
		{"rule priority", func() error {
			spec := ingressRule([]string{"10.0.0.0/8"}, 22)
			spec.Priority = 10
			return waitOK(ctx)(c.rules.Update(ctx, &vpcpb.UpdateSecurityRuleRequest{Metadata: meta(rule, sg, "mut-rule"), Spec: spec}))
		}},
		{"metadata parent", func() error {
			return waitOK(ctx)(c.networks.Update(ctx, &vpcpb.UpdateNetworkRequest{
				Metadata: meta(netID, "project-e00elsewhere", "mut-net"), Spec: &vpcpb.NetworkSpec{},
			}))
		}},
	}
	for _, tt := range tests {
		if got := codeOf(tt.do()); got != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", tt.name, got)
		}
	}

	// Nothing above changed anything.
	for what, v := range map[string]int64{
		"pool":       getPool(t, c, poolID).GetMetadata().GetResourceVersion(),
		"subnet":     getSubnet(t, c, sn).GetMetadata().GetResourceVersion(),
		"allocation": getAllocation(t, c, alloc).GetMetadata().GetResourceVersion(),
		"route":      getRoute(t, c, route).GetMetadata().GetResourceVersion(),
		"rule":       getSecurityRule(t, c, rule).GetMetadata().GetResourceVersion(),
	} {
		if v != 1 {
			t.Errorf("%s resource_version = %d after rejected updates, want 1", what, v)
		}
	}
}

func TestMutableSpecFields(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	_, netID := netWithPool(t, c, "mutable", "/16")
	sn := newSubnet(t, c, "mut-sn", netID, "/28")
	alloc := newAlloc(t, c, "mut-alloc", privAllocSpec(sn, "", ""))
	rt := newRouteTable(t, c, "mut-rt", netID)
	route := newRoute(t, c, rt, "mut-route", "192.168.0.0/16", allocHop(alloc))

	// A route's description may change; its match and next hop may not.
	r, err := c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: route})
	if err != nil {
		t.Fatal(err)
	}
	r.Spec.Description = "to the NAT instance"
	if err := waitOK(ctx)(c.routes.Update(ctx, &vpcpb.UpdateRouteRequest{
		Metadata: &commonpb.ResourceMetadata{Id: route, ParentId: rt, Name: "mut-route"}, Spec: r.GetSpec(),
	})); err != nil {
		t.Fatalf("update description: %v", err)
	}
	got, err := c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: route})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetDescription() != "to the NAT instance" || got.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("route description/version = %q/%d, want updated/2", got.GetSpec().GetDescription(), got.GetMetadata().GetResourceVersion())
	}

	// A subnet may grow another block.
	s, err := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: sn})
	if err != nil {
		t.Fatal(err)
	}
	first := subnetCIDRs(t, c, sn)
	s.Spec.GetIpv4PrivatePools().Pools[0].Cidrs = append(s.GetSpec().GetIpv4PrivatePools().GetPools()[0].GetCidrs(), &vpcpb.SubnetCidr{Cidr: "/28"})
	if err := waitOK(ctx)(c.subnets.Update(ctx, &vpcpb.UpdateSubnetRequest{
		Metadata: &commonpb.ResourceMetadata{Id: sn, ParentId: project, Name: "mut-sn"}, Spec: s.GetSpec(),
	})); err != nil {
		t.Fatalf("grow subnet: %v", err)
	}
	grown := subnetCIDRs(t, c, sn)
	if len(grown) != 2 || grown[0] != first[0] {
		t.Errorf("subnet cidrs after growing = %v, want %s kept first plus one more", grown, first[0])
	}
	// Dropping the block that holds the allocation is refused; dropping the other one is fine.
	s = getSubnet(t, c, sn)
	keepSecond := &vpcpb.SubnetSpec{NetworkId: netID, Ipv4PrivatePools: &vpcpb.IPv4PrivateSubnetPools{
		Pools: []*vpcpb.SubnetPool{{Cidrs: []*vpcpb.SubnetCidr{{Cidr: grown[1]}}}},
	}}
	err = waitOK(ctx)(c.subnets.Update(ctx, &vpcpb.UpdateSubnetRequest{
		Metadata: &commonpb.ResourceMetadata{Id: sn, ParentId: project, Name: "mut-sn"}, Spec: keepSecond,
	}))
	assertCode(t, err, codes.FailedPrecondition)
	s.Spec.GetIpv4PrivatePools().Pools[0].Cidrs = s.GetSpec().GetIpv4PrivatePools().GetPools()[0].GetCidrs()[:1]
	if err := waitOK(ctx)(c.subnets.Update(ctx, &vpcpb.UpdateSubnetRequest{
		Metadata: &commonpb.ResourceMetadata{Id: sn, ParentId: project, Name: "mut-sn"}, Spec: s.GetSpec(),
	})); err != nil {
		t.Fatalf("drop the unused block: %v", err)
	}
	if diff := cmp.Diff(first, subnetCIDRs(t, c, sn)); diff != "" {
		t.Errorf("subnet cidrs after shrinking (-want +got):\n%s", diff)
	}
}

func TestRouteValidation(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	_, netID := netWithPool(t, c, "routes", "/16")
	_, otherNet := netWithPool(t, c, "routes-other", "/16")
	sn := newSubnet(t, c, "sn", netID, "/28")
	alloc := newAlloc(t, c, "alloc-1", privAllocSpec(sn, "", ""))
	otherSn := newSubnet(t, c, "other-sn", otherNet, "/28")
	otherAlloc := newAlloc(t, c, "other-alloc", privAllocSpec(otherSn, "", ""))
	rt := newRouteTable(t, c, "rt", netID)

	// default_egress_gateway is only valid for 0.0.0.0/0.
	_, err := c.routes.Create(ctx, routeReq(rt, "bad-egress", "10.0.0.0/8", egressHop()))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.routes.Create(ctx, routeReq(rt, "bad-egress-2", "192.168.0.0/16", egressHop()))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.routes.Create(ctx, routeReq(rt, "false-egress", "0.0.0.0/0", &vpcpb.NextHop{NextHop: &vpcpb.NextHop_DefaultEgressGateway{DefaultEgressGateway: false}}))
	assertCode(t, err, codes.InvalidArgument)
	good := newRoute(t, c, rt, "egress", "0.0.0.0/0", egressHop())
	r, err := c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: good})
	if err != nil {
		t.Fatal(err)
	}
	if r.GetStatus().GetState() != vpcpb.RouteStatus_READY || r.GetStatus().GetNextHop().GetDefaultEgressGateway() == nil {
		t.Errorf("egress route status = %v", r.GetStatus())
	}

	// A destination appears once per route table, in any spelling.
	newRoute(t, c, rt, "to-nat", "192.168.0.0/16", allocHop(alloc))
	_, err = c.routes.Create(ctx, routeReq(rt, "to-nat-2", "192.168.0.0/16", allocHop(alloc)))
	assertCode(t, err, codes.AlreadyExists)
	_, err = c.routes.Create(ctx, routeReq(rt, "egress-2", "0.0.0.0/0", egressHop()))
	assertCode(t, err, codes.AlreadyExists)
	// The same destination is fine in another table.
	rt2 := newRouteTable(t, c, "rt2", netID)
	newRoute(t, c, rt2, "to-nat", "192.168.0.0/16", allocHop(alloc))

	// The next hop allocation status mirrors the allocation's address.
	nat, err := c.routes.GetByName(ctx, &vpcpb.GetRouteByNameRequest{ParentId: rt, Name: "to-nat"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nat.GetStatus().GetNextHop().GetAllocation().GetCidr(), allocCIDR(t, c, alloc).String(); got != want {
		t.Errorf("route next hop cidr = %q, want %q", got, want)
	}

	// An allocation from another network cannot be a next hop.
	_, err = c.routes.Create(ctx, routeReq(rt, "foreign-hop", "172.16.0.0/12", allocHop(otherAlloc)))
	assertCode(t, err, codes.InvalidArgument)
	// A public allocation outside any subnet cannot either.
	pub := newAlloc(t, c, "pub-alloc", pubAllocSpec("", seedPoolID(t, c, seedPublicPool), ""))
	_, err = c.routes.Create(ctx, routeReq(rt, "pub-hop", "172.17.0.0/16", allocHop(pub)))
	assertCode(t, err, codes.InvalidArgument)

	for name, dst := range map[string]string{
		"host bits": "10.1.2.3/8", "ipv6": "2001:db8::/32", "not a cidr": "somewhere", "empty": "",
	} {
		_, err := c.routes.Create(ctx, routeReq(rt, "bad-"+name, dst, allocHop(alloc)))
		if codeOf(err) != codes.InvalidArgument {
			t.Errorf("destination %q (%s): code = %v, want InvalidArgument", dst, name, codeOf(err))
		}
	}

	routes, err := c.routes.List(ctx, &vpcpb.ListRoutesRequest{ParentId: rt})
	if err != nil || len(routes.GetItems()) != 2 {
		t.Errorf("routes after the failures = %d, err %v, want 2", len(routes.GetItems()), err)
	}
}

func TestSecurityRules(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	netID := newNetwork(t, c, "net")
	sg := newGroup(t, c, "sg", netID)

	// Defaults: priority 500, STATEFUL.
	id := newRule(t, c, sg, "defaults", ingressRule([]string{"0.0.0.0/0"}, 443))
	r, err := c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if r.GetSpec().GetPriority() != 500 || r.GetSpec().GetType() != vpcpb.RuleType_STATEFUL {
		t.Errorf("defaults: priority %d type %v, want 500 STATEFUL", r.GetSpec().GetPriority(), r.GetSpec().GetType())
	}
	st := r.GetStatus()
	if st.GetState() != vpcpb.SecurityRuleStatus_READY || st.GetDirection() != vpcpb.RuleDirection_INGRESS {
		t.Errorf("status state/direction = %v/%v, want READY/INGRESS", st.GetState(), st.GetDirection())
	}
	if got := st.GetSource().GetCidrs(); len(got) != 1 || got[0] != "0.0.0.0/0" {
		t.Errorf("status source = %v, want 0.0.0.0/0", st.GetSource())
	}
	if got := st.GetDestination().GetPorts(); len(got) != 1 || got[0] != 443 || st.GetDestination().GetSecurityGroupId() != sg {
		t.Errorf("status destination = %v, want port 443 of %s", st.GetDestination(), sg)
	}
	allowEff := st.GetEffectivePriority()

	// DENY sorts before ALLOW at the same priority, lower priority numbers first.
	deny := ingressRule([]string{"0.0.0.0/0"}, 444)
	deny.Access = vpcpb.RuleAccessAction_DENY
	d := getSecurityRule(t, c, newRule(t, c, sg, "deny", deny))
	if d.GetStatus().GetEffectivePriority() >= allowEff {
		t.Errorf("DENY effective_priority %d should be below ALLOW's %d at equal priority", d.GetStatus().GetEffectivePriority(), allowEff)
	}
	early := ingressRule([]string{"0.0.0.0/0"}, 445)
	early.Priority = 100
	e := getSecurityRule(t, c, newRule(t, c, sg, "early", early))
	if e.GetStatus().GetEffectivePriority() >= d.GetStatus().GetEffectivePriority() {
		t.Errorf("priority 100 effective %d should be below priority 500's %d", e.GetStatus().GetEffectivePriority(), d.GetStatus().GetEffectivePriority())
	}

	// Egress.
	eg := &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_UDP, Type: vpcpb.RuleType_STATELESS,
		Match: &vpcpb.SecurityRuleSpec_Egress{Egress: &vpcpb.RuleEgress{DestinationCidrs: []string{"1.1.1.0/24"}, DestinationPorts: []int32{53}}},
	}
	er := getSecurityRule(t, c, newRule(t, c, sg, "dns", eg))
	if er.GetStatus().GetDirection() != vpcpb.RuleDirection_EGRESS || er.GetSpec().GetType() != vpcpb.RuleType_STATELESS {
		t.Errorf("egress rule direction/type = %v/%v", er.GetStatus().GetDirection(), er.GetSpec().GetType())
	}
	if got := er.GetStatus().GetDestination(); len(got.GetCidrs()) != 1 || got.GetCidrs()[0] != "1.1.1.0/24" {
		t.Errorf("egress status destination = %v", got)
	}
	if er.GetStatus().GetSource().GetSecurityGroupId() != sg {
		t.Errorf("egress status source = %v, want the group %s", er.GetStatus().GetSource(), sg)
	}

	// ICMP has no ports; other protocols and "any" are fine.
	icmp := ingressRule([]string{"10.0.0.0/8"})
	icmp.Protocol = vpcpb.RuleProtocol_ICMP
	newRule(t, c, sg, "ping", icmp)
	icmp.Match = &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceCidrs: []string{"10.0.0.0/8"}, DestinationPorts: []int32{8}}}
	_, err = c.rules.Create(ctx, ruleReq(sg, "ping-ports", icmp))
	assertCode(t, err, codes.InvalidArgument)
	icmp.Match = &vpcpb.SecurityRuleSpec_Egress{Egress: &vpcpb.RuleEgress{DestinationCidrs: []string{"10.0.0.0/8"}, DestinationPorts: []int32{8}}}
	_, err = c.rules.Create(ctx, ruleReq(sg, "ping-ports-out", icmp))
	assertCode(t, err, codes.InvalidArgument)

	bad := []struct {
		name string
		mut  func(*vpcpb.SecurityRuleSpec)
	}{
		{"no access", func(s *vpcpb.SecurityRuleSpec) { s.Access = vpcpb.RuleAccessAction_ACCESS_UNSPECIFIED }},
		{"no protocol", func(s *vpcpb.SecurityRuleSpec) { s.Protocol = vpcpb.RuleProtocol_PROTOCOL_UNSPECIFIED }},
		{"no match", func(s *vpcpb.SecurityRuleSpec) { s.Match = nil }},
		{"priority too high", func(s *vpcpb.SecurityRuleSpec) { s.Priority = 1001 }},
		{"negative priority", func(s *vpcpb.SecurityRuleSpec) { s.Priority = -1 }},
		{"port zero", func(s *vpcpb.SecurityRuleSpec) { s.GetIngress().DestinationPorts = []int32{0} }},
		{"port too high", func(s *vpcpb.SecurityRuleSpec) { s.GetIngress().DestinationPorts = []int32{65536} }},
		{"cidr host bits", func(s *vpcpb.SecurityRuleSpec) { s.GetIngress().SourceCidrs = []string{"10.0.0.1/8"} }},
		{"cidr ipv6", func(s *vpcpb.SecurityRuleSpec) { s.GetIngress().SourceCidrs = []string{"2001:db8::/32"} }},
		{"cidr garbage", func(s *vpcpb.SecurityRuleSpec) { s.GetIngress().SourceCidrs = []string{"anywhere"} }},
	}
	for _, tt := range bad {
		spec := ingressRule([]string{"10.0.0.0/8"}, 80)
		tt.mut(spec)
		_, err := c.rules.Create(ctx, ruleReq(sg, "bad-rule", spec))
		if got := codeOf(err); got != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", tt.name, got)
		}
	}

	// Rules can be renamed and relabelled, but not re-specified.
	rr := getSecurityRule(t, c, id)
	if err := waitOK(ctx)(c.rules.Update(ctx, &vpcpb.UpdateSecurityRuleRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: sg, Name: "https", Labels: map[string]string{"app": "web"}}, Spec: rr.GetSpec(),
	})); err != nil {
		t.Fatalf("rename rule: %v", err)
	}
	if got := getSecurityRule(t, c, id); got.GetMetadata().GetName() != "https" || got.GetSpec().GetPriority() != 500 {
		t.Errorf("renamed rule = %v", got)
	}
	// A request that leaves the defaulted fields unset is the same rule.
	unset := ingressRule([]string{"0.0.0.0/0"}, 443)
	if err := waitOK(ctx)(c.rules.Update(ctx, &vpcpb.UpdateSecurityRuleRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: sg, Name: "https", Labels: map[string]string{"app": "web"}}, Spec: unset,
	})); err != nil {
		t.Errorf("update with defaults left unset: %v", err)
	}
}

func TestSubnetAndNetworkUpdates(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	def := seedPoolID(t, c, seedPrivatePool)
	poolA := newPrivatePool(t, c, "pool-a", def, "/16")
	poolB := newPrivatePool(t, c, "pool-b", def, "/16")
	netID := newNetwork(t, c, "net", poolA)
	sn := newSubnet(t, c, "sn", netID, "/24")

	update := func(pools ...string) error {
		n, err := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: netID})
		if err != nil {
			t.Fatalf("Get network: %v", err)
		}
		n.Spec.Ipv4PrivatePools = &vpcpb.IPv4PrivateNetworkPools{}
		for _, p := range pools {
			n.Spec.Ipv4PrivatePools.Pools = append(n.Spec.Ipv4PrivatePools.Pools, &vpcpb.NetworkPool{Id: p})
		}
		return waitOK(ctx)(c.networks.Update(ctx, &vpcpb.UpdateNetworkRequest{
			Metadata: &commonpb.ResourceMetadata{Id: netID, ParentId: project, Name: "net"}, Spec: n.GetSpec(),
		}))
	}
	// A pool a subnet is carved from cannot be dropped from the network.
	assertCode(t, update(poolB), codes.FailedPrecondition)
	// Adding a pool is fine; so is dropping an unused one afterwards.
	if err := update(poolA, poolB); err != nil {
		t.Fatalf("add pool: %v", err)
	}
	assertCode(t, update(poolB), codes.FailedPrecondition)
	if err := update(poolA); err != nil {
		t.Fatalf("drop unused pool: %v", err)
	}
	// Invalid pools.
	assertCode(t, update(poolA, poolA), codes.InvalidArgument)                             // duplicate
	assertCode(t, update(seedPoolID(t, c, seedPublicPool)), codes.InvalidArgument)         // wrong visibility
	assertCode(t, update("vpcpool-e00missing"), codes.NotFound)                            // unknown
	assertCode(t, update(poolA, seedPoolID(t, c, seedPrivatePool)), codes.InvalidArgument) // overlapping pools
	if got := subnetCIDRs(t, c, sn); len(got) != 1 {
		t.Errorf("subnet cidrs = %v", got)
	}
}
