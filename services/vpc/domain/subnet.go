package domain

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// SubnetService implements nebius.vpc.v1.SubnetService.
type SubnetService struct {
	*kind[*vpcpb.Subnet, *vpcpb.SubnetSpec]
}

func newSubnetService(c *core) *SubnetService {
	s := &SubnetService{}
	s.kind = &kind[*vpcpb.Subnet, *vpcpb.SubnetSpec]{
		c: c, prefix: KindSubnet, what: "subnet", repo: c.r.Subnets,
		newMsg: func() *vpcpb.Subnet { return &vpcpb.Subnet{} }, newSpec: func() *vpcpb.SubnetSpec { return &vpcpb.SubnetSpec{} },
		quota: ownQuota[*vpcpb.Subnet](quota.VPCSubnetCount), build: s.build, apply: s.apply, checkDelete: s.checkDelete, decorate: s.decorate,
	}
	return s
}

// ListByNetwork returns one page of the subnets of a network.
func (s *SubnetService) ListByNetwork(ctx context.Context, networkID string, pageSize int64, pageToken string) (Page[*vpcpb.Subnet], error) {
	return s.listWhere(ctx, func(sn *vpcpb.Subnet) bool { return sn.GetSpec().GetNetworkId() == networkID }, pageSize, pageToken)
}

