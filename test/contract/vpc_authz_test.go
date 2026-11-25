package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// vpcRoles are VPC clients for each role, over one mocker enforcing authorization.
type vpcRoles struct {
	admin, editor, viewer, outsider vpcClients
	project                         string
}

// vpcSet is one of every resource kind, created by the admin.
type vpcSet struct {
	suffix                                                      string
	project, defPool, pool, net, sn, alloc, rt, route, sg, rule string
}

func startVPCRoles(t *testing.T) vpcRoles {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New()},
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{outsiderToken: outsiderID}),
	)
	return vpcRoles{
		admin:    vpcClientsFor(t, inst, config.DefaultDevToken),
		editor:   vpcClientsFor(t, inst, config.DefaultEditorToken),
		viewer:   vpcClientsFor(t, inst, config.DefaultViewerToken),
		outsider: vpcClientsFor(t, inst, outsiderToken),
		project:  inst.ProjectID,
	}
}

// newSet creates a fresh set of resources named with suffix.
func (r vpcRoles) newSet(t *testing.T, suffix string) vpcSet {
	t.Helper()
	c := r.admin
	s := vpcSet{suffix: suffix, project: r.project}
	s.defPool = seedPoolID(t, c, seedPrivatePool)
	s.pool = newPrivatePool(t, c, "authz-pool"+suffix, s.defPool, "/16")
	s.net = newNetwork(t, c, "authz-net"+suffix, s.pool)
	s.sn = newSubnet(t, c, "authz-sn"+suffix, s.net, "/24")
	s.alloc = newAlloc(t, c, "authz-alloc"+suffix, privAllocSpec(s.sn, "", ""))
	s.rt = newRouteTable(t, c, "authz-rt"+suffix, s.net)
	s.route = newRoute(t, c, s.rt, "authz-route"+suffix, "192.168.0.0/16", allocHop(s.alloc))
	s.sg = newGroup(t, c, "authz-sg"+suffix, s.net)
	s.rule = newRule(t, c, s.sg, "authz-rule"+suffix, ingressRule([]string{"10.0.0.0/8"}, 22))
	return s
}

