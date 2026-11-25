package domain

import (
	"testing"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// subnetWithBlocks creates a subnet in the default network from explicit
// SubnetCidr entries (state and max_mask_length included).
func (e *env) subnetWithBlocks(t *testing.T, name string, cidrs ...*vpcpb.SubnetCidr) *vpcpb.Subnet {
	t.Helper()
	spec := &vpcpb.SubnetSpec{
		NetworkId:        e.def.Network.GetMetadata().GetId(),
		Ipv4PrivatePools: &vpcpb.IPv4PrivateSubnetPools{Pools: []*vpcpb.SubnetPool{{Cidrs: cidrs}}},
	}
	return mustCreate(t, e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, name, spec)
}

func TestSubnetMaxMaskLengthBoundsAllocations(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.subnetWithBlocks(t, "wide", &vpcpb.SubnetCidr{Cidr: "10.0.0.0/24", MaxMaskLength: 28})

	_, err := e.tryAlloc(t, "single", privateAllocSpec(idOf(sn), ""))
	wantKind(t, "a /32 from a block limited to /28", err, errs.KindBadRequest)
	_, err = e.tryAlloc(t, "explicit", privateAllocSpec(idOf(sn), "10.0.0.17"))
	wantKind(t, "an explicit /32 from a block limited to /28", err, errs.KindBadRequest)

	a := e.createAlloc(t, "block", idOf(sn), "/28")
	if got := a.GetStatus().GetDetails().GetAllocatedCidr(); got != "10.0.0.16/28" {
		t.Errorf("allocated = %s, want 10.0.0.16/28 (the first /28 holds reserved addresses)", got)
	}
}

func TestSubnetDisabledBlockIsNotAllocated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.subnetWithBlocks(t, "mixed",
		&vpcpb.SubnetCidr{Cidr: "10.0.0.0/24", State: vpcpb.AddressBlockState_DISABLED},
		&vpcpb.SubnetCidr{Cidr: "10.0.1.0/24"},
	)
	if got := e.createAlloc(t, "a", idOf(sn), "").GetStatus().GetDetails().GetAllocatedCidr(); got != "10.0.1.2/32" {
		t.Errorf("allocated = %s, want 10.0.1.2/32 from the available block", got)
	}
	_, err := e.tryAlloc(t, "b", privateAllocSpec(idOf(sn), "10.0.0.9"))
	wantKind(t, "an address in a disabled block", err, errs.KindBadRequest)
}

func TestPoolMaxMaskLengthBoundsChildBlocks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := mustCreate(t, e.svc.Pools().Create, e.svc.Pools().Get, testProject, "root", &vpcpb.PoolSpec{
		Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: "172.16.0.0/16", MaxMaskLength: 20}},
	})
	child := func(cidr string) *vpcpb.PoolSpec {
		return &vpcpb.PoolSpec{
			SourcePoolId: idOf(root), Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
			Cidrs: []*vpcpb.PoolCidr{{Cidr: cidr}},
		}
	}
	_, err := tryCreate(t.Context(), e.svc.Pools().Create, e.svc.Pools().Get, testProject, "small", child("/24"))
	wantKind(t, "a /24 child of a block limited to /20", err, errs.KindBadRequest)
	p := mustCreate(t, e.svc.Pools().Create, e.svc.Pools().Get, testProject, "ok", child("/20"))
	if got := p.GetStatus().GetCidrs(); len(got) != 1 || got[0] != "172.16.0.0/20" {
		t.Errorf("child cidrs = %v, want [172.16.0.0/20]", got)
	}
}