func (s *SubnetService) build(ctx context.Context, md *commonpb.ResourceMetadata, spec *vpcpb.SubnetSpec) (*vpcpb.Subnet, error) {
	if err := s.c.checkProject(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	sn := &vpcpb.Subnet{Metadata: md, Spec: spec, Status: &vpcpb.SubnetStatus{State: vpcpb.SubnetStatus_READY}}
	if err := s.resolve(ctx, nil, sn); err != nil {
		return nil, err
	}
	return sn, nil
}

func (s *SubnetService) apply(ctx context.Context, stored, updated *vpcpb.Subnet) error {
	if stored.GetSpec().GetNetworkId() != updated.GetSpec().GetNetworkId() {
		return immutable("spec.network_id")
	}
	if err := s.resolve(ctx, stored, updated); err != nil {
		return err
	}
	return s.checkShrink(ctx, updated)
}

// family is one address family (private or public) of a subnet spec.
type family struct {
	vis        vpcpb.IpVisibility
	field      string
	useNetwork bool
	cidrs      []*vpcpb.SubnetCidr
	fields     []string // violation path of each cidr
}

// families normalizes the spec (NON_EMPTY_DEFAULT fields) and returns its families.
func families(spec *vpcpb.SubnetSpec) ([]family, error) {
	if spec.GetIpv4PrivatePools() == nil {
		spec.Ipv4PrivatePools = &vpcpb.IPv4PrivateSubnetPools{}
	}
	if spec.GetIpv4PublicPools() == nil {
		spec.Ipv4PublicPools = &vpcpb.IPv4PublicSubnetPools{}
	}
	priv, pub := spec.GetIpv4PrivatePools(), spec.GetIpv4PublicPools()
	out := []family{
		{vis: vpcpb.IpVisibility_PRIVATE, field: "spec.ipv4_private_pools"},
		{vis: vpcpb.IpVisibility_PUBLIC, field: "spec.ipv4_public_pools"},
	}
	pools := [][]*vpcpb.SubnetPool{priv.GetPools(), pub.GetPools()}
	use := []*bool{&priv.UseNetworkPools, &pub.UseNetworkPools}
	for i := range out {
		if err := out[i].normalize(pools[i], use[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (f *family) normalize(pools []*vpcpb.SubnetPool, use *bool) error {
	if len(pools) > 0 && *use {
		return errs.BadRequest(errs.Violation{Field: f.field + ".use_network_pools", Message: "must be false when pools are given"})
	}
	if len(pools) == 0 {
		*use = true
	}
	f.useNetwork = *use
	for pi, p := range pools {
		for ci, c := range p.GetCidrs() {
			if c.GetState() == vpcpb.AddressBlockState_STATE_UNSPECIFIED {
				c.State = vpcpb.AddressBlockState_AVAILABLE
			}
			if c.GetMaxMaskLength() == 0 {
				c.MaxMaskLength = ipv4Bits
			}
			f.cidrs = append(f.cidrs, c)
			f.fields = append(f.fields, fmt.Sprintf("%s.pools[%d].cidrs[%d].cidr", f.field, pi, ci))
		}
	}
	return nil
}

func (f *family) specs() []string {
	out := make([]string, 0, len(f.cidrs))
	for _, c := range f.cidrs {
		out = append(out, c.GetCidr())
	}
	return out
}

// flatCIDRs returns the deprecated flat status lists, in spec order.
func flatCIDRs(st *vpcpb.SubnetStatus, vis vpcpb.IpVisibility) []string {
	if vis == vpcpb.IpVisibility_PUBLIC {
		return st.GetIpv4PublicCidrs() //nolint:staticcheck // deprecated upstream but still served; it records the spec→block mapping
	}
	return st.GetIpv4PrivateCidrs() //nolint:staticcheck // deprecated upstream but still served; it records the spec→block mapping
}

func setFamilyStatus(st *vpcpb.SubnetStatus, vis vpcpb.IpVisibility, pools []*vpcpb.SubnetStatusPool, flat []string) {
	if vis == vpcpb.IpVisibility_PUBLIC {
		st.Ipv4PublicPools = pools
		st.Ipv4PublicCidrs = flat //nolint:staticcheck // deprecated upstream but still served
		return
	}
	st.Ipv4PrivatePools = pools
	st.Ipv4PrivateCidrs = flat //nolint:staticcheck // deprecated upstream but still served
}

// resolve normalizes sn's spec and computes its status; stored is the
// previous version (nil on create), whose blocks are kept where possible.
func (s *SubnetService) resolve(ctx context.Context, stored, sn *vpcpb.Subnet) error {
	n, err := ref(ctx, s.c.r.Networks, "spec.network_id", sn.GetSpec().GetNetworkId())
	if err != nil {
		return err
	}
	if err := sameParent("spec.network_id", sn.GetMetadata().GetParentId(), n); err != nil {
		return err
	}
	fams, err := families(sn.GetSpec())
	if err != nil {
		return err
	}
	if err := s.routeTable(ctx, n, sn); err != nil {
		return err
	}
	for _, f := range fams {
		pools, flat, err := s.familyStatus(ctx, stored, n, sn.GetMetadata().GetId(), f)
		if err != nil {
			return err
		}
		setFamilyStatus(sn.GetStatus(), f.vis, pools, flat)
	}
	return nil
}

func (s *SubnetService) routeTable(ctx context.Context, n *vpcpb.Network, sn *vpcpb.Subnet) error {
	rtID := sn.GetSpec().GetRouteTableId()
	if rtID == "" {
		sn.Status.RouteTable = &vpcpb.SubnetAssociatedRouteTable{Id: n.GetStatus().GetDefaultRouteTableId(), Default: true}
		return nil
	}
	rt, err := ref(ctx, s.c.r.RouteTables, "spec.route_table_id", rtID)
	if err != nil {
		return err
	}
	if rt.GetSpec().GetNetworkId() != n.GetMetadata().GetId() {
		return errs.BadRequest(errs.Violation{Field: "spec.route_table_id", Message: fmt.Sprintf("route table %s belongs to another network", rtID)})
	}
	sn.Status.RouteTable = &vpcpb.SubnetAssociatedRouteTable{Id: rtID}
	return nil
}

// familyStatus resolves one family: a subnet using network pools shares their
// whole space; otherwise each CIDR is placed in the network's pools.
func (s *SubnetService) familyStatus(ctx context.Context, stored *vpcpb.Subnet, n *vpcpb.Network, subnetID string, f family) ([]*vpcpb.SubnetStatusPool, []string, error) {
	blocks, pools, err := s.c.networkBlocks(ctx, n, f.vis)
	if err != nil {
		return nil, nil, err
	}
	if f.useNetwork {
		out := make([]*vpcpb.SubnetStatusPool, 0, len(pools))
		for _, p := range pools {
			out = append(out, &vpcpb.SubnetStatusPool{PoolId: p.GetMetadata().GetId(), Cidrs: cidrStrings(prefixes(poolBlocks(p)))})
		}
		return out, cidrStrings(prefixes(blocks)), nil
	}
	used, err := s.c.occupiedAll(ctx, pools, subnetID)
	if err != nil {
		return nil, nil, err
	}
	plan := planFor("subnet", blocks, used)
	if stored != nil {
		if prev, err := families(stored.GetSpec()); err == nil {
			plan.prev = newPrev(prev[slices.IndexFunc(prev, func(p family) bool { return p.vis == f.vis })].specs(), flatCIDRs(stored.GetStatus(), f.vis))
		}
	}
	got, err := plan.resolve(func(i int) string { return f.fields[i] }, f.specs(), ipv4Bits)
	if err != nil {
		return nil, nil, err
	}
	var out []*vpcpb.SubnetStatusPool
	for _, p := range pools {
		id := p.GetMetadata().GetId()
		var mine []netip.Prefix
		for _, g := range got {
			if poolOf(g, blocks) == id {
				mine = append(mine, g)
			}
		}
		if len(mine) > 0 {
			out = append(out, &vpcpb.SubnetStatusPool{PoolId: id, Cidrs: cidrStrings(mine)})
		}
	}
	return out, cidrStrings(got), nil
}

// checkShrink refuses dropping a block that still holds allocations.
func (s *SubnetService) checkShrink(ctx context.Context, sn *vpcpb.Subnet) error {
	space := slices.Concat(
		mustCIDRs(flatCIDRs(sn.GetStatus(), vpcpb.IpVisibility_PRIVATE)),
		mustCIDRs(flatCIDRs(sn.GetStatus(), vpcpb.IpVisibility_PUBLIC)),
	)
	allocs, err := s.c.r.Allocations.All(ctx, func(a *vpcpb.Allocation) bool {
		return a.GetStatus().GetDetails().GetSubnetId() == sn.GetMetadata().GetId()
	})
	if err != nil {
		return err
	}
	for _, a := range allocs {
		for _, p := range mustCIDRs([]string{a.GetStatus().GetDetails().GetAllocatedCidr()}) {
			if !contained(p, space) {
				return errs.BadState(sn.GetMetadata().GetId(), fmt.Sprintf(
					"allocation %s (%s) would fall outside the subnet", a.GetMetadata().GetId(), p))
			}
		}
	}
	return nil
}

func (s *SubnetService) checkDelete(ctx context.Context, sn *vpcpb.Subnet) error {
	id := sn.GetMetadata().GetId()
	allocs, err := s.c.r.Allocations.All(ctx, func(a *vpcpb.Allocation) bool { return a.GetStatus().GetDetails().GetSubnetId() == id })
	if err != nil {
		return err
	}
	if len(allocs) > 0 {
		return inUse(id, "subnet", idsOf(allocs))
	}
	return nil
}

// decorate refreshes the parts of the status that follow the network: shared
// pools and the default route table.
func (s *SubnetService) decorate(ctx context.Context, sn *vpcpb.Subnet) error {
	n, err := s.c.r.Networks.Get(ctx, sn.GetSpec().GetNetworkId())
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sn.GetStatus() == nil {
		sn.Status = &vpcpb.SubnetStatus{State: vpcpb.SubnetStatus_READY}
	}
	if sn.GetSpec().GetRouteTableId() == "" {
		sn.Status.RouteTable = &vpcpb.SubnetAssociatedRouteTable{Id: n.GetStatus().GetDefaultRouteTableId(), Default: true}
	}
	fams, err := families(sn.GetSpec())
	if err != nil {
		return nil //nolint:nilerr // a stored spec is already valid; nothing to refresh
	}
	for _, f := range fams {
		if !f.useNetwork {
			continue
		}
		pools, flat, err := s.familyStatus(ctx, nil, n, sn.GetMetadata().GetId(), f)
		if err != nil {
			return err
		}
		setFamilyStatus(sn.GetStatus(), f.vis, pools, flat)
	}
	return nil
}
