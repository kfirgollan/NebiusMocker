package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"
)

func TestVPCV1alpha1AllocationCreateKinds(t *testing.T) {
	t.Parallel()
	c, a := startAlpha(t)
	project := c.inst.ProjectID
	pubPool := seedPoolID(t, c, seedPublicPool)

	byPoolPool, net := netWithPool(t, c, "ak", "/16")
	sn := newSubnet(t, c, "ak-sn", net, "/24")

	// Private, by the default subnet.
	defSn := defaultSubnet(t, a, project)
	if defSn == "" {
		t.Fatal("default subnet id is empty")
	}
	idDef := newAlphaAlloc(t, c, a, "by-default-subnet", alphaPrivSpec(defSn, "", ""))
	got := getAlphaAlloc(t, a, idDef)
	if got.GetStatus().GetState() != vpcv1alpha1pb.AllocationStatus_ALLOCATED {
		t.Errorf("state = %v, want ALLOCATED", got.GetStatus().GetState())
	}
	if got.GetSpec().GetIpv4Private().GetSubnetId() != defSn {
		t.Errorf("spec = %v, want subnet_id %s", got.GetSpec(), defSn)
	}
	if got.GetStatus().GetDetails().GetAllocatedCidr() == "" || got.GetStatus().GetDetails().GetPoolId() == "" {
		t.Errorf("details = %v, want allocated_cidr and pool_id", got.GetStatus().GetDetails())
	}
	if got.GetStatus().GetDetails().GetVersion() != vpcv1alpha1pb.IpVersion_IPV4 {
		t.Errorf("version = %v, want IPV4", got.GetStatus().GetDetails().GetVersion())
	}

	// Private, by pool.
	idPool := newAlphaAlloc(t, c, a, "by-pool", alphaPrivSpec("", byPoolPool, ""))
	if p := getAlphaAlloc(t, a, idPool).GetStatus().GetDetails().GetPoolId(); p != byPoolPool {
		t.Errorf("by pool: details.pool_id = %s, want %s", p, byPoolPool)
	}

	// Private, explicit address.
	want := hostIn(t, subnetCIDRs(t, c, sn)[0], 5)
	idCIDR := newAlphaAlloc(t, c, a, "explicit", alphaPrivSpec(sn, "", want))
	if g := getAlphaAlloc(t, a, idCIDR).GetStatus().GetDetails().GetAllocatedCidr(); g != want {
		t.Errorf("explicit: allocated_cidr = %s, want %s", g, want)
	}

	// Public, empty ipv4_public and with a pool.
	idPubEmpty := newAlphaAlloc(t, c, a, "pub-empty", alphaPubSpec("", ""))
	pe := getAlphaAlloc(t, a, idPubEmpty)
	if pe.GetSpec().GetIpv4Public() == nil || pe.GetSpec().GetIpv4Public().GetPoolId() != "" {
		t.Errorf("pub-empty spec = %v, want an empty ipv4_public", pe.GetSpec())
	}
	if pe.GetStatus().GetDetails().GetPoolId() != pubPool {
		t.Errorf("pub-empty pool_id = %s, want default-public-pool %s", pe.GetStatus().GetDetails().GetPoolId(), pubPool)
	}
	idPubPool := newAlphaAlloc(t, c, a, "pub-pool", alphaPubSpec(pubPool, ""))
	pp := getAlphaAlloc(t, a, idPubPool)
	if pp.GetSpec().GetIpv4Public().GetPoolId() != pubPool || pp.GetStatus().GetDetails().GetAllocatedCidr() == "" {
		t.Errorf("pub-pool = %v", pp)
	}
}

