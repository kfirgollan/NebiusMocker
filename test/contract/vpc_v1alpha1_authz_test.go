package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
)

// alphaReads calls every v1alpha1 read RPC and returns the errors by name.
func alphaReads(t *testing.T, a alphaClients, s vpcSet) map[string]error {
	t.Helper()
	ctx := t.Context()
	p, n := s.project, s.suffix
	out := map[string]error{}
	rec := func(name string, _ any, err error) { out[name] = err }

	v1, err := a.pools.Get(ctx, &vpcv1alpha1pb.GetPoolRequest{Id: s.pool})
	rec("Pool.Get", v1, err)
	v2, err := a.pools.GetByName(ctx, &vpcv1alpha1pb.GetPoolByNameRequest{ParentId: p, Name: "authz-pool" + n})
	rec("Pool.GetByName", v2, err)
	v3, err := a.pools.List(ctx, &vpcv1alpha1pb.ListPoolsRequest{ParentId: p})
	rec("Pool.List", v3, err)

	v4, err := a.networks.Get(ctx, &vpcv1alpha1pb.GetNetworkRequest{Id: s.net})
	rec("Network.Get", v4, err)
	v5, err := a.networks.GetByName(ctx, &vpcv1alpha1pb.GetNetworkByNameRequest{ParentId: p, Name: "authz-net" + n})
	rec("Network.GetByName", v5, err)
	v6, err := a.networks.List(ctx, &vpcv1alpha1pb.ListNetworksRequest{ParentId: p})
	rec("Network.List", v6, err)
	v7, err := a.networks.GetDefault(ctx, &vpcv1alpha1pb.GetDefaultNetworkRequest{ParentId: p})
	rec("Network.GetDefault", v7, err)

	v8, err := a.subnets.Get(ctx, &vpcv1alpha1pb.GetSubnetRequest{Id: s.sn})
	rec("Subnet.Get", v8, err)
	v9, err := a.subnets.GetByName(ctx, &vpcv1alpha1pb.GetSubnetByNameRequest{ParentId: p, Name: "authz-sn" + n})
	rec("Subnet.GetByName", v9, err)
	v10, err := a.subnets.List(ctx, &vpcv1alpha1pb.ListSubnetsRequest{ParentId: p})
	rec("Subnet.List", v10, err)
	v11, err := a.subnets.ListByNetwork(ctx, &vpcv1alpha1pb.ListSubnetsByNetworkRequest{NetworkId: s.net})
	rec("Subnet.ListByNetwork", v11, err)

	v12, err := a.allocations.Get(ctx, &vpcv1alpha1pb.GetAllocationRequest{Id: s.alloc})
	rec("Allocation.Get", v12, err)
	v13, err := a.allocations.GetByName(ctx, &vpcv1alpha1pb.GetAllocationByNameRequest{ParentId: p, Name: "authz-alloc" + n})
	rec("Allocation.GetByName", v13, err)
	v14, err := a.allocations.List(ctx, &vpcv1alpha1pb.ListAllocationsRequest{ParentId: p})
	rec("Allocation.List", v14, err)

	v15, err := a.scopes.List(ctx, &vpcv1alpha1pb.ListScopesRequest{ParentId: p})
	rec("Scope.List", v15, err)
	if err == nil && len(v15.GetItems()) > 0 {
		v16, err := a.scopes.Get(ctx, &vpcv1alpha1pb.GetScopeRequest{Id: v15.GetItems()[0].GetMetadata().GetId()})
		rec("Scope.Get", v16, err)
	}
	v17, err := a.scopes.GetByName(ctx, &vpcv1alpha1pb.GetScopeByNameRequest{ParentId: p, Name: seedPrivateScope})
	rec("Scope.GetByName", v17, err)
	return out
}

