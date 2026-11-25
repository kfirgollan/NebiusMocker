package contract

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// nthBlock returns the n-th block of the given size inside p.
func nthBlock(t *testing.T, p netip.Prefix, bits, n int) string {
	t.Helper()
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v += uint32(n) << (32 - bits)
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), bits).String()
}

// addrOf strips the prefix length from a CIDR.
func addrOf(cidr string) string {
	a, _, _ := strings.Cut(cidr, "/")
	return a
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return p
}

func poolCIDR(t *testing.T, c vpcClients, id string) netip.Prefix {
	t.Helper()
	p, err := c.pools.Get(t.Context(), &vpcpb.GetPoolRequest{Id: id})
	if err != nil {
		t.Fatalf("Get pool: %v", err)
	}
	if len(p.GetStatus().GetCidrs()) != 1 {
		t.Fatalf("pool %s cidrs = %v, want one", id, p.GetStatus().GetCidrs())
	}
	return mustPrefix(t, p.GetStatus().GetCidrs()[0])
}

func TestPoolCarvesLowestFree(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	def := seedPoolID(t, c, seedPrivatePool)

	a := newPrivatePool(t, c, "pool-a", def, "/16")
	b := newPrivatePool(t, c, "pool-b", def, "/16")
	cc := newPrivatePool(t, c, "pool-c", def, "/17")
	base := mustPrefix(t, seedPrivateCIDR)
	if got, want := poolCIDR(t, c, a).String(), nthBlock(t, base, 16, 0); got != want {
		t.Errorf("first /16 = %s, want %s", got, want)
	}
	if got, want := poolCIDR(t, c, b).String(), nthBlock(t, base, 16, 1); got != want {
		t.Errorf("second /16 = %s, want %s", got, want)
	}
	if got, want := poolCIDR(t, c, cc).String(), nthBlock(t, base, 17, 4); got != want {
		t.Errorf("/17 after two /16 = %s, want %s", got, want)
	}

	// Freed space is reused, lowest first.
	if err := waitOK(ctx)(c.pools.Delete(ctx, &vpcpb.DeletePoolRequest{Id: a})); err != nil {
		t.Fatalf("delete pool-a: %v", err)
	}
	again := newPrivatePool(t, c, "pool-d", def, "/16")
	if got, want := poolCIDR(t, c, again).String(), nthBlock(t, base, 16, 0); got != want {
		t.Errorf("/16 after freeing the first = %s, want %s", got, want)
	}

	// Explicit blocks must be free and inside the source pool.
	_, err := c.pools.Create(ctx, poolReq(c.inst.ProjectID, "overlap", def, vpcpb.IpVisibility_PRIVATE, nthBlock(t, base, 16, 1)))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.pools.Create(ctx, poolReq(c.inst.ProjectID, "outside", def, vpcpb.IpVisibility_PRIVATE, "192.168.0.0/16"))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.pools.Create(ctx, poolReq(c.inst.ProjectID, "hostbits", def, vpcpb.IpVisibility_PRIVATE, "10.1.0.1/16"))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.pools.Create(ctx, poolReq(c.inst.ProjectID, "v6", def, vpcpb.IpVisibility_PRIVATE, "2001:db8::/48"))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.pools.Create(ctx, poolReq(c.inst.ProjectID, "wrongvis", def, vpcpb.IpVisibility_PUBLIC, "/24"))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.pools.Create(ctx, &vpcpb.CreatePoolRequest{Metadata: md(c.inst.ProjectID, "ipv6"), Spec: &vpcpb.PoolSpec{
		SourcePoolId: def, Version: vpcpb.IpVersion_IPV6, Visibility: vpcpb.IpVisibility_PRIVATE, Cidrs: []*vpcpb.PoolCidr{{Cidr: "/64"}},
	}})
	assertCode(t, err, codes.InvalidArgument)

	// An explicit block that is free is granted verbatim.
	exp := nthBlock(t, base, 16, 5)
	e := newPrivatePool(t, c, "explicit", def, exp)
	if got := poolCIDR(t, c, e).String(); got != exp {
		t.Errorf("explicit pool block = %s, want %s", got, exp)
	}
}

