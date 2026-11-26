package api

import (
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"
)

// alphaPool converts a stored v1 pool to its v1alpha1 view. v1 visibility and
// assignment have no v1alpha1 field. A pool with no source pool is a root
// pool, whose source in v1alpha1 is its scope.
func alphaPool(p *vpcpb.Pool) *vpcv1alpha1pb.Pool {
	if p == nil {
		return nil
	}
	spec := &vpcv1alpha1pb.PoolSpec{Version: vpcv1alpha1pb.IpVersion(p.GetSpec().GetVersion())}
	if src := p.GetSpec().GetSourcePoolId(); src != "" {
		spec.Source = &vpcv1alpha1pb.PoolSpec_SourcePoolId{SourcePoolId: src}
	} else {
		spec.Source = &vpcv1alpha1pb.PoolSpec_SourceScopeId{SourceScopeId: p.GetStatus().GetScopeId()}
	}
	for _, c := range p.GetSpec().GetCidrs() {
		spec.Cidrs = append(spec.Cidrs, &vpcv1alpha1pb.PoolCidr{
			Cidr:        c.GetCidr(),
			State:       vpcv1alpha1pb.PoolCidrState(c.GetState()),
			AllowedMask: c.GetMaxMaskLength(),
		})
	}
	return &vpcv1alpha1pb.Pool{
		Metadata: p.GetMetadata(),
		Spec:     spec,
		Status: &vpcv1alpha1pb.PoolStatus{
			State:   vpcv1alpha1pb.PoolStatus_State(p.GetStatus().GetState()),
			Cidrs:   p.GetStatus().GetCidrs(),
			ScopeId: p.GetStatus().GetScopeId(),
		},
	}
}

// alphaNetwork converts a stored v1 network to its v1alpha1 view. scopeID is
// the scope of the network's private pools. Public pools have no v1alpha1 field.
func alphaNetwork(n *vpcpb.Network, scopeID string) *vpcv1alpha1pb.Network {
	if n == nil {
		return nil
	}
	spec := &vpcv1alpha1pb.NetworkSpec{}
	for _, p := range n.GetSpec().GetIpv4PrivatePools().GetPools() {
		spec.Pools = append(spec.Pools, &vpcv1alpha1pb.NetworkPool{PoolId: p.GetId()})
	}
	return &vpcv1alpha1pb.Network{
		Metadata: n.GetMetadata(),
		Spec:     spec,
		Status: &vpcv1alpha1pb.NetworkStatus{
			State:   vpcv1alpha1pb.NetworkStatus_State(n.GetStatus().GetState()),
			Default: n.GetStatus().GetDefault(),
			ScopeId: scopeID,
		},
	}
}

// alphaSubnet converts a stored v1 subnet to its v1alpha1 view. v1 has no
// egress NAT setting, so enable_egress_nat is always false.
func alphaSubnet(sn *vpcpb.Subnet) *vpcv1alpha1pb.Subnet {
	if sn == nil {
		return nil
	}
	pools := sn.GetSpec().GetIpv4PrivatePools()
	spec := &vpcv1alpha1pb.SubnetSpec{NetworkId: sn.GetSpec().GetNetworkId()}
	if pools.GetUseNetworkPools() {
		for _, p := range sn.GetStatus().GetIpv4PrivatePools() {
			spec.Pools = append(spec.Pools, &vpcv1alpha1pb.SubnetPool{
				Pool: &vpcv1alpha1pb.SubnetPool_PoolId{PoolId: p.GetPoolId()},
			})
		}
	} else {
		for _, p := range pools.GetPools() {
			ps := &vpcv1alpha1pb.SubnetPoolSpec{Version: vpcv1alpha1pb.IpVersion_IPV4}
			for _, c := range p.GetCidrs() {
				ps.Cidrs = append(ps.Cidrs, &vpcv1alpha1pb.SubnetCidr{
					Cidr:        c.GetCidr(),
					State:       vpcv1alpha1pb.PoolCidrState(c.GetState()),
					AllowedMask: c.GetMaxMaskLength(),
				})
			}
			spec.Pools = append(spec.Pools, &vpcv1alpha1pb.SubnetPool{Pool: &vpcv1alpha1pb.SubnetPool_Spec{Spec: ps}})
		}
	}
	var cidrs []string
	for _, p := range sn.GetStatus().GetIpv4PrivatePools() {
		cidrs = append(cidrs, p.GetCidrs()...)
	}
	return &vpcv1alpha1pb.Subnet{
		Metadata: sn.GetMetadata(),
		Spec:     spec,
		Status: &vpcv1alpha1pb.SubnetStatus{
			State:     vpcv1alpha1pb.SubnetStatus_State(sn.GetStatus().GetState()),
			Ipv4Cidrs: cidrs,
			Default:   sn.GetStatus().GetDefault(),
		},
	}
}

