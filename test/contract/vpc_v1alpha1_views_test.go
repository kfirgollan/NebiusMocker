package contract

import (
	"net/netip"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"
)

const (
	seedPrivateScope = "default-private-scope"
	seedPublicScope  = "default-public-scope"
)

func TestVPCV1alpha1Scopes(t *testing.T) {
	t.Parallel()
	c, a := startAlpha(t)
	ctx := t.Context()
	project := c.inst.ProjectID

	l, err := a.scopes.List(ctx, &vpcv1alpha1pb.ListScopesRequest{ParentId: project})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.GetItems()) != 2 {
		t.Fatalf("scopes = %d, want 2", len(l.GetItems()))
	}
	for name, typ := range map[string]vpcv1alpha1pb.ScopeSpec_Type{
		seedPrivateScope: vpcv1alpha1pb.ScopeSpec_PRIVATE,
		seedPublicScope:  vpcv1alpha1pb.ScopeSpec_PUBLIC,
	} {
		s, err := a.scopes.GetByName(ctx, &vpcv1alpha1pb.GetScopeByNameRequest{ParentId: project, Name: name})
		if err != nil {
			t.Fatalf("GetByName %s: %v", name, err)
		}
		if s.GetSpec().GetType() != typ || s.GetStatus().GetState() != vpcv1alpha1pb.ScopeStatus_READY {
			t.Errorf("%s: spec %v status %v, want %v READY", name, s.GetSpec(), s.GetStatus(), typ)
		}
		byID, err := a.scopes.Get(ctx, &vpcv1alpha1pb.GetScopeRequest{Id: s.GetMetadata().GetId()})
		if err != nil || byID.GetMetadata().GetName() != name {
			t.Errorf("Get(%s) = %v (err %v)", s.GetMetadata().GetId(), byID.GetMetadata().GetName(), err)
		}
		if !slices.ContainsFunc(l.GetItems(), func(x *vpcv1alpha1pb.Scope) bool { return x.GetMetadata().GetId() == s.GetMetadata().GetId() }) {
			t.Errorf("List lacks scope %s", name)
		}
	}
	_, err = a.scopes.Get(ctx, &vpcv1alpha1pb.GetScopeRequest{Id: "vpcscope-e00missing"})
	assertCode(t, err, codes.NotFound)
	_, err = a.scopes.List(ctx, &vpcv1alpha1pb.ListScopesRequest{ParentId: project, Filter: "x"})
	assertCode(t, err, codes.InvalidArgument)
}