func TestPoolCIDRUpdate(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	def := seedPoolID(t, c, seedPrivatePool)
	id := newPrivatePool(t, c, "grow", def, "/16")
	first := poolCIDR(t, c, id)

	update := func(cidrs ...string) error {
		cur, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		spec := cur.GetSpec()
		spec.Cidrs = nil
		for _, cd := range cidrs {
			spec.Cidrs = append(spec.Cidrs, &vpcpb.PoolCidr{Cidr: cd})
		}
		return waitOK(ctx)(c.pools.Update(ctx, &vpcpb.UpdatePoolRequest{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: c.inst.ProjectID, Name: "grow"}, Spec: spec}))
	}
	if err := update("/16", "/16"); err != nil {
		t.Fatalf("add a second block: %v", err)
	}
	p, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{first.String(), nthBlock(t, mustPrefix(t, seedPrivateCIDR), 16, 1)}
	if diff := cmp.Diff(want, p.GetStatus().GetCidrs()); diff != "" {
		t.Errorf("cidrs after growing keep the old block first (-want +got):\n%s", diff)
	}

	// The second block holds a child pool, so it cannot be removed.
	child := newPrivatePool(t, c, "child", id, nthBlock(t, mustPrefix(t, seedPrivateCIDR), 24, 256))
	assertCode(t, update("/16"), codes.FailedPrecondition)
	if err := waitOK(ctx)(c.pools.Delete(ctx, &vpcpb.DeletePoolRequest{Id: child})); err != nil {
		t.Fatal(err)
	}
	if err := update("/16"); err != nil {
		t.Fatalf("shrink after the child is gone: %v", err)
	}
	p, err = c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{first.String()}, p.GetStatus().GetCidrs()); diff != "" {
		t.Errorf("cidrs after shrinking (-want +got):\n%s", diff)
	}
}

func TestSubnetLowestFree(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	poolID, netID := netWithPool(t, c, "ipam", "/16")
	pool := poolCIDR(t, c, poolID)

	s1 := newSubnet(t, c, "s1", netID, "/24")
	s2 := newSubnet(t, c, "s2", netID, "/24")
	s3 := newSubnet(t, c, "s3", netID, "/24")
	for i, id := range []string{s1, s2, s3} {
		want := []string{nthBlock(t, pool, 24, i)}
		if diff := cmp.Diff(want, subnetCIDRs(t, c, id)); diff != "" {
			t.Errorf("subnet %d cidrs (-want +got):\n%s", i+1, diff)
		}
	}
	// Deterministic: the same inputs on a fresh mocker give the same blocks.
	c2 := startVPC(t)
	poolID2, netID2 := netWithPool(t, c2, "ipam", "/16")
	if got, want := subnetCIDRs(t, c2, newSubnet(t, c2, "s1", netID2, "/24")), []string{nthBlock(t, poolCIDR(t, c2, poolID2), 24, 0)}; cmp.Diff(want, got) != "" {
		t.Errorf("fresh mocker first /24 = %v, want %v", got, want)
	}

	// A gap is filled before new space: free the middle subnet.
	if err := waitOK(ctx)(c.subnets.Delete(ctx, &vpcpb.DeleteSubnetRequest{Id: s2})); err != nil {
		t.Fatal(err)
	}
	s4 := newSubnet(t, c, "s4", netID, "/24")
	if diff := cmp.Diff([]string{nthBlock(t, pool, 24, 1)}, subnetCIDRs(t, c, s4)); diff != "" {
		t.Errorf("gap not reused (-want +got):\n%s", diff)
	}

	// Mixed sizes are aligned to their size: /25 lands after the /24s, the /23
	// skips to the next aligned block.
	s5 := newSubnet(t, c, "s5", netID, "/25")
	if diff := cmp.Diff([]string{addrOf(nthBlock(t, pool, 24, 3)) + "/25"}, subnetCIDRs(t, c, s5)); diff != "" {
		t.Errorf("/25 placement (-want +got):\n%s", diff)
	}
	s6 := newSubnet(t, c, "s6", netID, "/23")
	if diff := cmp.Diff([]string{nthBlock(t, pool, 23, 2)}, subnetCIDRs(t, c, s6)); diff != "" {
		t.Errorf("/23 placement (-want +got):\n%s", diff)
	}

	// Several blocks in one subnet.
	multi := newSubnet(t, c, "multi", netID, "/26", "/26")
	if got := subnetCIDRs(t, c, multi); len(got) != 2 || got[0] == got[1] {
		t.Errorf("multi-block subnet cidrs = %v, want two distinct /26", got)
	}
}