// reads calls every read RPC as c and returns the errors by name.
func (r vpcRoles) reads(t *testing.T, c vpcClients, s vpcSet) map[string]error {
	t.Helper()
	ctx := t.Context()
	out := map[string]error{}
	rec := func(name string, _ any, err error) { out[name] = err }
	p, n := s.project, s.suffix

	v1, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: s.pool})
	rec("Pool.Get", v1, err)
	v2, err := c.pools.GetByName(ctx, &vpcpb.GetPoolByNameRequest{ParentId: p, Name: "authz-pool" + n})
	rec("Pool.GetByName", v2, err)
	v3, err := c.pools.List(ctx, &vpcpb.ListPoolsRequest{ParentId: p})
	rec("Pool.List", v3, err)
	v4, err := c.pools.ListAggregated(ctx, &vpcpb.ListAggregatedPoolsRequest{ParentId: p})
	rec("Pool.ListAggregated", v4, err)
	v5, err := c.pools.ListBySourcePool(ctx, &vpcpb.ListPoolsBySourcePoolRequest{PoolId: s.defPool})
	rec("Pool.ListBySourcePool", v5, err)

	v6, err := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: s.net})
	rec("Network.Get", v6, err)
	v7, err := c.networks.GetByName(ctx, &vpcpb.GetNetworkByNameRequest{ParentId: p, Name: "authz-net" + n})
	rec("Network.GetByName", v7, err)
	v8, err := c.networks.List(ctx, &vpcpb.ListNetworksRequest{ParentId: p})
	rec("Network.List", v8, err)
	v9, err := c.networks.ListAggregated(ctx, &vpcpb.ListAggregatedNetworksRequest{ParentId: p})
	rec("Network.ListAggregated", v9, err)
	v10, err := c.networks.GetDefault(ctx, &vpcpb.GetDefaultNetworkRequest{ParentId: p})
	rec("Network.GetDefault", v10, err)

	v11, err := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: s.sn})
	rec("Subnet.Get", v11, err)
	v12, err := c.subnets.GetByName(ctx, &vpcpb.GetSubnetByNameRequest{ParentId: p, Name: "authz-sn" + n})
	rec("Subnet.GetByName", v12, err)
	v13, err := c.subnets.List(ctx, &vpcpb.ListSubnetsRequest{ParentId: p})
	rec("Subnet.List", v13, err)
	v14, err := c.subnets.ListAggregated(ctx, &vpcpb.ListAggregatedSubnetsRequest{ParentId: p})
	rec("Subnet.ListAggregated", v14, err)
	v15, err := c.subnets.ListByNetwork(ctx, &vpcpb.ListSubnetsByNetworkRequest{NetworkId: s.net})
	rec("Subnet.ListByNetwork", v15, err)

	v16, err := c.allocations.Get(ctx, &vpcpb.GetAllocationRequest{Id: s.alloc})
	rec("Allocation.Get", v16, err)
	v17, err := c.allocations.GetByName(ctx, &vpcpb.GetAllocationByNameRequest{ParentId: p, Name: "authz-alloc" + n})
	rec("Allocation.GetByName", v17, err)
	v18, err := c.allocations.List(ctx, &vpcpb.ListAllocationsRequest{ParentId: p})
	rec("Allocation.List", v18, err)
	v19, err := c.allocations.ListAggregated(ctx, &vpcpb.ListAggregatedAllocationsRequest{ParentId: p})
	rec("Allocation.ListAggregated", v19, err)
	v20, err := c.allocations.ListByPool(ctx, &vpcpb.ListAllocationsByPoolRequest{PoolId: s.pool})
	rec("Allocation.ListByPool", v20, err)
	v21, err := c.allocations.ListBySubnet(ctx, &vpcpb.ListAllocationsBySubnetRequest{SubnetId: s.sn})
	rec("Allocation.ListBySubnet", v21, err)

	v22, err := c.routeTables.Get(ctx, &vpcpb.GetRouteTableRequest{Id: s.rt})
	rec("RouteTable.Get", v22, err)
	v23, err := c.routeTables.GetByName(ctx, &vpcpb.GetRouteTableByNameRequest{ParentId: p, Name: "authz-rt" + n})
	rec("RouteTable.GetByName", v23, err)
	v24, err := c.routeTables.List(ctx, &vpcpb.ListRouteTablesRequest{ParentId: p})
	rec("RouteTable.List", v24, err)
	v25, err := c.routeTables.ListAggregated(ctx, &vpcpb.ListAggregatedRouteTablesRequest{ParentId: p})
	rec("RouteTable.ListAggregated", v25, err)
	v26, err := c.routeTables.ListByNetwork(ctx, &vpcpb.ListRouteTablesByNetworkRequest{NetworkId: s.net})
	rec("RouteTable.ListByNetwork", v26, err)

	v27, err := c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: s.route})
	rec("Route.Get", v27, err)
	v28, err := c.routes.GetByName(ctx, &vpcpb.GetRouteByNameRequest{ParentId: s.rt, Name: "authz-route" + n})
	rec("Route.GetByName", v28, err)
	v29, err := c.routes.List(ctx, &vpcpb.ListRoutesRequest{ParentId: s.rt})
	rec("Route.List", v29, err)

	v30, err := c.groups.Get(ctx, &vpcpb.GetSecurityGroupRequest{Id: s.sg})
	rec("SecurityGroup.Get", v30, err)
	v31, err := c.groups.GetByName(ctx, &vpcpb.GetSecurityGroupByNameRequest{ParentId: p, Name: "authz-sg" + n})
	rec("SecurityGroup.GetByName", v31, err)
	v32, err := c.groups.List(ctx, &vpcpb.ListSecurityGroupsRequest{ParentId: p})
	rec("SecurityGroup.List", v32, err)
	v33, err := c.groups.ListAggregated(ctx, &vpcpb.ListAggregatedSecurityGroupsRequest{ParentId: p})
	rec("SecurityGroup.ListAggregated", v33, err)
	v34, err := c.groups.ListByNetwork(ctx, &vpcpb.ListSecurityGroupsByNetworkRequest{NetworkId: s.net})
	rec("SecurityGroup.ListByNetwork", v34, err)

	v35, err := c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: s.rule})
	rec("SecurityRule.Get", v35, err)
	v36, err := c.rules.GetByName(ctx, &vpcpb.GetSecurityRuleByNameRequest{ParentId: s.sg, Name: "authz-rule" + n})
	rec("SecurityRule.GetByName", v36, err)
	v37, err := c.rules.List(ctx, &vpcpb.ListSecurityRulesRequest{ParentId: s.sg})
	rec("SecurityRule.List", v37, err)
	return out
}

