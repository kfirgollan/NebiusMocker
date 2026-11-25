package domain

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func privatePoolAllocSpec(poolID string) *vpcpb.AllocationSpec {
	return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
		Pool: &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: poolID},
	}}}
}

func publicPoolAllocSpec(poolID string) *vpcpb.AllocationSpec {
	return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{
		Pool: &vpcpb.IPv4PublicAllocationSpec_PoolId{PoolId: poolID},
	}}}
}

func (e *env) updateAlloc(t *testing.T, a *vpcpb.Allocation, spec *vpcpb.AllocationSpec) (*vpcpb.Allocation, error) {
	t.Helper()
	return tryUpdate(t.Context(), e.svc.Allocations().Update, e.svc.Allocations().Get, UpdateCmd[*vpcpb.AllocationSpec]{
		Metadata: &commonpb.ResourceMetadata{Id: idOf(a), Name: a.GetMetadata().GetName()}, Spec: spec,
	})
}

// TestAllocationSubnetToOwnPool covers the one spec change upstream allows:
// subnet_id to a pool_id equal to status.details.pool_id. The address stays.
func TestAllocationSubnetToOwnPool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "sn", "/24")
	a := e.createAlloc(t, "a", idOf(sn), "")
	own := a.GetStatus().GetDetails().GetPoolId()

	got, err := e.updateAlloc(t, a, privatePoolAllocSpec(own))
	if err != nil {
		t.Fatalf("subnet_id → own pool_id: %v", err)
	}
	if got.GetSpec().GetIpv4Private().GetPoolId() != own || got.GetSpec().GetIpv4Private().GetSubnetId() != "" {
		t.Errorf("spec = %v, want pool_id %s", got.GetSpec(), own)
	}
	// Same address and pool; no longer associated with the subnet.
	want := proto.CloneOf(a.GetStatus())
	want.Details.SubnetId = ""
	if diff := cmp.Diff(want, got.GetStatus(), protocmp.Transform()); diff != "" {
		t.Errorf("status (-want +got):\n%s", diff)
	}
	if got.GetMetadata().GetResourceVersion() != a.GetMetadata().GetResourceVersion()+1 {
		t.Errorf("resource_version = %d, want %d", got.GetMetadata().GetResourceVersion(), a.GetMetadata().GetResourceVersion()+1)
	}

	// The same spec again is a no-op.
	again, err := e.updateAlloc(t, got, privatePoolAllocSpec(own))
	if err != nil || again.GetMetadata().GetResourceVersion() != got.GetMetadata().GetResourceVersion() {
		t.Errorf("repeated update: version %d, err %v; want unchanged", again.GetMetadata().GetResourceVersion(), err)
	}

	// The address is still held: the next allocation in the subnet gets another one.
	next := e.createAlloc(t, "b", idOf(sn), "")
	if next.GetStatus().GetDetails().GetAllocatedCidr() == a.GetStatus().GetDetails().GetAllocatedCidr() {
		t.Errorf("address %s handed out twice", next.GetStatus().GetDetails().GetAllocatedCidr())
	}

	// Still available to the subnet: unassigned, inside its space.
	page, err := e.svc.Allocations().ListBySubnet(t.Context(), idOf(sn), 0, "")
	if err != nil || len(page.Items) != 2 {
		t.Errorf("ListBySubnet = %d items (err %v), want a and b", len(page.Items), err)
	}
	if err := tryDelete(t.Context(), e.svc.Allocations().Delete, idOf(next)); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	// The subnet no longer pins the allocation, and deleting it keeps the address.
	if err := tryDelete(t.Context(), e.svc.Subnets().Delete, idOf(sn)); err != nil {
		t.Fatalf("delete the subnet after the allocation moved to its pool: %v", err)
	}
	kept, err := e.svc.Allocations().Get(t.Context(), idOf(a))
	if err != nil || kept.GetStatus().GetDetails().GetAllocatedCidr() != a.GetStatus().GetDetails().GetAllocatedCidr() {
		t.Errorf("allocation after subnet delete = %v (err %v), want the same address", kept.GetStatus(), err)
	}
	// A new subnet carved from the pool does not take the held address.
	sn2 := e.createSubnet(t, "sn2", "/24")
	c := e.createAlloc(t, "c", idOf(sn2), "")
	if c.GetStatus().GetDetails().GetAllocatedCidr() == a.GetStatus().GetDetails().GetAllocatedCidr() {
		t.Errorf("address %s handed out twice after the subnet was recreated", a.GetStatus().GetDetails().GetAllocatedCidr())
	}
}