func TestSubnetExplicitCIDR(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	poolID, netID := netWithPool(t, c, "explicit", "/16")
	pool := poolCIDR(t, c, poolID)

	want := nthBlock(t, pool, 24, 7)
	id := newSubnet(t, c, "chosen", netID, want)
	if diff := cmp.Diff([]string{want}, subnetCIDRs(t, c, id)); diff != "" {
		t.Errorf("explicit subnet cidrs (-want +got):\n%s", diff)
	}
	// Auto placement avoids it but starts at the lowest free block.
	auto := newSubnet(t, c, "auto", netID, "/24")
	if diff := cmp.Diff([]string{nthBlock(t, pool, 24, 0)}, subnetCIDRs(t, c, auto)); diff != "" {
		t.Errorf("auto subnet next to an explicit one (-want +got):\n%s", diff)
	}

	tests := []struct {
		name  string
		cidrs []string
	}{
		{"same block", []string{want}},
		{"inside an existing block", []string{nthBlock(t, pool, 25, 14)}},
		{"containing an existing block", []string{nthBlock(t, pool, 23, 3)}},
		{"outside the network pools", []string{"192.168.0.0/24"}},
		{"outside the pool but inside the default pool", []string{"10.7.0.0/24"}},
		{"host bits set", []string{addrOf(nthBlock(t, pool, 24, 9))[:len(addrOf(nthBlock(t, pool, 24, 9)))-1] + "5/24"}},
		{"ipv6", []string{"2001:db8::/64"}},
		{"prefix length above 32", []string{"/33"}},
		{"not a cidr", []string{"banana"}},
		{"overlap within one request", []string{nthBlock(t, pool, 24, 20), nthBlock(t, pool, 25, 40)}},
	}
	for _, tt := range tests {
		_, err := c.subnets.Create(ctx, createSubnetReq(c, "bad", subnetSpec(netID, tt.cidrs...)))
		if got := codeOf(err); got != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", tt.name, got)
		}
	}
	// Nothing leaked from the failed requests.
	list, err := c.subnets.ListByNetwork(ctx, &vpcpb.ListSubnetsByNetworkRequest{NetworkId: netID})
	if err != nil || len(list.GetItems()) != 2 {
		t.Errorf("subnets after failures = %d, err %v, want 2", len(list.GetItems()), err)
	}

	// A subnet in another network may not use blocks outside that network's pools,
	// even when they are free in this one.
	_, otherNet := netWithPool(t, c, "other", "/16")
	_, err = c.subnets.Create(ctx, createSubnetReq(c, "cross", subnetSpec(otherNet, nthBlock(t, pool, 24, 30))))
	assertCode(t, err, codes.InvalidArgument)
}

func TestSubnetUsesNetworkPools(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	poolID, netID := netWithPool(t, c, "shared", "/20")
	pool := poolCIDR(t, c, poolID)

	// A subnet without CIDRs shares the whole space of the network's pools.
	id := wait(t)(c.subnets.Create(ctx, createSubnetReq(c, "shared-subnet", &vpcpb.SubnetSpec{NetworkId: netID})))
	sn, err := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if !sn.GetSpec().GetIpv4PrivatePools().GetUseNetworkPools() {
		t.Errorf("use_network_pools = false, want true (default when no pools are given)")
	}
	pools := sn.GetStatus().GetIpv4PrivatePools()
	if len(pools) != 1 || pools[0].GetPoolId() != poolID || len(pools[0].GetCidrs()) != 1 || pools[0].GetCidrs()[0] != pool.String() {
		t.Errorf("status pools = %v, want %s from %s", pools, pool, poolID)
	}
	rt := sn.GetStatus().GetRouteTable()
	n, err := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: netID})
	if err != nil {
		t.Fatal(err)
	}
	if rt.GetId() != n.GetStatus().GetDefaultRouteTableId() || !rt.GetDefault() {
		t.Errorf("subnet route table = %v, want the network's default %s", rt, n.GetStatus().GetDefaultRouteTableId())
	}

	// pools and use_network_pools together are contradictory.
	spec := subnetSpec(netID, "/24")
	spec.Ipv4PrivatePools.UseNetworkPools = true
	_, err = c.subnets.Create(ctx, createSubnetReq(c, "both", spec))
	assertCode(t, err, codes.InvalidArgument)

	// A route table of another network cannot be used.
	otherNet := newNetwork(t, c, "elsewhere")
	foreign := newRouteTable(t, c, "foreign", otherNet)
	spec = subnetSpec(netID, "/24")
	spec.RouteTableId = foreign
	_, err = c.subnets.Create(ctx, createSubnetReq(c, "foreign-rt", spec))
	assertCode(t, err, codes.InvalidArgument)
}