// alphaAllocation converts a stored v1 allocation to its v1alpha1 view. Empty
// spec fields are kept exactly as stored: Terraform round-trips the spec.
func alphaAllocation(a *vpcpb.Allocation) *vpcv1alpha1pb.Allocation {
	if a == nil {
		return nil
	}
	out := &vpcv1alpha1pb.Allocation{Metadata: a.GetMetadata()}
	if a.GetSpec() != nil {
		out.Spec = &vpcv1alpha1pb.AllocationSpec{}
		switch ip := a.GetSpec().GetIpSpec().(type) {
		case *vpcpb.AllocationSpec_Ipv4Private:
			p := &vpcv1alpha1pb.IPv4PrivateAllocationSpec{Cidr: ip.Ipv4Private.GetCidr()}
			switch pool := ip.Ipv4Private.GetPool().(type) {
			case *vpcpb.IPv4PrivateAllocationSpec_SubnetId:
				p.Pool = &vpcv1alpha1pb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: pool.SubnetId}
			case *vpcpb.IPv4PrivateAllocationSpec_PoolId:
				p.Pool = &vpcv1alpha1pb.IPv4PrivateAllocationSpec_PoolId{PoolId: pool.PoolId}
			}
			out.Spec.IpSpec = &vpcv1alpha1pb.AllocationSpec_Ipv4Private{Ipv4Private: p}
		case *vpcpb.AllocationSpec_Ipv4Public:
			// A v1 public allocation may name a subnet instead of a pool; v1alpha1
			// has no such field, so the subnet is dropped.
			out.Spec.IpSpec = &vpcv1alpha1pb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcv1alpha1pb.IPv4PublicAllocationSpec{
				Cidr: ip.Ipv4Public.GetCidr(), PoolId: ip.Ipv4Public.GetPoolId(),
			}}
		}
	}
	if st := a.GetStatus(); st != nil {
		out.Status = &vpcv1alpha1pb.AllocationStatus{
			State:  vpcv1alpha1pb.AllocationStatus_State(st.GetState()),
			Static: st.GetStatic(),
		}
		if d := st.GetDetails(); d != nil {
			out.Status.Details = &vpcv1alpha1pb.AllocationDetails{
				AllocatedCidr: d.GetAllocatedCidr(),
				PoolId:        d.GetPoolId(),
				Version:       vpcv1alpha1pb.IpVersion(d.GetVersion()),
			}
		}
		switch as := st.GetAssignment().GetType().(type) {
		case *vpcpb.Assignment_NetworkInterface:
			out.Status.Assignment = &vpcv1alpha1pb.Assignment{Type: &vpcv1alpha1pb.Assignment_NetworkInterface{
				NetworkInterface: &vpcv1alpha1pb.NetworkInterfaceAssignment{
					InstanceId: as.NetworkInterface.GetInstanceId(),
					Name:       as.NetworkInterface.GetName(),
					Type:       vpcv1alpha1pb.NetworkInterfaceAssignment_Type(as.NetworkInterface.GetType()),
				},
			}}
		case *vpcpb.Assignment_LoadBalancer:
			out.Status.Assignment = &vpcv1alpha1pb.Assignment{Type: &vpcv1alpha1pb.Assignment_LoadBalancer{
				LoadBalancer: &vpcv1alpha1pb.LoadBalancerAssignment{Id: as.LoadBalancer.GetId()},
			}}
		}
	}
	return out
}

// v1AllocationSpec converts a v1alpha1 allocation spec to the stored v1 form.
// A nil spec stays nil: the domain decides what that means.
func v1AllocationSpec(s *vpcv1alpha1pb.AllocationSpec) *vpcpb.AllocationSpec {
	if s == nil {
		return nil
	}
	out := &vpcpb.AllocationSpec{}
	switch ip := s.GetIpSpec().(type) {
	case *vpcv1alpha1pb.AllocationSpec_Ipv4Private:
		p := &vpcpb.IPv4PrivateAllocationSpec{Cidr: ip.Ipv4Private.GetCidr()}
		switch pool := ip.Ipv4Private.GetPool().(type) {
		case *vpcv1alpha1pb.IPv4PrivateAllocationSpec_SubnetId:
			p.Pool = &vpcpb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: pool.SubnetId}
		case *vpcv1alpha1pb.IPv4PrivateAllocationSpec_PoolId:
			p.Pool = &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: pool.PoolId}
		}
		out.IpSpec = &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: p}
	case *vpcv1alpha1pb.AllocationSpec_Ipv4Public:
		p := &vpcpb.IPv4PublicAllocationSpec{Cidr: ip.Ipv4Public.GetCidr()}
		if id := ip.Ipv4Public.GetPoolId(); id != "" {
			p.Pool = &vpcpb.IPv4PublicAllocationSpec_PoolId{PoolId: id}
		}
		out.IpSpec = &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: p}
	}
	return out
}