// writes calls a create, update and delete RPC of every service as c, on the
// resources of s, leaves first so every delete is valid for an authorized caller.
// It returns the error of each RPC itself (not of its operation).
func (r vpcRoles) writes(t *testing.T, c vpcClients, s vpcSet) map[string]error {
	t.Helper()
	ctx := t.Context()
	out := map[string]error{}
	who, p, adm := s.suffix, s.project, r.admin
	meta := func(id, parent, name string) *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name}
	}
	rec := func(name string, err error) { out[name] = err }

	rule := getSecurityRule(t, adm, s.rule).GetSpec()
	rec("SecurityRule.Create", errOf(c.rules.Create(ctx, ruleReq(s.sg, "new-rule"+who, ingressRule([]string{"10.1.0.0/16"}, 8080)))))
	rec("SecurityRule.Update", errOf(c.rules.Update(ctx, &vpcpb.UpdateSecurityRuleRequest{Metadata: meta(s.rule, s.sg, "renamed-rule"+who), Spec: rule})))
	rec("SecurityRule.Delete", errOf(c.rules.Delete(ctx, &vpcpb.DeleteSecurityRuleRequest{Id: s.rule})))

	rec("SecurityGroup.Create", errOf(c.groups.Create(ctx, &vpcpb.CreateSecurityGroupRequest{Metadata: md(p, "new-sg"+who), Spec: &vpcpb.SecurityGroupSpec{NetworkId: s.net}})))
	rec("SecurityGroup.Update", errOf(c.groups.Update(ctx, &vpcpb.UpdateSecurityGroupRequest{Metadata: meta(s.sg, p, "renamed-sg"+who), Spec: &vpcpb.SecurityGroupSpec{NetworkId: s.net}})))
	rec("SecurityGroup.Delete", errOf(c.groups.Delete(ctx, &vpcpb.DeleteSecurityGroupRequest{Id: s.sg})))

	route := getRoute(t, adm, s.route).GetSpec()
	rec("Route.Create", errOf(c.routes.Create(ctx, routeReq(s.rt, "new-route"+who, "172.16.0.0/12", allocHop(s.alloc)))))
	rec("Route.Update", errOf(c.routes.Update(ctx, &vpcpb.UpdateRouteRequest{Metadata: meta(s.route, s.rt, "renamed-route"+who), Spec: route})))
	rec("Route.Delete", errOf(c.routes.Delete(ctx, &vpcpb.DeleteRouteRequest{Id: s.route})))

	rec("RouteTable.Create", errOf(c.routeTables.Create(ctx, &vpcpb.CreateRouteTableRequest{Metadata: md(p, "new-rt"+who), Spec: &vpcpb.RouteTableSpec{NetworkId: s.net}})))
	rec("RouteTable.Update", errOf(c.routeTables.Update(ctx, &vpcpb.UpdateRouteTableRequest{Metadata: meta(s.rt, p, "renamed-rt"+who), Spec: &vpcpb.RouteTableSpec{NetworkId: s.net}})))
	rec("RouteTable.Delete", errOf(c.routeTables.Delete(ctx, &vpcpb.DeleteRouteTableRequest{Id: s.rt})))

	alloc := getAllocation(t, adm, s.alloc).GetSpec()
	rec("Allocation.Create", errOf(c.allocations.Create(ctx, createAllocReq(c, "new-alloc"+who, privAllocSpec(s.sn, "", "")))))
	rec("Allocation.Update", errOf(c.allocations.Update(ctx, &vpcpb.UpdateAllocationRequest{Metadata: meta(s.alloc, p, "renamed-alloc"+who), Spec: alloc})))
	rec("Allocation.Delete", errOf(c.allocations.Delete(ctx, &vpcpb.DeleteAllocationRequest{Id: s.alloc})))

	sn := getSubnet(t, adm, s.sn).GetSpec()
	rec("Subnet.Create", errOf(c.subnets.Create(ctx, createSubnetReq(c, "new-sn"+who, subnetSpec(s.net, "/28")))))
	rec("Subnet.Update", errOf(c.subnets.Update(ctx, &vpcpb.UpdateSubnetRequest{Metadata: meta(s.sn, p, "renamed-sn"+who), Spec: sn})))
	rec("Subnet.Delete", errOf(c.subnets.Delete(ctx, &vpcpb.DeleteSubnetRequest{Id: s.sn})))

	net := getNetwork(t, adm, s.net).GetSpec()
	rec("Network.Create", errOf(c.networks.Create(ctx, &vpcpb.CreateNetworkRequest{Metadata: md(p, "new-net"+who), Spec: &vpcpb.NetworkSpec{}})))
	rec("Network.CreateDefault", errOf(c.networks.CreateDefault(ctx, &vpcpb.CreateDefaultNetworkRequest{Metadata: md(p, "new-default"+who)})))
	rec("Network.Update", errOf(c.networks.Update(ctx, &vpcpb.UpdateNetworkRequest{Metadata: meta(s.net, p, "renamed-net"+who), Spec: net})))
	rec("Network.Delete", errOf(c.networks.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: s.net})))

	pool := getPool(t, adm, s.pool).GetSpec()
	rec("Pool.Create", errOf(c.pools.Create(ctx, poolReq(p, "new-pool"+who, s.defPool, vpcpb.IpVisibility_PRIVATE, "/24"))))
	rec("Pool.Update", errOf(c.pools.Update(ctx, &vpcpb.UpdatePoolRequest{Metadata: meta(s.pool, p, "renamed-pool"+who), Spec: pool})))
	rec("Pool.Delete", errOf(c.pools.Delete(ctx, &vpcpb.DeletePoolRequest{Id: s.pool})))
	return out
}