func TestPoolExhaustion(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	def := seedPoolID(t, c, seedPrivatePool)

	t.Run("child pools", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		parent := newPrivatePool(t, c, "tiny-parent", def, "/29")
		newPrivatePool(t, c, "tiny-1", parent, "/30")
		second := newPrivatePool(t, c, "tiny-2", parent, "/30")
		_, err := c.pools.Create(ctx, poolReq(c.inst.ProjectID, "tiny-3", parent, vpcpb.IpVisibility_PRIVATE, "/30"))
		assertCode(t, err, codes.FailedPrecondition)
		_, err = c.pools.Create(ctx, poolReq(c.inst.ProjectID, "tiny-big", parent, vpcpb.IpVisibility_PRIVATE, "/28"))
		assertCode(t, err, codes.FailedPrecondition)
		if err := waitOK(ctx)(c.pools.Delete(ctx, &vpcpb.DeletePoolRequest{Id: second})); err != nil {
			t.Fatal(err)
		}
		newPrivatePool(t, c, "tiny-3", parent, "/30")
	})

	t.Run("subnets", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		_, netID := netWithPool(t, c, "sub-exhaust", "/30")
		first := newSubnet(t, c, "only", netID, "/30")
		_, err := c.subnets.Create(ctx, createSubnetReq(c, "second", subnetSpec(netID, "/30")))
		assertCode(t, err, codes.FailedPrecondition)
		_, err = c.subnets.Create(ctx, createSubnetReq(c, "smaller", subnetSpec(netID, "/31")))
		assertCode(t, err, codes.FailedPrecondition)
		if err := waitOK(ctx)(c.subnets.Delete(ctx, &vpcpb.DeleteSubnetRequest{Id: first})); err != nil {
			t.Fatal(err)
		}
		newSubnet(t, c, "again", netID, "/30")
	})

	t.Run("allocations from a pool", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		poolID, _ := netWithPool(t, c, "alloc-exhaust", "/30")
		pool := poolCIDR(t, c, poolID)
		var ids []string
		for i := range 4 {
			id := newAlloc(t, c, nameN("a", i), privAllocSpec("", poolID, ""))
			if got, want := allocCIDR(t, c, id).String(), nthBlock(t, pool, 32, i); got != want {
				t.Errorf("allocation %d = %s, want %s", i, got, want)
			}
			ids = append(ids, id)
		}
		_, err := c.allocations.Create(ctx, createAllocReq(c, "a-4", privAllocSpec("", poolID, "")))
		assertCode(t, err, codes.FailedPrecondition)
		if err := waitOK(ctx)(c.allocations.Delete(ctx, &vpcpb.DeleteAllocationRequest{Id: ids[1]})); err != nil {
			t.Fatal(err)
		}
		again := newAlloc(t, c, "a-again", privAllocSpec("", poolID, ""))
		if got, want := allocCIDR(t, c, again).String(), nthBlock(t, pool, 32, 1); got != want {
			t.Errorf("allocation after freeing = %s, want %s", got, want)
		}
	})

	t.Run("allocations in a subnet", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		_, netID := netWithPool(t, c, "snet-exhaust", "/24")
		sn := newSubnet(t, c, "tiny-subnet", netID, "/29")
		block := mustPrefix(t, subnetCIDRs(t, c, sn)[0])
		got := map[string]bool{}
		// A /29 has 8 addresses; network, gateway (.1) and broadcast are reserved.
		for i := range 5 {
			cidr := allocCIDR(t, c, newAlloc(t, c, nameN("h", i), privAllocSpec(sn, "", ""))).String()
			if got[cidr] {
				t.Errorf("address %s allocated twice", cidr)
			}
			got[cidr] = true
			if want := nthBlock(t, block, 32, i+2); cidr != want {
				t.Errorf("allocation %d = %s, want %s", i, cidr, want)
			}
		}
		_, err := c.allocations.Create(ctx, createAllocReq(c, "h-5", privAllocSpec(sn, "", "")))
		assertCode(t, err, codes.FailedPrecondition)
	})
}

