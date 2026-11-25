package domain

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// AllocationService implements nebius.vpc.v1.AllocationService.
type AllocationService struct {
	*kind[*vpcpb.Allocation, *vpcpb.AllocationSpec]
}

func newAllocationService(c *core) *AllocationService {
	s := &AllocationService{}
	s.kind = &kind[*vpcpb.Allocation, *vpcpb.AllocationSpec]{
		c: c, prefix: KindAllocation, what: "allocation", repo: c.r.Allocations,
		newMsg:  func() *vpcpb.Allocation { return &vpcpb.Allocation{} },
		newSpec: func() *vpcpb.AllocationSpec { return &vpcpb.AllocationSpec{} },
		build:   s.build, apply: s.apply, checkDelete: s.checkDelete,
	}
	return s
}

// ListBySubnet returns one page of the allocations made in a subnet.
func (s *AllocationService) ListBySubnet(ctx context.Context, subnetID string, pageSize int64, pageToken string) (Page[*vpcpb.Allocation], error) {
	return s.listWhere(ctx, func(a *vpcpb.Allocation) bool { return a.GetStatus().GetDetails().GetSubnetId() == subnetID }, pageSize, pageToken)
}

// ListByPool returns one page of the allocations made from a pool.
func (s *AllocationService) ListByPool(ctx context.Context, poolID string, pageSize int64, pageToken string) (Page[*vpcpb.Allocation], error) {
	return s.listWhere(ctx, func(a *vpcpb.Allocation) bool { return a.GetStatus().GetDetails().GetPoolId() == poolID }, pageSize, pageToken)
}

func (s *AllocationService) build(ctx context.Context, md *commonpb.ResourceMetadata, spec *vpcpb.AllocationSpec) (*vpcpb.Allocation, error) {
	if err := s.c.checkProject(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	a := &vpcpb.Allocation{Metadata: md, Spec: spec}
	var req addressRequest
	switch {
	case spec.GetIpv4Private() != nil:
		p := spec.GetIpv4Private()
		req = addressRequest{field: "spec.ipv4_private", cidr: p.GetCidr(), subnetID: p.GetSubnetId(), poolID: p.GetPoolId(), vis: vpcpb.IpVisibility_PRIVATE}
	case spec.GetIpv4Public() != nil:
		p := spec.GetIpv4Public()
		req = addressRequest{field: "spec.ipv4_public", cidr: p.GetCidr(), subnetID: p.GetSubnetId(), poolID: p.GetPoolId(), vis: vpcpb.IpVisibility_PUBLIC}
	default:
		return nil, errs.BadRequest(errs.Violation{Field: "spec", Message: "one of ipv4_private or ipv4_public is required"})
	}
	details, err := s.place(ctx, a, req)
	if err != nil {
		return nil, err
	}
	a.Status = &vpcpb.AllocationStatus{State: vpcpb.AllocationStatus_ALLOCATED, Details: details, Static: true}
	return a, nil
}

type addressRequest struct {
	field            string
	cidr             string
	subnetID, poolID string
	vis              vpcpb.IpVisibility
}

// place picks the address of a new allocation.
func (s *AllocationService) place(ctx context.Context, a *vpcpb.Allocation, req addressRequest) (*vpcpb.AllocationDetails, error) {
	parent, id := a.GetMetadata().GetParentId(), a.GetMetadata().GetId()
	details := &vpcpb.AllocationDetails{Version: vpcpb.IpVersion_IPV4}
	var (
		blocks []block
		used   []netip.Prefix
		err    error
	)
	switch {
	case req.subnetID != "":
		var sn *vpcpb.Subnet
		if sn, err = ref(ctx, s.c.r.Subnets, req.field+".subnet_id", req.subnetID); err != nil {
			return nil, err
		}
		if err := sameParent(req.field+".subnet_id", parent, sn); err != nil {
			return nil, err
		}
		blocks, used, err = s.subnetSpace(ctx, sn, req.vis, id)
		details.SubnetId = req.subnetID
	case req.poolID != "":
		blocks, used, err = s.poolSpace(ctx, req.field+".pool_id", req.poolID, req.vis, id)
	case req.vis == vpcpb.IpVisibility_PUBLIC:
		var p *vpcpb.Pool
		if p, err = s.c.r.Pools.GetByName(ctx, parent, DefaultPublicPoolName); err != nil {
			return nil, fmt.Errorf("default public pool of %s: %w", parent, err)
		}
		blocks, used, err = s.poolSpace(ctx, req.field+".pool_id", p.GetMetadata().GetId(), req.vis, id)
	default:
		return nil, errs.BadRequest(errs.Violation{Field: req.field, Message: "one of subnet_id or pool_id is required"})
	}
	if err != nil {
		return nil, err
	}
	plan := planFor("allocation", blocks, used)
	got, err := plan.resolve(func(int) string { return req.field + ".cidr" }, []string{req.cidr}, ipv4Bits)
	if err != nil {
		return nil, err
	}
	details.AllocatedCidr = got[0].String()
	details.PoolId = poolOf(got[0], blocks)
	return details, nil
}

func (s *AllocationService) poolSpace(ctx context.Context, field, poolID string, vis vpcpb.IpVisibility, except string) ([]block, []netip.Prefix, error) {
	p, err := ref(ctx, s.c.r.Pools, field, poolID)
	if err != nil {
		return nil, nil, err
	}
	if p.GetSpec().GetVisibility() != vis {
		return nil, nil, errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("pool %s is not %s", poolID, vis)})
	}
	used, err := s.c.occupied(ctx, poolID, except)
	if err != nil {
		return nil, nil, err
	}
	return poolBlocks(p), used, nil
}