func TestVPCV1alpha1Pools(t *testing.T) {
	t.Parallel()
	c, a := startAlpha(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	privScope, err := a.scopes.GetByName(ctx, &vpcv1alpha1pb.GetScopeByNameRequest{ParentId: project, Name: seedPrivateScope})
	if err != nil {
		t.Fatal(err)
	}

	def, err := a.pools.GetByName(ctx, &vpcv1alpha1pb.GetPoolByNameRequest{ParentId: project, Name: seedPrivatePool})
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if def.GetSpec().GetSourceScopeId() != privScope.GetMetadata().GetId() || def.GetStatus().GetScopeId() != privScope.GetMetadata().GetId() {
		t.Errorf("source_scope_id %q, status.scope_id %q, want the private scope %s",
			def.GetSpec().GetSourceScopeId(), def.GetStatus().GetScopeId(), privScope.GetMetadata().GetId())
	}
	if def.GetSpec().GetVersion() != vpcv1alpha1pb.IpVersion_IPV4 || def.GetStatus().GetState() != vpcv1alpha1pb.PoolStatus_READY {
		t.Errorf("version %v state %v, want IPV4 READY", def.GetSpec().GetVersion(), def.GetStatus().GetState())
	}
	if cs := def.GetSpec().GetCidrs(); len(cs) != 1 || cs[0].GetCidr() != seedPrivateCIDR || cs[0].GetState() != vpcv1alpha1pb.PoolCidrState_AVAILABLE {
		t.Errorf("cidrs = %v, want %s AVAILABLE", cs, seedPrivateCIDR)
	}
	byID, err := a.pools.Get(ctx, &vpcv1alpha1pb.GetPoolRequest{Id: def.GetMetadata().GetId()})
	if err != nil || byID.GetMetadata().GetName() != seedPrivatePool {
		t.Errorf("Get = %v (err %v)", byID.GetMetadata().GetName(), err)
	}

	// The public pool hangs off the public scope.
	pub, err := a.pools.GetByName(ctx, &vpcv1alpha1pb.GetPoolByNameRequest{ParentId: project, Name: seedPublicPool})
	if err != nil {
		t.Fatal(err)
	}
	pubScope, err := a.scopes.GetByName(ctx, &vpcv1alpha1pb.GetScopeByNameRequest{ParentId: project, Name: seedPublicScope})
	if err != nil {
		t.Fatal(err)
	}
	if pub.GetSpec().GetSourceScopeId() != pubScope.GetMetadata().GetId() || pub.GetStatus().GetScopeId() != pubScope.GetMetadata().GetId() {
		t.Errorf("public pool scope = %q / %q, want %s", pub.GetSpec().GetSourceScopeId(), pub.GetStatus().GetScopeId(), pubScope.GetMetadata().GetId())
	}

	// A pool created through v1 shows its source pool.
	child := newPrivatePool(t, c, "child", def.GetMetadata().GetId(), "/16")
	ch, err := a.pools.Get(ctx, &vpcv1alpha1pb.GetPoolRequest{Id: child})
	if err != nil {
		t.Fatal(err)
	}
	if ch.GetSpec().GetSourcePoolId() != def.GetMetadata().GetId() || ch.GetSpec().GetSourceScopeId() != "" {
		t.Errorf("child spec = %v, want source_pool_id %s", ch.GetSpec(), def.GetMetadata().GetId())
	}
	if ch.GetStatus().GetScopeId() != privScope.GetMetadata().GetId() {
		t.Errorf("child status.scope_id = %q, want %s", ch.GetStatus().GetScopeId(), privScope.GetMetadata().GetId())
	}

	l := pageAll(t, 1, func(size int64, token string) ([]*vpcv1alpha1pb.Pool, string, error) {
		r, err := a.pools.List(ctx, &vpcv1alpha1pb.ListPoolsRequest{ParentId: project, PageSize: size, PageToken: token})
		return r.GetItems(), r.GetNextPageToken(), err
	})
	var names []string
	for _, p := range l {
		names = append(names, p.GetMetadata().GetName())
	}
	for _, n := range []string{seedPrivatePool, seedPublicPool, "child"} {
		if !slices.Contains(names, n) {
			t.Errorf("List %v lacks %s", names, n)
		}
	}
	_, err = a.pools.Get(ctx, &vpcv1alpha1pb.GetPoolRequest{Id: "vpcpool-e00missing"})
	assertCode(t, err, codes.NotFound)
	_, err = a.pools.List(ctx, &vpcv1alpha1pb.ListPoolsRequest{ParentId: project, Filter: "x"})
	assertCode(t, err, codes.InvalidArgument)
}

func TestVPCV1alpha1NetworksAndSubnets(t *testing.T) {
	t.Parallel()
	c, a := startAlpha(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	defPool := seedPoolID(t, c, seedPrivatePool)
	defPoolAlpha, err := a.pools.Get(ctx, &vpcv1alpha1pb.GetPoolRequest{Id: defPool})
	if err != nil {
		t.Fatal(err)
	}

	d, err := a.networks.GetDefault(ctx, &vpcv1alpha1pb.GetDefaultNetworkRequest{ParentId: project})
	if err != nil {
		t.Fatalf("GetDefault: %v", err)
	}
	n, sn := d.GetNetwork(), d.GetSubnet()
	if !n.GetStatus().GetDefault() || n.GetStatus().GetState() != vpcv1alpha1pb.NetworkStatus_READY {
		t.Errorf("default network status = %v", n.GetStatus())
	}
	if n.GetStatus().GetScopeId() != defPoolAlpha.GetStatus().GetScopeId() || n.GetStatus().GetScopeId() == "" {
		t.Errorf("network scope_id = %q, want the private pool's %q", n.GetStatus().GetScopeId(), defPoolAlpha.GetStatus().GetScopeId())
	}
	if ps := n.GetSpec().GetPools(); len(ps) != 1 || ps[0].GetPoolId() != defPool {
		t.Errorf("network pools = %v, want %s", ps, defPool)
	}
	if !sn.GetStatus().GetDefault() || len(sn.GetStatus().GetIpv4Cidrs()) == 0 || sn.GetSpec().GetNetworkId() != n.GetMetadata().GetId() {
		t.Errorf("default subnet = %v", sn)
	}

	gn, err := a.networks.Get(ctx, &vpcv1alpha1pb.GetNetworkRequest{Id: n.GetMetadata().GetId()})
	if err != nil || gn.GetMetadata().GetName() != "default-network" {
		t.Errorf("network Get = %v (err %v)", gn.GetMetadata().GetName(), err)
	}
	gnn, err := a.networks.GetByName(ctx, &vpcv1alpha1pb.GetNetworkByNameRequest{ParentId: project, Name: "default-network"})
	if err != nil || gnn.GetMetadata().GetId() != n.GetMetadata().GetId() {
		t.Errorf("network GetByName = %v (err %v)", gnn.GetMetadata().GetId(), err)
	}

	// A network with its own pool, made through v1.
	pool, net := netWithPool(t, c, "views", "/16")
	other, err := a.networks.Get(ctx, &vpcv1alpha1pb.GetNetworkRequest{Id: net})
	if err != nil {
		t.Fatal(err)
	}
	if ps := other.GetSpec().GetPools(); len(ps) != 1 || ps[0].GetPoolId() != pool || other.GetStatus().GetDefault() {
		t.Errorf("custom network = %v", other)
	}
	nets := pageAll(t, 1, func(size int64, token string) ([]*vpcv1alpha1pb.Network, string, error) {
		r, err := a.networks.List(ctx, &vpcv1alpha1pb.ListNetworksRequest{ParentId: project, PageSize: size, PageToken: token})
		return r.GetItems(), r.GetNextPageToken(), err
	})
	if len(nets) != 2 {
		t.Errorf("networks = %d, want 2", len(nets))
	}
	_, err = a.networks.List(ctx, &vpcv1alpha1pb.ListNetworksRequest{ParentId: project, Filter: "x"})
	assertCode(t, err, codes.InvalidArgument)

	// Subnets: the default one, and a v1 subnet with an explicit block.
	gs, err := a.subnets.GetByName(ctx, &vpcv1alpha1pb.GetSubnetByNameRequest{ParentId: project, Name: "default-subnet"})
	if err != nil || gs.GetMetadata().GetId() != sn.GetMetadata().GetId() || !gs.GetStatus().GetDefault() {
		t.Errorf("default subnet GetByName = %v (err %v)", gs, err)
	}
	poolCIDR := netip.MustParsePrefix(getPool(t, c, pool).GetStatus().GetCidrs()[0])
	b := poolCIDR.Addr().As4()
	b[2] = 100
	explicit := netip.PrefixFrom(netip.AddrFrom4(b), 24).String()
	vsn := newSubnet(t, c, "views-sn", net, explicit)
	got, err := a.subnets.Get(ctx, &vpcv1alpha1pb.GetSubnetRequest{Id: vsn})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetNetworkId() != net || got.GetStatus().GetDefault() ||
		!slices.Equal(got.GetStatus().GetIpv4Cidrs(), []string{explicit}) {
		t.Errorf("subnet = %v, want network %s, cidrs [%s]", got, net, explicit)
	}
	if ps := got.GetSpec().GetPools(); len(ps) != 1 || len(ps[0].GetSpec().GetCidrs()) != 1 || ps[0].GetSpec().GetCidrs()[0].GetCidr() != explicit {
		t.Errorf("subnet pools = %v, want a SubnetPool spec with %s", ps, explicit)
	}
	by, err := a.subnets.ListByNetwork(ctx, &vpcv1alpha1pb.ListSubnetsByNetworkRequest{NetworkId: net})
	if err != nil || len(by.GetItems()) != 1 || by.GetItems()[0].GetMetadata().GetId() != vsn {
		t.Errorf("ListByNetwork = %v (err %v), want [%s]", by.GetItems(), err, vsn)
	}
	all := pageAll(t, 1, func(size int64, token string) ([]*vpcv1alpha1pb.Subnet, string, error) {
		r, err := a.subnets.List(ctx, &vpcv1alpha1pb.ListSubnetsRequest{ParentId: project, PageSize: size, PageToken: token})
		return r.GetItems(), r.GetNextPageToken(), err
	})
	if len(all) != 2 {
		t.Errorf("subnets = %d, want 2", len(all))
	}
	_, err = a.subnets.List(ctx, &vpcv1alpha1pb.ListSubnetsRequest{ParentId: project, Filter: "x"})
	assertCode(t, err, codes.InvalidArgument)
	_, err = a.subnets.ListByNetwork(ctx, &vpcv1alpha1pb.ListSubnetsByNetworkRequest{NetworkId: net, Filter: "x"})
	assertCode(t, err, codes.InvalidArgument)
	_, err = a.subnets.Get(ctx, &vpcv1alpha1pb.GetSubnetRequest{Id: "vpcsubnet-e00missing"})
	assertCode(t, err, codes.NotFound)
	_, err = a.networks.Get(ctx, &vpcv1alpha1pb.GetNetworkRequest{Id: "vpcnetwork-e00missing"})
	assertCode(t, err, codes.NotFound)
}

func TestVPCV1alpha1CrossVersionAllocations(t *testing.T) {
	t.Parallel()
	c, a := startAlpha(t)
	ctx := t.Context()
	_, net := netWithPool(t, c, "xv", "/16")
	sn := newSubnet(t, c, "xv-sn", net, "/24")

	// v1 -> v1alpha1.
	v1ID := newAlloc(t, c, "from-v1", privAllocSpec(sn, "", ""))
	if got := getAlphaAlloc(t, a, v1ID).GetStatus().GetDetails().GetAllocatedCidr(); got != allocCIDR(t, c, v1ID).String() {
		t.Errorf("v1alpha1 sees %s, v1 sees %s", got, allocCIDR(t, c, v1ID))
	}
	// v1alpha1 -> v1.
	aID := newAlphaAlloc(t, c, a, "from-alpha", alphaPrivSpec(sn, "", ""))
	if got := allocCIDR(t, c, aID).String(); got != getAlphaAlloc(t, a, aID).GetStatus().GetDetails().GetAllocatedCidr() {
		t.Errorf("v1 sees %s, v1alpha1 sees %s", got, getAlphaAlloc(t, a, aID).GetStatus().GetDetails().GetAllocatedCidr())
	}

	// One IPAM: alternating creates never collide.
	seen := map[string]string{}
	record := func(id string) {
		t.Helper()
		cidr := allocCIDR(t, c, id).String()
		if prev, dup := seen[cidr]; dup {
			t.Errorf("%s handed out to %s and %s", cidr, prev, id)
		}
		seen[cidr] = id
	}
	record(v1ID)
	record(aID)
	for i := range 6 {
		if i%2 == 0 {
			record(newAlloc(t, c, nameN("alt-v1", i), privAllocSpec(sn, "", "")))
		} else {
			record(newAlphaAlloc(t, c, a, nameN("alt-alpha", i), alphaPrivSpec(sn, "", "")))
		}
	}

	// An address taken through one version is refused through the other.
	taken := allocCIDR(t, c, v1ID).String()
	_, err := a.allocations.Create(ctx, alphaAllocReq(c.inst.ProjectID, "again", alphaPrivSpec(sn, "", taken)))
	if codeOf(err) == codes.OK {
		t.Errorf("v1alpha1 re-allocated %s held by a v1 allocation", taken)
	}

	// Deleting through one version removes it from both.
	if err := alphaOK(ctx)(a.allocations.Delete(ctx, &vpcv1alpha1pb.DeleteAllocationRequest{Id: v1ID})); err != nil {
		t.Fatalf("v1alpha1 Delete of a v1 allocation: %v", err)
	}
	if _, err := c.allocations.Get(ctx, &vpcpb.GetAllocationRequest{Id: v1ID}); codeOf(err) != codes.NotFound {
		t.Errorf("v1 Get after v1alpha1 Delete: %v, want NotFound", err)
	}
	if err := waitOK(ctx)(c.allocations.Delete(ctx, &vpcpb.DeleteAllocationRequest{Id: aID})); err != nil {
		t.Fatalf("v1 Delete of a v1alpha1 allocation: %v", err)
	}
	_, err = a.allocations.Get(ctx, &vpcv1alpha1pb.GetAllocationRequest{Id: aID})
	assertCode(t, err, codes.NotFound)
}