func TestPrivateAllocations(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	poolID, netID := netWithPool(t, c, "priv", "/16")
	pool := poolCIDR(t, c, poolID)
	sn := newSubnet(t, c, "sn", netID, "/24")
	block := mustPrefix(t, subnetCIDRs(t, c, sn)[0])

	reserved := map[string]bool{
		nthBlock(t, block, 32, 0):   true, // network address
		nthBlock(t, block, 32, 1):   true, // gateway
		nthBlock(t, block, 32, 255): true, // broadcast
	}
	seen := map[string]bool{}
	for i := range 20 {
		id := newAlloc(t, c, nameN("ip", i), privAllocSpec(sn, "", ""))
		a, err := c.allocations.Get(ctx, &vpcpb.GetAllocationRequest{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		d := a.GetStatus().GetDetails()
		p := mustPrefix(t, d.GetAllocatedCidr())
		if p.Bits() != 32 {
			t.Errorf("allocation %d = %s, want a single address", i, p)
		}
		if !block.Contains(p.Addr()) {
			t.Errorf("allocation %s is outside the subnet %s", p, block)
		}
		if reserved[p.String()] {
			t.Errorf("allocation %s is a reserved address", p)
		}
		if seen[p.String()] {
			t.Errorf("allocation %s handed out twice", p)
		}
		seen[p.String()] = true
		if want := nthBlock(t, block, 32, i+2); p.String() != want {
			t.Errorf("allocation %d = %s, want the lowest free %s", i, p, want)
		}
		if d.GetSubnetId() != sn || d.GetPoolId() != poolID || d.GetVersion() != vpcpb.IpVersion_IPV4 {
			t.Errorf("allocation details = %v, want subnet %s pool %s IPV4", d, sn, poolID)
		}
		if a.GetStatus().GetState() != vpcpb.AllocationStatus_ALLOCATED {
			t.Errorf("state = %v, want ALLOCATED", a.GetStatus().GetState())
		}
	}

	// Explicit address.
	want := nthBlock(t, block, 32, 100)
	id := newAlloc(t, c, "explicit", privAllocSpec(sn, "", want))
	if got := allocCIDR(t, c, id).String(); got != want {
		t.Errorf("explicit allocation = %s, want %s", got, want)
	}
	// Without a prefix length a bare address means a /32.
	bare := newAlloc(t, c, "bare", privAllocSpec(sn, "", addrOf(nthBlock(t, block, 32, 101))))
	if got := allocCIDR(t, c, bare).String(); got != nthBlock(t, block, 32, 101) {
		t.Errorf("bare-address allocation = %s", got)
	}

	tests := []struct {
		name string
		spec *vpcpb.AllocationSpec
		want codes.Code
	}{
		{"taken", privAllocSpec(sn, "", want), codes.InvalidArgument},
		{"network address", privAllocSpec(sn, "", nthBlock(t, block, 32, 0)), codes.InvalidArgument},
		{"gateway", privAllocSpec(sn, "", nthBlock(t, block, 32, 1)), codes.InvalidArgument},
		{"broadcast", privAllocSpec(sn, "", nthBlock(t, block, 32, 255)), codes.InvalidArgument},
		{"outside the subnet", privAllocSpec(sn, "", nthBlock(t, pool, 32, 4000)), codes.InvalidArgument},
		{"host bits set", privAllocSpec(sn, "", addrOf(nthBlock(t, block, 32, 50))+"/24"), codes.InvalidArgument},
		{"ipv6", privAllocSpec(sn, "", "2001:db8::1/128"), codes.InvalidArgument},
		{"ipv6 bare address", privAllocSpec(sn, "", "2001:db8::1"), codes.InvalidArgument},
		{"prefix above 32", privAllocSpec(sn, "", "/33"), codes.InvalidArgument},
		{"not an address", privAllocSpec(sn, "", "banana"), codes.InvalidArgument},
	}
	for _, tt := range tests {
		_, err := c.allocations.Create(ctx, createAllocReq(c, "bad", tt.spec))
		if got := codeOf(err); got != tt.want {
			t.Errorf("%s: code = %v, want %v", tt.name, got, tt.want)
		}
	}

	// The same address can be requested again once it is released.
	if err := waitOK(ctx)(c.allocations.Delete(ctx, &vpcpb.DeleteAllocationRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	newAlloc(t, c, "reuse", privAllocSpec(sn, "", want))
}

func TestPrivateAllocationInSharedSubnet(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	poolID, netID := netWithPool(t, c, "shared", "/24")
	pool := poolCIDR(t, c, poolID)
	sn := wait(t)(c.subnets.Create(t.Context(), createSubnetReq(c, "shared", &vpcpb.SubnetSpec{NetworkId: netID})))

	// A subnet sharing the network's pools hands out the pools' addresses,
	// skipping the network and gateway addresses of the block.
	for i := range 3 {
		got := allocCIDR(t, c, newAlloc(t, c, nameN("s", i), privAllocSpec(sn, "", ""))).String()
		if want := nthBlock(t, pool, 32, i+2); got != want {
			t.Errorf("allocation %d = %s, want %s", i, got, want)
		}
	}
}

func TestPublicAllocations(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	pubPool := seedPoolID(t, c, seedPublicPool)
	_, netID := netWithPool(t, c, "pub", "/24")
	sn := newSubnet(t, c, "sn", netID, "/26")
	public := mustPrefix(t, seedPublicCIDR)

	seen := map[string]bool{}
	check := func(what, id string) {
		t.Helper()
		a, err := c.allocations.Get(ctx, &vpcpb.GetAllocationRequest{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		p := mustPrefix(t, a.GetStatus().GetDetails().GetAllocatedCidr())
		if !public.Contains(p.Addr()) || p.Bits() != 32 {
			t.Errorf("%s: %s is not a single address of %s", what, p, public)
		}
		if seen[p.String()] {
			t.Errorf("%s: %s handed out twice", what, p)
		}
		seen[p.String()] = true
		if got := a.GetStatus().GetDetails().GetPoolId(); got != pubPool {
			t.Errorf("%s: pool_id = %s, want the default public pool %s", what, got, pubPool)
		}
	}
	for i := range 3 {
		check("by pool", newAlloc(t, c, nameN("by-pool", i), pubAllocSpec("", pubPool, "")))
	}
	for i := range 3 {
		// The subnet has no public pools of its own, so it draws from its network's.
		check("by subnet", newAlloc(t, c, nameN("by-subnet", i), pubAllocSpec(sn, "", "")))
	}

	explicit := nthBlock(t, public, 32, 200)
	check("explicit", newAlloc(t, c, "explicit", pubAllocSpec("", pubPool, explicit)))
	_, err := c.allocations.Create(ctx, createAllocReq(c, "dup", pubAllocSpec("", pubPool, explicit)))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.allocations.Create(ctx, createAllocReq(c, "out", pubAllocSpec("", pubPool, "198.51.100.7/32")))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.allocations.Create(ctx, createAllocReq(c, "v6", pubAllocSpec("", pubPool, "2001:db8::1/128")))
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.allocations.Create(ctx, createAllocReq(c, "hostbits", pubAllocSpec("", pubPool, "203.0.113.5/24")))
	assertCode(t, err, codes.InvalidArgument)

	// The pool lists its allocations.
	list, err := c.allocations.ListByPool(ctx, &vpcpb.ListAllocationsByPoolRequest{PoolId: pubPool})
	if err != nil || len(list.GetItems()) != len(seen) {
		t.Errorf("ListByPool(public) = %d items, err %v, want %d", len(list.GetItems()), err, len(seen))
	}
}

func TestPublicPoolExhaustion(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	// A root public pool with a single /30 documentation block.
	root := wait(t)(c.pools.Create(ctx, poolReq(c.inst.ProjectID, "pub-small", "", vpcpb.IpVisibility_PUBLIC, "198.51.100.0/30")))
	for i := range 4 {
		newAlloc(t, c, nameN("p", i), pubAllocSpec("", root, ""))
	}
	_, err := c.allocations.Create(ctx, createAllocReq(c, "p-4", pubAllocSpec("", root, "")))
	assertCode(t, err, codes.FailedPrecondition)
}