// alphaWrites calls every v1alpha1 write RPC and returns the errors by name.
func alphaWrites(t *testing.T, a alphaClients, s vpcSet) map[string]error {
	t.Helper()
	ctx := t.Context()
	out := map[string]error{}
	// A scratch allocation to delete; the set's own one is held by a route.
	op, err := a.allocations.Create(ctx, alphaAllocReq(s.project, "w-alloc"+s.suffix, alphaPrivSpec(s.sn, "", "")))
	out["Allocation.Create"] = alphaOK(ctx)(op, err)
	md := &commonpb.ResourceMetadata{
		Id: s.alloc, ParentId: s.project, Name: "authz-alloc" + s.suffix, Labels: map[string]string{"by": "role"},
	}
	out["Allocation.Update"] = alphaOK(ctx)(a.allocations.Update(ctx, &vpcv1alpha1pb.UpdateAllocationRequest{
		Metadata: md, Spec: alphaPrivSpec(s.sn, "", ""),
	}))
	scratch := s.alloc
	if err == nil {
		scratch = op.ResourceID()
	}
	out["Allocation.Delete"] = alphaOK(ctx)(a.allocations.Delete(ctx, &vpcv1alpha1pb.DeleteAllocationRequest{Id: scratch}))
	return out
}

func TestVPCV1alpha1AuthzViewer(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	s := r.newSet(t, "-v")
	viewer := alphaClientsFor(t, r.admin.inst, config.DefaultViewerToken)
	for name, err := range alphaReads(t, viewer, s) {
		if err != nil {
			t.Errorf("viewer %s: %v", name, err)
		}
	}
	for name, err := range alphaWrites(t, viewer, s) {
		if codeOf(err) != codes.PermissionDenied {
			t.Errorf("viewer %s: code = %v, want PermissionDenied (err %v)", name, codeOf(err), err)
		}
	}
	// Nothing changed.
	if a := getAllocation(t, r.admin, s.alloc); len(a.GetMetadata().GetLabels()) != 0 {
		t.Errorf("viewer changed the allocation: %v", a.GetMetadata().GetLabels())
	}
	if _, err := r.admin.allocations.GetByName(t.Context(), &vpcpb.GetAllocationByNameRequest{ParentId: s.project, Name: "w-alloc" + s.suffix}); codeOf(err) != codes.NotFound {
		t.Errorf("an allocation created by the viewer exists: %v", err)
	}
}

func TestVPCV1alpha1AuthzEditor(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	s := r.newSet(t, "-e")
	editor := alphaClientsFor(t, r.admin.inst, config.DefaultEditorToken)
	for name, err := range alphaReads(t, editor, s) {
		if err != nil {
			t.Errorf("editor %s: %v", name, err)
		}
	}
	for name, err := range alphaWrites(t, editor, s) {
		if err != nil {
			t.Errorf("editor %s: %v", name, err)
		}
	}
	// The creator can read its own operation through common.v1alpha1.
	op, err := editor.allocations.Create(t.Context(), alphaAllocReq(s.project, "ops-alloc", alphaPrivSpec(s.sn, "", "")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	//nolint:staticcheck // SA1019: common.v1alpha1 operation requests are deprecated upstream, still what v1alpha1 SDKs send
	got, err := editor.allocations.GetOperation(t.Context(), &commonv1alpha1pb.GetOperationRequest{Id: op.ID()})
	if err != nil || !got.Done() {
		t.Errorf("creator GetOperation = %v (err %v), want a finished operation", got, err)
	}
}

func TestVPCV1alpha1AuthzOutsider(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	s := r.newSet(t, "-o")
	outsider := alphaClientsFor(t, r.admin.inst, outsiderToken)
	for name, err := range alphaReads(t, outsider, s) {
		if codeOf(err) != codes.PermissionDenied && codeOf(err) != codes.NotFound {
			t.Errorf("outsider %s: code = %v, want PermissionDenied or NotFound (err %v)", name, codeOf(err), err)
		}
	}
	for name, err := range alphaWrites(t, outsider, s) {
		if codeOf(err) != codes.PermissionDenied {
			t.Errorf("outsider %s: code = %v, want PermissionDenied (err %v)", name, codeOf(err), err)
		}
	}
	if _, err := r.admin.allocations.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: s.alloc}); err != nil {
		t.Errorf("the outsider's delete went through: %v", err)
	}
}