// authorizedWriteCode is the code an authorized caller gets from writes. Three
// deletes are refused because the same call sequence just created resources
// that still use their target; CreateDefault finds the existing default network.
func authorizedWriteCode(name string) codes.Code {
	switch name {
	case "Network.CreateDefault":
		return codes.AlreadyExists
	case "Subnet.Delete", "Network.Delete", "Pool.Delete":
		return codes.FailedPrecondition
	}
	return codes.OK
}

// errOf drops the operation of a started call and keeps the error.
func errOf[T any](_ T, err error) error { return err }

func TestVPCAuthzViewer(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	s := r.newSet(t, "-v")
	for name, err := range r.reads(t, r.viewer, s) {
		if err != nil {
			t.Errorf("viewer %s: %v", name, err)
		}
	}
	for name, err := range r.writes(t, r.viewer, s) {
		if codeOf(err) != codes.PermissionDenied {
			t.Errorf("viewer %s: code = %v, want PermissionDenied (err %v)", name, codeOf(err), err)
		}
	}
	// Nothing changed: no new or renamed resources, nothing deleted.
	ctx := t.Context()
	if _, err := r.admin.networks.GetByName(ctx, &vpcpb.GetNetworkByNameRequest{ParentId: r.project, Name: "new-net-v"}); codeOf(err) != codes.NotFound {
		t.Errorf("a network created by the viewer exists: %v", err)
	}
	if n := getNetwork(t, r.admin, s.net); n.GetMetadata().GetName() != "authz-net-v" {
		t.Errorf("viewer renamed the network to %q", n.GetMetadata().GetName())
	}
	if _, err := r.admin.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: s.rule}); err != nil {
		t.Errorf("viewer deleted the rule: %v", err)
	}
}

func TestVPCAuthzEditor(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	s := r.newSet(t, "-e")
	for name, err := range r.reads(t, r.editor, s) {
		if err != nil {
			t.Errorf("editor %s: %v", name, err)
		}
	}
	// The editor may write everywhere. CreateDefault is authorized but the
	// project already has a default network; everything else must succeed.
	for name, err := range r.writes(t, r.editor, s) {
		if want := authorizedWriteCode(name); codeOf(err) != want {
			t.Errorf("editor %s: code = %v, want %v (err %v)", name, codeOf(err), want, err)
		}
	}
}

func TestVPCAuthzOutsider(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	s := r.newSet(t, "-o")
	for name, err := range r.reads(t, r.outsider, s) {
		// Looking up one resource hides whether it exists; listing is refused.
		want := codes.PermissionDenied
		if strings.Contains(name, ".Get") {
			want = codes.NotFound
		}
		if codeOf(err) != want {
			t.Errorf("outsider %s: code = %v, want %v (err %v)", name, codeOf(err), want, err)
		}
	}
	for name, err := range r.writes(t, r.outsider, s) {
		if codeOf(err) != codes.PermissionDenied {
			t.Errorf("outsider %s: code = %v, want PermissionDenied (err %v)", name, codeOf(err), err)
		}
	}
	// Nothing leaked or changed.
	ctx := t.Context()
	if n := getNetwork(t, r.admin, s.net); n.GetMetadata().GetName() != "authz-net-o" {
		t.Errorf("outsider renamed the network to %q", n.GetMetadata().GetName())
	}
	if _, err := r.admin.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: s.rule}); err != nil {
		t.Errorf("the outsider's delete went through: %v", err)
	}
}

func TestVPCAuthzAdmin(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	s := r.newSet(t, "-a")
	for name, err := range r.reads(t, r.admin, s) {
		if err != nil {
			t.Errorf("admin %s: %v", name, err)
		}
	}
	for name, err := range r.writes(t, r.admin, s) {
		if want := authorizedWriteCode(name); codeOf(err) != want {
			t.Errorf("admin %s: code = %v, want %v (err %v)", name, codeOf(err), want, err)
		}
	}
}

// TestVPCAuthzMissingResources checks the codes for filters that name nothing.
func TestVPCAuthzMissingResources(t *testing.T) {
	t.Parallel()
	r := startVPCRoles(t)
	ctx := t.Context()
	_, err := r.editor.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: "vpcnetwork-e00missing"})
	assertCode(t, err, codes.NotFound)
	_, err = r.viewer.subnets.ListByNetwork(ctx, &vpcpb.ListSubnetsByNetworkRequest{NetworkId: "vpcnetwork-e00missing"})
	assertCode(t, err, codes.NotFound)
	_, err = r.viewer.subnets.ListByNetwork(ctx, &vpcpb.ListSubnetsByNetworkRequest{})
	assertCode(t, err, codes.InvalidArgument)
}