//nolint:staticcheck // SA1019: common.v1alpha1 operation requests are deprecated upstream, still what v1alpha1 SDKs send
func TestVPCV1alpha1AllocationLifecycle(t *testing.T) {
	t.Parallel()
	c, a := startAlpha(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	_, net := netWithPool(t, c, "lc", "/16")
	sn := newSubnet(t, c, "lc-sn", net, "/24")

	op, err := a.allocations.Create(ctx, alphaAllocReq(project, "life", alphaPrivSpec(sn, "", "")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	id := op.ResourceID()

	// Operation lookups through the common.v1alpha1 OperationService.
	got, err := a.allocations.GetOperation(ctx, &commonv1alpha1pb.GetOperationRequest{Id: op.ID()})
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if !got.Done() || !got.Successful() || got.ResourceID() != id {
		t.Errorf("GetOperation: done %v successful %v resource %s, want done, successful, %s", got.Done(), got.Successful(), got.ResourceID(), id)
	}
	ops, err := a.allocations.ListOperations(ctx, &commonv1alpha1pb.ListOperationsRequest{ResourceId: id})
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	found := false
	for _, o := range ops.GetOperations() {
		found = found || o.GetId() == op.ID()
		if o.GetResourceId() != id {
			t.Errorf("listed operation %s has resource %s, want %s", o.GetId(), o.GetResourceId(), id)
		}
	}
	if !found {
		t.Errorf("ListOperations(%s) lacks %s", id, op.ID())
	}

	// Get / GetByName.
	a1 := getAlphaAlloc(t, a, id)
	byName, err := a.allocations.GetByName(ctx, &vpcv1alpha1pb.GetAllocationByNameRequest{ParentId: project, Name: "life"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v (err %v), want %s", byName.GetMetadata().GetId(), err, id)
	}

	// Update labels: the version bumps, the spec stays.
	md := proto.CloneOf(a1.GetMetadata())
	md.Labels = map[string]string{"env": "dev"}
	if err := alphaOK(ctx)(a.allocations.Update(ctx, &vpcv1alpha1pb.UpdateAllocationRequest{Metadata: md, Spec: a1.GetSpec()})); err != nil {
		t.Fatalf("Update labels: %v", err)
	}
	a2 := getAlphaAlloc(t, a, id)
	if a2.GetMetadata().GetLabels()["env"] != "dev" {
		t.Errorf("labels = %v", a2.GetMetadata().GetLabels())
	}
	if a2.GetMetadata().GetResourceVersion() != a1.GetMetadata().GetResourceVersion()+1 {
		t.Errorf("resource_version = %d, want %d", a2.GetMetadata().GetResourceVersion(), a1.GetMetadata().GetResourceVersion()+1)
	}
	if !proto.Equal(a1.GetSpec(), a2.GetSpec()) {
		t.Errorf("spec changed: %v -> %v", a1.GetSpec(), a2.GetSpec())
	}

	// cidr is immutable.
	err = alphaOK(ctx)(a.allocations.Update(ctx, &vpcv1alpha1pb.UpdateAllocationRequest{
		Metadata: a2.GetMetadata(), Spec: alphaPrivSpec(sn, "", hostIn(t, subnetCIDRs(t, c, sn)[0], 9)),
	}))
	assertCode(t, err, codes.InvalidArgument)

	// Delete.
	if err := alphaOK(ctx)(a.allocations.Delete(ctx, &vpcv1alpha1pb.DeleteAllocationRequest{Id: id})); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = a.allocations.Get(ctx, &vpcv1alpha1pb.GetAllocationRequest{Id: id})
	assertCode(t, err, codes.NotFound)
	_, err = a.allocations.GetByName(ctx, &vpcv1alpha1pb.GetAllocationByNameRequest{ParentId: project, Name: "life"})
	assertCode(t, err, codes.NotFound)
	err = alphaOK(ctx)(a.allocations.Delete(ctx, &vpcv1alpha1pb.DeleteAllocationRequest{Id: id}))
	assertCode(t, err, codes.NotFound)
}

func TestVPCV1alpha1AllocationList(t *testing.T) {
	t.Parallel()
	c, a := startAlpha(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	_, net := netWithPool(t, c, "ls", "/16")
	sn := newSubnet(t, c, "ls-sn", net, "/24")
	want := map[string]bool{}
	for i := range 5 {
		want[newAlphaAlloc(t, c, a, nameN("list", i), alphaPrivSpec(sn, "", ""))] = true
	}

	all := pageAll(t, 2, func(size int64, token string) ([]*vpcv1alpha1pb.Allocation, string, error) {
		r, err := a.allocations.List(ctx, &vpcv1alpha1pb.ListAllocationsRequest{ParentId: project, PageSize: size, PageToken: token})
		return r.GetItems(), r.GetNextPageToken(), err
	})
	seen := map[string]int{}
	for _, it := range all {
		seen[it.GetMetadata().GetId()]++
	}
	for id := range want {
		if seen[id] != 1 {
			t.Errorf("allocation %s seen %d times in the paged list", id, seen[id])
		}
	}
	v1 := pageAll(t, 0, func(size int64, token string) ([]*vpcpb.Allocation, string, error) {
		r, err := c.allocations.List(ctx, &vpcpb.ListAllocationsRequest{ParentId: project, PageSize: size, PageToken: token})
		return r.GetItems(), r.GetNextPageToken(), err
	})
	if len(v1) != len(all) {
		t.Errorf("v1alpha1 lists %d allocations, v1 lists %d", len(all), len(v1))
	}
}

func TestVPCV1alpha1AllocationErrors(t *testing.T) {
	t.Parallel()
	c, a := startAlpha(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	pool, net := netWithPool(t, c, "er", "/16")
	sn := newSubnet(t, c, "er-sn", net, "/24")

	_, err := a.allocations.Get(ctx, &vpcv1alpha1pb.GetAllocationRequest{Id: "vpcallocation-e00missing"})
	assertCode(t, err, codes.NotFound)

	newAlphaAlloc(t, c, a, "dup", alphaPrivSpec(sn, "", ""))
	_, err = a.allocations.Create(ctx, alphaAllocReq(project, "dup", alphaPrivSpec(sn, "", "")))
	assertCode(t, err, codes.AlreadyExists)

	// An address outside the pool is refused, as in v1.
	_, v1err := c.allocations.Create(ctx, createAllocReq(c, "out-v1", privAllocSpec("", pool, "192.168.0.7/32")))
	_, err = a.allocations.Create(ctx, alphaAllocReq(project, "out-alpha", alphaPrivSpec("", pool, "192.168.0.7/32")))
	if codeOf(err) != codeOf(v1err) || codeOf(err) == codes.OK {
		t.Errorf("cidr outside pool: v1alpha1 %v, v1 %v, want the same error", codeOf(err), codeOf(v1err))
	}

	// No pool at all and an empty spec are invalid.
	_, err = a.allocations.Create(ctx, alphaAllocReq(project, "nopool", alphaPrivSpec("", "", "")))
	assertCode(t, err, codes.InvalidArgument)
	_, err = a.allocations.Create(ctx, alphaAllocReq(project, "nospec", &vpcv1alpha1pb.AllocationSpec{}))
	assertCode(t, err, codes.InvalidArgument)

	// A filter is not implemented: it is refused by name, never ignored.
	_, err = a.allocations.List(ctx, &vpcv1alpha1pb.ListAllocationsRequest{ParentId: project, Filter: "name = 'x'"})
	assertCode(t, err, codes.InvalidArgument)
	if !strings.Contains(err.Error(), "filter") {
		t.Errorf("filter error %q does not name filter", err)
	}
}