// subnetSpace returns where a subnet hands out addresses of one visibility,
// and what is taken there (including reserved network/gateway/broadcast).
func (s *AllocationService) subnetSpace(ctx context.Context, sn *vpcpb.Subnet, vis vpcpb.IpVisibility, except string) ([]block, []netip.Prefix, error) {
	n, err := s.c.r.Networks.Get(ctx, sn.GetSpec().GetNetworkId())
	if err != nil {
		return nil, nil, err
	}
	fams, err := families(proto.CloneOf(sn.GetSpec()))
	if err != nil {
		return nil, nil, err
	}
	var (
		blocks []block
		used   []netip.Prefix
	)
	if i := slices.IndexFunc(fams, func(f family) bool { return f.vis == vis }); fams[i].useNetwork {
		var pools []*vpcpb.Pool
		if blocks, pools, err = s.c.networkBlocks(ctx, n, vis); err == nil {
			used, err = s.c.occupiedAll(ctx, pools, except)
		}
	} else {
		blocks, used, err = s.ownBlocks(ctx, sn, fams[i], except)
	}
	if err != nil {
		return nil, nil, err
	}
	for _, b := range blocks {
		used = append(used, reservedAddrs(b.prefix)...)
	}
	return blocks, used, nil
}

// ownBlocks returns the explicit, AVAILABLE blocks of a subnet family (with
// their max_mask_length) and the allocations made in their pools.
func (s *AllocationService) ownBlocks(ctx context.Context, sn *vpcpb.Subnet, f family, except string) ([]block, []netip.Prefix, error) {
	statusPools := sn.GetStatus().GetIpv4PrivatePools()
	if f.vis == vpcpb.IpVisibility_PUBLIC {
		statusPools = sn.GetStatus().GetIpv4PublicPools()
	}
	var all []block
	for _, sp := range statusPools {
		for _, p := range mustCIDRs(sp.GetCidrs()) {
			all = append(all, block{prefix: p, poolID: sp.GetPoolId()})
		}
	}
	var (
		blocks []block
		used   []netip.Prefix
		pools  = map[string]bool{}
	)
	// The flat status list is in spec order, so entry i resolves f.cidrs[i].
	for i, p := range mustCIDRs(flatCIDRs(sn.GetStatus(), f.vis)) {
		if i >= len(f.cidrs) || f.cidrs[i].GetState() == vpcpb.AddressBlockState_DISABLED {
			continue
		}
		b := block{prefix: p, poolID: poolOf(p, all), maxMask: f.cidrs[i].GetMaxMaskLength()}
		blocks = append(blocks, b)
		if !pools[b.poolID] {
			pools[b.poolID] = true
			taken, err := s.c.allocationsIn(ctx, b.poolID, except)
			if err != nil {
				return nil, nil, err
			}
			used = append(used, taken...)
		}
	}
	return blocks, used, nil
}

func (s *AllocationService) apply(_ context.Context, stored, updated *vpcpb.Allocation) error {
	if !proto.Equal(stored.GetSpec(), updated.GetSpec()) {
		return immutable("spec")
	}
	return nil
}

func (s *AllocationService) checkDelete(ctx context.Context, a *vpcpb.Allocation) error {
	id := a.GetMetadata().GetId()
	if a.GetStatus().GetAssignment() != nil {
		return errs.BadState(id, "allocation is assigned; detach it first")
	}
	routes, err := s.c.r.Routes.All(ctx, func(r *vpcpb.Route) bool { return r.GetSpec().GetNextHop().GetAllocation().GetId() == id })
	if err != nil {
		return err
	}
	if len(routes) > 0 {
		return inUse(id, "allocation", idsOf(routes))
	}
	return nil
}
