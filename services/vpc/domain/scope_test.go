package domain

import (
	"testing"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func TestSeededPoolsGetProjectScopes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	for _, tt := range []struct {
		pool, scope string
		typ         vpcv1alpha1pb.ScopeSpec_Type
	}{
		{DefaultPrivatePoolName, DefaultPrivateScopeName, vpcv1alpha1pb.ScopeSpec_PRIVATE},
		{DefaultPublicPoolName, DefaultPublicScopeName, vpcv1alpha1pb.ScopeSpec_PUBLIC},
	} {
		p, err := e.svc.Pools().GetByName(ctx, testProject, tt.pool)
		if err != nil {
			t.Fatalf("GetByName(%s): %v", tt.pool, err)
		}
		sc, err := e.svc.Scopes().Get(ctx, p.GetStatus().GetScopeId())
		if err != nil {
			t.Fatalf("scope of %s (%q): %v", tt.pool, p.GetStatus().GetScopeId(), err)
		}
		if sc.GetMetadata().GetName() != tt.scope || sc.GetSpec().GetType() != tt.typ ||
			sc.GetMetadata().GetParentId() != testProject || sc.GetStatus().GetState() != vpcv1alpha1pb.ScopeStatus_READY {
			t.Errorf("scope of %s = %v, want %s %s READY in %s", tt.pool, sc, tt.scope, tt.typ, testProject)
		}
	}
	page, err := e.svc.Scopes().List(ctx, testProject, 0, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("List scopes = %v, %v; want the two project scopes", page.Items, err)
	}
}

func TestPoolScopeIsSharedByTreeAndProject(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	def, err := e.svc.Pools().GetByName(ctx, testProject, DefaultPrivatePoolName)
	if err != nil {
		t.Fatal(err)
	}
	pools := e.svc.Pools()
	root := mustCreate(t, pools.Create, pools.Get, testProject, "root2", &vpcpb.PoolSpec{
		Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: "172.16.0.0/16"}},
	})
	child := mustCreate(t, pools.Create, pools.Get, testProject, "child", &vpcpb.PoolSpec{
		SourcePoolId: root.GetMetadata().GetId(), Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: "/24"}},
	})
	want := def.GetStatus().GetScopeId()
	if want == "" || root.GetStatus().GetScopeId() != want || child.GetStatus().GetScopeId() != want {
		t.Fatalf("scopes: default %q, root %q, child %q; want all equal and set",
			want, root.GetStatus().GetScopeId(), child.GetStatus().GetScopeId())
	}
	other := mustCreate(t, pools.Create, pools.Get, "project-e00other", "root-other", &vpcpb.PoolSpec{
		Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: "172.16.0.0/16"}},
	})
	if got := other.GetStatus().GetScopeId(); got == "" || got == want {
		t.Fatalf("scope in another project = %q, want a new scope (not %q)", got, want)
	}
	n, err := e.svc.Networks().Get(ctx, e.def.Network.GetMetadata().GetId())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.Networks().Scope(ctx, n); err != nil || got != want {
		t.Fatalf("Networks().Scope(default) = %q, %v; want %q", got, err, want)
	}
}

func TestCheckFilter(t *testing.T) {
	t.Parallel()
	if err := CheckFilter(""); err != nil {
		t.Fatalf("CheckFilter(\"\") = %v", err)
	}
	if err := CheckFilter("name=x"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("CheckFilter(name=x) = %v, want BadRequest", err)
	}
}

func TestScopeIsDeletedWithItsLastPool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	pools := e.svc.Pools()
	const other = "project-e00other"
	root := mustCreate(t, pools.Create, pools.Get, other, "root", &vpcpb.PoolSpec{
		Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: "172.16.0.0/16"}},
	})
	child := mustCreate(t, pools.Create, pools.Get, other, "child", &vpcpb.PoolSpec{
		SourcePoolId: root.GetMetadata().GetId(), Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: "/24"}},
	})
	scope := root.GetStatus().GetScopeId()
	if err := tryDelete(ctx, pools.Delete, child.GetMetadata().GetId()); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	if _, err := e.svc.Scopes().Get(ctx, scope); err != nil {
		t.Fatalf("scope gone while root pool %s still uses it: %v", root.GetMetadata().GetId(), err)
	}
	if err := tryDelete(ctx, pools.Delete, root.GetMetadata().GetId()); err != nil {
		t.Fatalf("delete root: %v", err)
	}
	if _, err := e.svc.Scopes().Get(ctx, scope); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("scope after its last pool was deleted: %v, want NotFound", err)
	}
	if page, err := e.svc.Scopes().List(ctx, other, 0, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("scopes left in %s: %v, %v", other, page.Items, err)
	}
}
