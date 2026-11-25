package domain

import (
	"context"
	"net/netip"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// Address-space bookkeeping. A pool's CIDRs are consumed by child pools,
// subnets that reserve explicit blocks, and allocations. Subnets that use
// their network's pools share the pools' space instead of consuming it.

// block is an address block and the pool it comes from.
type block struct {
	prefix netip.Prefix
	poolID string
	// maxMask is the longest prefix that may be carved from the block
	// (spec max_mask_length); 0 means no limit beyond /32.
	maxMask int64
}

// planFor is a blockPlan over blocks, honoring each block's max_mask_length.
func planFor(what string, blocks []block, used []netip.Prefix) blockPlan {
	limits := map[netip.Prefix]int64{}
	for _, b := range blocks {
		if b.maxMask > 0 {
			limits[b.prefix] = b.maxMask
		}
	}
	return blockPlan{what: what, space: prefixes(blocks), used: used, maxMask: limits}
}

func prefixes(bs []block) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.prefix)
	}
	return out
}

// poolOf returns the pool of the block that contains p.
func poolOf(p netip.Prefix, bs []block) string {
	for _, b := range bs {
		if b.prefix.Bits() <= p.Bits() && b.prefix.Contains(p.Addr()) {
			return b.poolID
		}
	}
	return ""
}

// poolBlocks returns the pool's resolved CIDRs that accept new allocations.
func poolBlocks(p *vpcpb.Pool) []block {
	resolved := mustCIDRs(p.GetStatus().GetCidrs())
	var out []block
	for i, pc := range resolved {
		b := block{prefix: pc, poolID: p.GetMetadata().GetId()}
		if i < len(p.GetSpec().GetCidrs()) {
			spec := p.GetSpec().GetCidrs()[i]
			if spec.GetState() == vpcpb.AddressBlockState_DISABLED {
				continue
			}
			b.maxMask = spec.GetMaxMaskLength()
		}
		out = append(out, b)
	}
	return out
}

// carved returns the explicit blocks a subnet reserves, by pool ID.
func carved(s *vpcpb.Subnet) map[string][]netip.Prefix {
	out := map[string][]netip.Prefix{}
	add := func(useNetwork bool, pools []*vpcpb.SubnetStatusPool) {
		if useNetwork {
			return
		}
		for _, sp := range pools {
			out[sp.GetPoolId()] = append(out[sp.GetPoolId()], mustCIDRs(sp.GetCidrs())...)
		}
	}
	add(s.GetSpec().GetIpv4PrivatePools().GetUseNetworkPools(), s.GetStatus().GetIpv4PrivatePools())
	add(s.GetSpec().GetIpv4PublicPools().GetUseNetworkPools(), s.GetStatus().GetIpv4PublicPools())
	return out
}

// occupied returns everything carved out of a pool, ignoring resource except.
func (c *core) occupied(ctx context.Context, poolID, except string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	children, err := c.r.Pools.All(ctx, func(p *vpcpb.Pool) bool {
		return p.GetSpec().GetSourcePoolId() == poolID && p.GetMetadata().GetId() != except
	})
	if err != nil {
		return nil, err
	}
	for _, p := range children {
		out = append(out, mustCIDRs(p.GetStatus().GetCidrs())...)
	}
	subnets, err := c.r.Subnets.All(ctx, func(s *vpcpb.Subnet) bool { return s.GetMetadata().GetId() != except })
	if err != nil {
		return nil, err
	}
	for _, s := range subnets {
		out = append(out, carved(s)[poolID]...)
	}
	allocs, err := c.allocationsIn(ctx, poolID, except)
	if err != nil {
		return nil, err
	}
	return append(out, allocs...), nil
}

// allocationsIn returns the CIDRs of the allocations made from a pool.
func (c *core) allocationsIn(ctx context.Context, poolID, except string) ([]netip.Prefix, error) {
	allocs, err := c.r.Allocations.All(ctx, func(a *vpcpb.Allocation) bool {
		return a.GetStatus().GetDetails().GetPoolId() == poolID && a.GetMetadata().GetId() != except
	})
	if err != nil {
		return nil, err
	}
	out := make([]netip.Prefix, 0, len(allocs))
	for _, a := range allocs {
		out = append(out, mustCIDRs([]string{a.GetStatus().GetDetails().GetAllocatedCidr()})...)
	}
	return out, nil
}

// loadPools loads pools by ID.
func (c *core) loadPools(ctx context.Context, field string, poolIDs []string) ([]*vpcpb.Pool, error) {
	out := make([]*vpcpb.Pool, 0, len(poolIDs))
	for _, id := range poolIDs {
		p, err := ref(ctx, c.r.Pools, field, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// networkPoolIDs returns the IDs of a network's private or public pools.
func networkPoolIDs(n *vpcpb.Network, vis vpcpb.IpVisibility) []string {
	pools := n.GetSpec().GetIpv4PrivatePools().GetPools()
	if vis == vpcpb.IpVisibility_PUBLIC {
		pools = n.GetSpec().GetIpv4PublicPools().GetPools()
	}
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.GetId())
	}
	return out
}

// networkBlocks returns the usable blocks of a network's pools of one visibility.
func (c *core) networkBlocks(ctx context.Context, n *vpcpb.Network, vis vpcpb.IpVisibility) ([]block, []*vpcpb.Pool, error) {
	pools, err := c.loadPools(ctx, "spec.network_id", networkPoolIDs(n, vis))
	if err != nil {
		return nil, nil, err
	}
	var out []block
	for _, p := range pools {
		out = append(out, poolBlocks(p)...)
	}
	return out, pools, nil
}

// occupiedAll is occupied over several pools.
func (c *core) occupiedAll(ctx context.Context, pools []*vpcpb.Pool, except string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, p := range pools {
		used, err := c.occupied(ctx, p.GetMetadata().GetId(), except)
		if err != nil {
			return nil, err
		}
		out = append(out, used...)
	}
	return out, nil
}
