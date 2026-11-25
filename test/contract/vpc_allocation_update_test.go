package contract

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// Allocation Update follows allocation.proto: cidr is IMMUTABLE, and the pool
// oneof "may be changed only from subnet_id to pool_id, and the new pool_id
// must be equal to status.details.pool_id". The official provider plans any
// subnet_id/pool_id change in place, so the API is what refuses the rest.

func updateAlloc(t *testing.T, c vpcClients, id string, spec *vpcpb.AllocationSpec) error {
	t.Helper()
	a := getAllocation(t, c, id)
	return waitOK(t.Context())(c.allocations.Update(t.Context(), &vpcpb.UpdateAllocationRequest{Metadata: a.GetMetadata(), Spec: spec}))
}

func TestAllocationUpdateSubnetToOwnPool(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	pool, net := netWithPool(t, c, "upd", "/16")
	sn := newSubnet(t, c, "upd-sn", net, "/24")
	id := newAlloc(t, c, "upd-alloc", privAllocSpec(sn, "", ""))
	before := getAllocation(t, c, id)
	if got := before.GetStatus().GetDetails().GetPoolId(); got != pool {
		t.Fatalf("status.details.pool_id = %s, want %s", got, pool)
	}

	// As the provider sends it: the full spec plus a reset mask for the old field.
	ctx := resetCtx(t.Context(), t, "spec.ipv4_private.subnet_id")
	if err := waitOK(ctx)(c.allocations.Update(ctx, &vpcpb.UpdateAllocationRequest{
		Metadata: before.GetMetadata(), Spec: privAllocSpec("", pool, ""),
	})); err != nil {
		t.Fatalf("Update subnet_id → pool_id: %v", err)
	}
	after := getAllocation(t, c, id)
	if got := after.GetSpec().GetIpv4Private(); got.GetPoolId() != pool || got.GetSubnetId() != "" {
		t.Errorf("spec = %v, want pool_id %s only", after.GetSpec(), pool)
	}
	// Same address and pool; no longer associated with the subnet.
	want := proto.CloneOf(before.GetStatus())
	want.Details.SubnetId = ""
	if diff := cmp.Diff(want, after.GetStatus(), protocmp.Transform()); diff != "" {
		t.Errorf("status (-want +got):\n%s", diff)
	}
	if after.GetMetadata().GetResourceVersion() != before.GetMetadata().GetResourceVersion()+1 {
		t.Errorf("resource_version = %d, want %d", after.GetMetadata().GetResourceVersion(), before.GetMetadata().GetResourceVersion()+1)
	}
	byPool, err := c.allocations.ListByPool(t.Context(), &vpcpb.ListAllocationsByPoolRequest{PoolId: pool})
	if err != nil || !hasID(idsOfRes(byPool.GetItems()), id) {
		t.Errorf("ListByPool(%s) = %v (err %v), want %s", pool, idsOfRes(byPool.GetItems()), err, id)
	}

	// The address stays held: a new allocation in the subnet gets another one.
	next := newAlloc(t, c, "upd-next", privAllocSpec(sn, "", ""))
	if allocCIDR(t, c, next) == allocCIDR(t, c, id) {
		t.Errorf("address %s handed out twice", allocCIDR(t, c, id))
	}
	// It is still available to the subnet (unassigned, inside its block).
	bySubnet, err := c.allocations.ListBySubnet(t.Context(), &vpcpb.ListAllocationsBySubnetRequest{SubnetId: sn})
	if err != nil || !hasID(idsOfRes(bySubnet.GetItems()), id) || !hasID(idsOfRes(bySubnet.GetItems()), next) {
		t.Errorf("ListBySubnet(%s) = %v (err %v), want %s and %s", sn, idsOfRes(bySubnet.GetItems()), err, id, next)
	}
	// The subnet no longer pins the allocation: once its own allocation is
	// gone it can be deleted, and the moved allocation keeps its address.
	cidr := allocCIDR(t, c, id)
	if err := waitOK(t.Context())(c.allocations.Delete(t.Context(), &vpcpb.DeleteAllocationRequest{Id: next})); err != nil {
		t.Fatalf("Delete %s: %v", next, err)
	}
	if err := waitOK(t.Context())(c.subnets.Delete(t.Context(), &vpcpb.DeleteSubnetRequest{Id: sn})); err != nil {
		t.Fatalf("Delete subnet after the allocation moved to its pool: %v", err)
	}
	if got := allocCIDR(t, c, id); got != cidr {
		t.Errorf("allocated_cidr = %s after the subnet was deleted, want %s", got, cidr)
	}
	// A route in the network may still use it as its next hop.
	newRoute(t, c, newRouteTable(t, c, "upd-rt", net), "via-moved", "192.168.0.0/16", allocHop(id))
}

