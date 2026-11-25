package domain

import (
	"testing"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

func TestNICBindPoolAllocationInSubnetSpace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "np") // uses the network's pools
	a := mustCreate(t, e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, "pool-only", &vpcpb.AllocationSpec{
		IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
			Cidr: "10.0.9.9/32", Pool: &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: idOf(e.poolByName(t, DefaultPrivatePoolName))},
		}},
	})
	b := e.nicBinding(sn, "eth0")
	b.PrivateAllocationID = idOf(a)
	if got := e.bind(t, b).PrivateAddress; got != "10.0.9.9" {
		t.Errorf("address = %s", got)
	}
	wantAssigned(t, e.alloc(t, idOf(a)), vpcpb.NetworkInterfaceAssignment_PRIMARY, idOf(sn))
	if err := e.svc.NICs().ReleaseInterface(t.Context(), nicInstance, "eth0"); err != nil {
		t.Fatal(err)
	}
	if got := e.alloc(t, idOf(a)).GetStatus().GetDetails().GetSubnetId(); got != "" {
		t.Errorf("subnet_id after release = %q, want cleared (spec has no subnet_id)", got)
	}
}