// TestRouteNextHopFromPoolAllocation: an allocation that came from a pool of
// the route table's network is a valid next hop, one from another pool is not.
func TestRouteNextHopFromPoolAllocation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "sn", "/24")
	a := e.createAlloc(t, "a", idOf(sn), "")
	if _, err := e.updateAlloc(t, a, privatePoolAllocSpec(a.GetStatus().GetDetails().GetPoolId())); err != nil {
		t.Fatalf("subnet_id → pool_id: %v", err)
	}
	other := mustCreate(t, e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, "other",
		privatePoolAllocSpec(idOf(newRootPool(t, e, "10.9.0.0/24"))))
	rt := e.createRouteTable(t, "rt", idOf(e.def.Network))
	hop := func(dst, id string) *vpcpb.RouteSpec {
		return &vpcpb.RouteSpec{
			Destination: &vpcpb.DestinationMatch{Cidr: dst},
			NextHop:     &vpcpb.NextHop{NextHop: &vpcpb.NextHop_Allocation{Allocation: &vpcpb.AllocationNextHop{Id: id}}},
		}
	}
	if _, err := tryCreate(t.Context(), e.svc.Routes().Create, e.svc.Routes().Get, idOf(rt), "r", hop("10.77.0.0/16", idOf(a))); err != nil {
		t.Errorf("route via an allocation from the network's pool: %v", err)
	}
	_, err := tryCreate(t.Context(), e.svc.Routes().Create, e.svc.Routes().Get, idOf(rt), "r2", hop("10.78.0.0/16", idOf(other)))
	wantKind(t, "route via an allocation from a pool outside the network", err, errs.KindBadRequest)
}

// TestAllocationPublicDefaultPoolToOwnPool names the pool that a public
// allocation without a pool was placed in.
func TestAllocationPublicDefaultPoolToOwnPool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := mustCreate(t, e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, "pub", publicAllocSpec(""))
	own := a.GetStatus().GetDetails().GetPoolId()
	got, err := e.updateAlloc(t, a, publicPoolAllocSpec(own))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.GetSpec().GetIpv4Public().GetPoolId() != own || got.GetStatus().GetDetails().GetAllocatedCidr() != a.GetStatus().GetDetails().GetAllocatedCidr() {
		t.Errorf("allocation = %v, want pool_id %s and the same address", got, own)
	}
}

// TestAllocationForbiddenSpecChanges checks that every transition upstream
// forbids is refused with InvalidArgument on the right field, and that a
// refused update leaves the stored allocation exactly as it was.
func TestAllocationForbiddenSpecChanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "sn", "/24")
	sn2 := e.createSubnet(t, "sn2", "/24")
	other := newRootPool(t, e, "10.9.0.0/24")
	pub := idOf(e.poolByName(t, DefaultPublicPoolName))
	bySubnet := e.createAlloc(t, "by-subnet", idOf(sn), "")
	own := bySubnet.GetStatus().GetDetails().GetPoolId()
	byPool := mustCreate(t, e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, "by-pool", privatePoolAllocSpec(own))

	tests := []struct {
		name  string
		a     *vpcpb.Allocation
		spec  *vpcpb.AllocationSpec
		field string
	}{
		{"another subnet", bySubnet, privateAllocSpec(idOf(sn2), ""), "spec.ipv4_private.subnet_id"},
		{"a pool other than status.details.pool_id", bySubnet, privatePoolAllocSpec(idOf(other)), "spec.ipv4_private.pool_id"},
		{"pool_id back to subnet_id", byPool, privateAllocSpec(idOf(sn), ""), "spec.ipv4_private.subnet_id"},
		{"pool to another pool", byPool, privatePoolAllocSpec(idOf(other)), "spec.ipv4_private.pool_id"},
		{"pool oneof cleared", bySubnet, privateAllocSpec("", ""), "spec.ipv4_private.subnet_id"},
		{"cidr", bySubnet, privateAllocSpec(idOf(sn), "/31"), "spec.ipv4_private.cidr"},
		{"private to public", bySubnet, publicPoolAllocSpec(pub), "spec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before, err := e.svc.Allocations().Get(t.Context(), idOf(tt.a))
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.updateAlloc(t, tt.a, tt.spec)
			wantKind(t, tt.name, err, errs.KindBadRequest)
			if ee, ok := errors.AsType[*errs.Error](err); !ok || len(ee.Violations) != 1 || ee.Violations[0].Field != tt.field {
				t.Errorf("error = %v, want one violation on %s", err, tt.field)
			}
			after, err := e.svc.Allocations().Get(t.Context(), idOf(tt.a))
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(before, after) {
				t.Errorf("refused update changed the allocation:\n%s", cmp.Diff(before, after, protocmp.Transform()))
			}
		})
	}
}