func TestAllocationUpdateForbiddenTransitions(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	pool, net := netWithPool(t, c, "fbd", "/16")
	other := newPrivatePool(t, c, "fbd-other", seedPoolID(t, c, seedPrivatePool), "/16")
	sn := newSubnet(t, c, "fbd-sn", net, "/24")
	sn2 := newSubnet(t, c, "fbd-sn2", net, "/24")
	bySubnet := newAlloc(t, c, "fbd-by-subnet", privAllocSpec(sn, "", ""))
	byPool := newAlloc(t, c, "fbd-by-pool", privAllocSpec("", pool, ""))
	pub := newAlloc(t, c, "fbd-pub", pubAllocSpec("", seedPoolID(t, c, seedPublicPool), ""))

	tests := []struct {
		name, id string
		spec     *vpcpb.AllocationSpec
		field    string
	}{
		{"subnet to another subnet", bySubnet, privAllocSpec(sn2, "", ""), "spec.ipv4_private.subnet_id"},
		{"subnet to a pool other than status.details.pool_id", bySubnet, privAllocSpec("", other, ""), "spec.ipv4_private.pool_id"},
		{"pool back to subnet", byPool, privAllocSpec(sn, "", ""), "spec.ipv4_private.subnet_id"},
		{"pool to another pool", byPool, privAllocSpec("", other, ""), "spec.ipv4_private.pool_id"},
		{"cidr", bySubnet, privAllocSpec(sn, "", "/31"), "spec.ipv4_private.cidr"},
		{"private to public", bySubnet, pubAllocSpec("", seedPoolID(t, c, seedPublicPool), ""), "spec"},
		{"public to private", pub, privAllocSpec("", pool, ""), "spec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := getAllocation(t, c, tt.id)
			err := updateAlloc(t, c, tt.id, tt.spec)
			if codeOf(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("Update: code %v, err %v; want InvalidArgument naming %s", codeOf(err), err, tt.field)
			}
			if after := getAllocation(t, c, tt.id); !proto.Equal(before, after) {
				t.Errorf("refused update changed the allocation:\n%s", cmp.Diff(before, after, protocmp.Transform()))
			}
		})
	}
}

// TestAllocationUpdateAuthz: only roles that may update allocations can
// change the pool reference.
func TestAllocationUpdateAuthz(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	s := r.newSet(t, "-upd")
	spec := privAllocSpec("", s.pool, "")
	for _, who := range []struct {
		name string
		c    vpcClients
		want codes.Code
	}{
		{"viewer", r.viewer, codes.PermissionDenied},
		{"outsider", r.outsider, codes.PermissionDenied},
		{"editor", r.editor, codes.OK},
	} {
		a := getAllocation(t, r.admin, s.alloc)
		err := waitOK(t.Context())(who.c.allocations.Update(t.Context(), &vpcpb.UpdateAllocationRequest{Metadata: a.GetMetadata(), Spec: spec}))
		if codeOf(err) != who.want {
			t.Errorf("%s: code %v (err %v), want %v", who.name, codeOf(err), err, who.want)
		}
	}
	if got := getAllocation(t, r.admin, s.alloc).GetSpec().GetIpv4Private().GetPoolId(); got != s.pool {
		t.Errorf("pool_id = %q after the editor's update, want %s", got, s.pool)
	}
}
