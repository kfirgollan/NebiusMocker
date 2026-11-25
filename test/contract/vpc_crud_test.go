package contract

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// vpcDriver exposes one resource kind through a uniform set of closures so the
// same lifecycle and paging tests run against all eight services.
type vpcDriver struct {
	name string
	// parent is the metadata.parent_id of the driver's resources.
	parent string
	// create makes a resource named name in the driver's parent.
	create    func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error)
	get       func(ctx context.Context, id string) (vpcRes, error)
	getByName func(ctx context.Context, name string) (vpcRes, error)
	list      func(ctx context.Context, size int64, token string) ([]vpcRes, string, error)
	// aggregated is nil for kinds without ListAggregated (routes, rules).
	aggregated func(ctx context.Context, size int64, token string) ([]vpcRes, string, error)
	update     func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error)
	del        func(ctx context.Context, id string) (operations.Operation, error)
}

func asVpcRes[T vpcRes](v T, err error) (vpcRes, error) {
	if err != nil {
		return nil, err
	}
	return v, nil
}

func listOf[T vpcRes](items []T, next string, err error) ([]vpcRes, string, error) {
	return asRes(items), next, err
}

// vpcFixture is the set of prerequisites the drivers hang resources on.
type vpcFixture struct {
	project, netID, allocID, rtID, sgID string
}

func newFixture(t *testing.T, c vpcClients) vpcFixture {
	t.Helper()
	f := vpcFixture{project: c.inst.ProjectID}
	f.netID = newNetwork(t, c, "fx-net")
	subnetID := newSubnet(t, c, "fx-subnet", f.netID, "/24")
	f.allocID = newAlloc(t, c, "fx-alloc", privAllocSpec(subnetID, "", ""))
	f.rtID = newRouteTable(t, c, "fx-rt", f.netID)
	f.sgID = newGroup(t, c, "fx-sg", f.netID)
	return f
}

type vpcDriverFactory struct {
	name string
	make func(c vpcClients, f vpcFixture) vpcDriver
}

func vpcDrivers() []vpcDriverFactory {
	return []vpcDriverFactory{
		{"pool", poolDriver},
		{"network", networkDriver},
		{"subnet", subnetDriver},
		{"allocation", allocationDriver},
		{"route_table", routeTableDriver},
		{"route", routeDriver},
		{"security_group", securityGroupDriver},
		{"security_rule", ruleDriver},
	}
}

func poolDriver(c vpcClients, f vpcFixture) vpcDriver {
	var src atomic.Value
	d := vpcDriver{name: "pool", parent: f.project}
	d.create = func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
		id, _ := src.Load().(string)
		if id == "" {
			p, err := c.pools.GetByName(ctx, &vpcpb.GetPoolByNameRequest{ParentId: f.project, Name: seedPrivatePool})
			if err != nil {
				return nil, err
			}
			id = p.GetMetadata().GetId()
			src.Store(id)
		}
		req := poolReq(f.project, name, id, vpcpb.IpVisibility_PRIVATE, "/28")
		req.Metadata.Labels = labels
		return c.pools.Create(ctx, req)
	}
	d.get = func(ctx context.Context, id string) (vpcRes, error) {
		return asVpcRes(c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: id}))
	}
	d.getByName = func(ctx context.Context, name string) (vpcRes, error) {
		return asVpcRes(c.pools.GetByName(ctx, &vpcpb.GetPoolByNameRequest{ParentId: f.project, Name: name}))
	}
	d.list = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.pools.List(ctx, &vpcpb.ListPoolsRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.aggregated = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.pools.ListAggregated(ctx, &vpcpb.ListAggregatedPoolsRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.update = func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
		cur, _ := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: md.GetId()})
		return c.pools.Update(ctx, &vpcpb.UpdatePoolRequest{Metadata: md, Spec: cur.GetSpec()})
	}
	d.del = func(ctx context.Context, id string) (operations.Operation, error) {
		return c.pools.Delete(ctx, &vpcpb.DeletePoolRequest{Id: id})
	}
	return d
}

func networkDriver(c vpcClients, f vpcFixture) vpcDriver {
	d := vpcDriver{name: "network", parent: f.project}
	d.create = func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
		m := md(f.project, name)
		m.Labels = labels
		return c.networks.Create(ctx, &vpcpb.CreateNetworkRequest{Metadata: m, Spec: &vpcpb.NetworkSpec{}})
	}
	d.get = func(ctx context.Context, id string) (vpcRes, error) {
		return asVpcRes(c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: id}))
	}
	d.getByName = func(ctx context.Context, name string) (vpcRes, error) {
		return asVpcRes(c.networks.GetByName(ctx, &vpcpb.GetNetworkByNameRequest{ParentId: f.project, Name: name}))
	}
	d.list = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.networks.List(ctx, &vpcpb.ListNetworksRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.aggregated = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.networks.ListAggregated(ctx, &vpcpb.ListAggregatedNetworksRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.update = func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
		cur, _ := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: md.GetId()})
		return c.networks.Update(ctx, &vpcpb.UpdateNetworkRequest{Metadata: md, Spec: cur.GetSpec()})
	}
	d.del = func(ctx context.Context, id string) (operations.Operation, error) {
		return c.networks.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: id})
	}
	return d
}

func subnetDriver(c vpcClients, f vpcFixture) vpcDriver {
	d := vpcDriver{name: "subnet", parent: f.project}
	d.create = func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
		req := createSubnetReq(c, name, subnetSpec(f.netID, "/28"))
		req.Metadata.Labels = labels
		return c.subnets.Create(ctx, req)
	}
	d.get = func(ctx context.Context, id string) (vpcRes, error) {
		return asVpcRes(c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: id}))
	}
	d.getByName = func(ctx context.Context, name string) (vpcRes, error) {
		return asVpcRes(c.subnets.GetByName(ctx, &vpcpb.GetSubnetByNameRequest{ParentId: f.project, Name: name}))
	}
	d.list = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.subnets.List(ctx, &vpcpb.ListSubnetsRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.aggregated = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.subnets.ListAggregated(ctx, &vpcpb.ListAggregatedSubnetsRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.update = func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
		cur, _ := c.subnets.Get(ctx, &vpcpb.GetSubnetRequest{Id: md.GetId()})
		return c.subnets.Update(ctx, &vpcpb.UpdateSubnetRequest{Metadata: md, Spec: cur.GetSpec()})
	}
	d.del = func(ctx context.Context, id string) (operations.Operation, error) {
		return c.subnets.Delete(ctx, &vpcpb.DeleteSubnetRequest{Id: id})
	}
	return d
}

func allocationDriver(c vpcClients, f vpcFixture) vpcDriver {
	d := vpcDriver{name: "allocation", parent: f.project}
	d.create = func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
		pool, err := c.pools.GetByName(ctx, &vpcpb.GetPoolByNameRequest{ParentId: f.project, Name: seedPublicPool})
		if err != nil {
			return nil, err
		}
		req := createAllocReq(c, name, pubAllocSpec("", pool.GetMetadata().GetId(), ""))
		req.Metadata.Labels = labels
		return c.allocations.Create(ctx, req)
	}
	d.get = func(ctx context.Context, id string) (vpcRes, error) {
		return asVpcRes(c.allocations.Get(ctx, &vpcpb.GetAllocationRequest{Id: id}))
	}
	d.getByName = func(ctx context.Context, name string) (vpcRes, error) {
		return asVpcRes(c.allocations.GetByName(ctx, &vpcpb.GetAllocationByNameRequest{ParentId: f.project, Name: name}))
	}
	d.list = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.allocations.List(ctx, &vpcpb.ListAllocationsRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.aggregated = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.allocations.ListAggregated(ctx, &vpcpb.ListAggregatedAllocationsRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.update = func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
		cur, _ := c.allocations.Get(ctx, &vpcpb.GetAllocationRequest{Id: md.GetId()})
		return c.allocations.Update(ctx, &vpcpb.UpdateAllocationRequest{Metadata: md, Spec: cur.GetSpec()})
	}
	d.del = func(ctx context.Context, id string) (operations.Operation, error) {
		return c.allocations.Delete(ctx, &vpcpb.DeleteAllocationRequest{Id: id})
	}
	return d
}

func routeTableDriver(c vpcClients, f vpcFixture) vpcDriver {
	d := vpcDriver{name: "route_table", parent: f.project}
	d.create = func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
		m := md(f.project, name)
		m.Labels = labels
		return c.routeTables.Create(ctx, &vpcpb.CreateRouteTableRequest{Metadata: m, Spec: &vpcpb.RouteTableSpec{NetworkId: f.netID}})
	}
	d.get = func(ctx context.Context, id string) (vpcRes, error) {
		return asVpcRes(c.routeTables.Get(ctx, &vpcpb.GetRouteTableRequest{Id: id}))
	}
	d.getByName = func(ctx context.Context, name string) (vpcRes, error) {
		return asVpcRes(c.routeTables.GetByName(ctx, &vpcpb.GetRouteTableByNameRequest{ParentId: f.project, Name: name}))
	}
	d.list = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.routeTables.List(ctx, &vpcpb.ListRouteTablesRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.aggregated = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.routeTables.ListAggregated(ctx, &vpcpb.ListAggregatedRouteTablesRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.update = func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
		cur, _ := c.routeTables.Get(ctx, &vpcpb.GetRouteTableRequest{Id: md.GetId()})
		return c.routeTables.Update(ctx, &vpcpb.UpdateRouteTableRequest{Metadata: md, Spec: cur.GetSpec()})
	}
	d.del = func(ctx context.Context, id string) (operations.Operation, error) {
		return c.routeTables.Delete(ctx, &vpcpb.DeleteRouteTableRequest{Id: id})
	}
	return d
}

func routeDriver(c vpcClients, f vpcFixture) vpcDriver {
	var n atomic.Int32
	d := vpcDriver{name: "route", parent: f.rtID}
	d.create = func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
		req := routeReq(f.rtID, name, fmt.Sprintf("192.168.%d.0/24", n.Add(1)), allocHop(f.allocID))
		req.Metadata.Labels = labels
		return c.routes.Create(ctx, req)
	}
	d.get = func(ctx context.Context, id string) (vpcRes, error) {
		return asVpcRes(c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: id}))
	}
	d.getByName = func(ctx context.Context, name string) (vpcRes, error) {
		return asVpcRes(c.routes.GetByName(ctx, &vpcpb.GetRouteByNameRequest{ParentId: f.rtID, Name: name}))
	}
	d.list = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.routes.List(ctx, &vpcpb.ListRoutesRequest{ParentId: f.rtID, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.update = func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
		cur, _ := c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: md.GetId()})
		return c.routes.Update(ctx, &vpcpb.UpdateRouteRequest{Metadata: md, Spec: cur.GetSpec()})
	}
	d.del = func(ctx context.Context, id string) (operations.Operation, error) {
		return c.routes.Delete(ctx, &vpcpb.DeleteRouteRequest{Id: id})
	}
	return d
}

func securityGroupDriver(c vpcClients, f vpcFixture) vpcDriver {
	d := vpcDriver{name: "security_group", parent: f.project}
	d.create = func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
		m := md(f.project, name)
		m.Labels = labels
		return c.groups.Create(ctx, &vpcpb.CreateSecurityGroupRequest{Metadata: m, Spec: &vpcpb.SecurityGroupSpec{NetworkId: f.netID}})
	}
	d.get = func(ctx context.Context, id string) (vpcRes, error) {
		return asVpcRes(c.groups.Get(ctx, &vpcpb.GetSecurityGroupRequest{Id: id}))
	}
	d.getByName = func(ctx context.Context, name string) (vpcRes, error) {
		return asVpcRes(c.groups.GetByName(ctx, &vpcpb.GetSecurityGroupByNameRequest{ParentId: f.project, Name: name}))
	}
	d.list = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.groups.List(ctx, &vpcpb.ListSecurityGroupsRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.aggregated = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.groups.ListAggregated(ctx, &vpcpb.ListAggregatedSecurityGroupsRequest{ParentId: f.project, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.update = func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
		cur, _ := c.groups.Get(ctx, &vpcpb.GetSecurityGroupRequest{Id: md.GetId()})
		return c.groups.Update(ctx, &vpcpb.UpdateSecurityGroupRequest{Metadata: md, Spec: cur.GetSpec()})
	}
	d.del = func(ctx context.Context, id string) (operations.Operation, error) {
		return c.groups.Delete(ctx, &vpcpb.DeleteSecurityGroupRequest{Id: id})
	}
	return d
}

func ruleDriver(c vpcClients, f vpcFixture) vpcDriver {
	var n atomic.Int32
	d := vpcDriver{name: "security_rule", parent: f.sgID}
	d.create = func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
		req := ruleReq(f.sgID, name, ingressRule([]string{"10.0.0.0/8"}, 1000+n.Add(1)))
		req.Metadata.Labels = labels
		return c.rules.Create(ctx, req)
	}
	d.get = func(ctx context.Context, id string) (vpcRes, error) {
		return asVpcRes(c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: id}))
	}
	d.getByName = func(ctx context.Context, name string) (vpcRes, error) {
		return asVpcRes(c.rules.GetByName(ctx, &vpcpb.GetSecurityRuleByNameRequest{ParentId: f.sgID, Name: name}))
	}
	d.list = func(ctx context.Context, size int64, token string) ([]vpcRes, string, error) {
		r, err := c.rules.List(ctx, &vpcpb.ListSecurityRulesRequest{ParentId: f.sgID, PageSize: size, PageToken: token})
		return listOf(r.GetItems(), r.GetNextPageToken(), err)
	}
	d.update = func(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
		cur, _ := c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: md.GetId()})
		return c.rules.Update(ctx, &vpcpb.UpdateSecurityRuleRequest{Metadata: md, Spec: cur.GetSpec()})
	}
	d.del = func(ctx context.Context, id string) (operations.Operation, error) {
		return c.rules.Delete(ctx, &vpcpb.DeleteSecurityRuleRequest{Id: id})
	}
	return d
}

// upd updates a resource of the driver's parent.
func (d vpcDriver) upd(ctx context.Context, md *commonpb.ResourceMetadata) (operations.Operation, error) {
	md.ParentId = d.parent
	return d.update(ctx, md)
}

func (d vpcDriver) mustCreate(t *testing.T, name string, labels map[string]string) string {
	t.Helper()
	return wait(t)(d.create(t.Context(), name, labels))
}

func (d vpcDriver) mustGet(t *testing.T, id string) vpcRes {
	t.Helper()
	r, err := d.get(t.Context(), id)
	if err != nil {
		t.Fatalf("%s Get(%s): %v", d.name, id, err)
	}
	return r
}

// forEachDriver runs fn once per resource kind, each with its own mocker.
func forEachDriver(t *testing.T, fn func(t *testing.T, c vpcClients, d vpcDriver), opts ...testkit.Option) {
	t.Helper()
	for _, fac := range vpcDrivers() {
		t.Run(fac.name, func(t *testing.T) {
			t.Parallel()
			c := startVPC(t, opts...)
			fn(t, c, fac.make(c, newFixture(t, c)))
		})
	}
}

func TestVPCLifecycle(t *testing.T) {
	t.Parallel()
	forEachDriver(t, runLifecycle)
}

// TestVPCLifecycleAsync runs the same lifecycle with operations that take time.
func TestVPCLifecycleAsync(t *testing.T) {
	t.Parallel()
	forEachDriver(t, runLifecycle, testkit.WithOperationDelay(50*time.Millisecond))
}

func runLifecycle(t *testing.T, _ vpcClients, d vpcDriver) {
	t.Helper()
	ctx := t.Context()
	id := d.mustCreate(t, "life", map[string]string{"team": "core", "tier": "gold"})

	got := d.mustGet(t, id)
	m := got.GetMetadata()
	if m.GetId() != id || m.GetName() != "life" {
		t.Errorf("metadata id/name = %q/%q, want %q/life", m.GetId(), m.GetName(), id)
	}
	if m.GetResourceVersion() != 1 {
		t.Errorf("resource_version = %d, want 1", m.GetResourceVersion())
	}
	if m.GetCreatedAt() == nil || m.GetUpdatedAt() == nil {
		t.Errorf("timestamps not set: %v", m)
	}
	if diff := cmp.Diff(map[string]string{"team": "core", "tier": "gold"}, m.GetLabels()); diff != "" {
		t.Errorf("labels (-want +got):\n%s", diff)
	}

	byName, err := d.getByName(ctx, "life")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if diff := cmp.Diff(got, byName, protocmp.Transform()); diff != "" {
		t.Errorf("GetByName differs from Get (-get +byName):\n%s", diff)
	}
	if _, err := d.getByName(ctx, "no-such-name"); codeOf(err) != codes.NotFound {
		t.Errorf("GetByName(missing) code = %v, want NotFound (err %v)", codeOf(err), err)
	}

	items, _, err := d.list(ctx, 0, "")
	if err != nil || !hasID(idsOfRes(items), id) {
		t.Errorf("List does not contain %s (err %v)", id, err)
	}
	if d.aggregated != nil {
		agg, _, err := d.aggregated(ctx, 0, "")
		if err != nil {
			t.Fatalf("ListAggregated: %v", err)
		}
		if diff := cmp.Diff(idsOfRes(items), idsOfRes(agg)); diff != "" {
			t.Errorf("ListAggregated differs from List (-list +aggregated):\n%s", diff)
		}
	}

	// The SDK derives a reset mask from the request, so updates carry the labels to keep.
	if err := waitOK(ctx)(d.upd(ctx, &commonpb.ResourceMetadata{Id: id, Name: "life-2", Labels: m.GetLabels()})); err != nil {
		t.Fatalf("rename: %v", err)
	}
	after := d.mustGet(t, id).GetMetadata()
	if after.GetName() != "life-2" || after.GetResourceVersion() != 2 {
		t.Errorf("after rename name/version = %q/%d, want life-2/2", after.GetName(), after.GetResourceVersion())
	}
	if len(after.GetLabels()) != 2 {
		t.Errorf("labels after rename = %v, want both kept", after.GetLabels())
	}
	if _, err := d.getByName(ctx, "life"); codeOf(err) != codes.NotFound {
		t.Errorf("GetByName(old name) code = %v, want NotFound", codeOf(err))
	}
	if _, err := d.getByName(ctx, "life-2"); err != nil {
		t.Errorf("GetByName(new name): %v", err)
	}

	// Relabel: labels in the request replace the stored values.
	if err := waitOK(ctx)(d.upd(ctx, &commonpb.ResourceMetadata{Id: id, Name: "life-2", Labels: map[string]string{"team": "infra"}})); err != nil {
		t.Fatalf("relabel: %v", err)
	}
	after = d.mustGet(t, id).GetMetadata()
	if after.GetLabels()["team"] != "infra" || after.GetResourceVersion() != 3 {
		t.Errorf("after relabel labels/version = %v/%d, want team=infra/3", after.GetLabels(), after.GetResourceVersion())
	}

	// A no-op update keeps the version.
	if err := waitOK(ctx)(d.upd(ctx, &commonpb.ResourceMetadata{Id: id, Name: "life-2", Labels: after.GetLabels()})); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	same := d.mustGet(t, id).GetMetadata()
	if same.GetResourceVersion() != 3 {
		t.Errorf("no-op update bumped resource_version to %d, want 3", same.GetResourceVersion())
	}
	if diff := cmp.Diff(after.GetUpdatedAt(), same.GetUpdatedAt(), protocmp.Transform()); diff != "" {
		t.Errorf("no-op update changed updated_at:\n%s", diff)
	}

	// The reset mask clears labels.
	if err := waitOK(ctx)(d.upd(resetCtx(ctx, t, "metadata.labels"), &commonpb.ResourceMetadata{Id: id, Name: "life-2"})); err != nil {
		t.Fatalf("reset labels: %v", err)
	}
	cleared := d.mustGet(t, id).GetMetadata()
	if len(cleared.GetLabels()) != 0 || cleared.GetResourceVersion() != 4 {
		t.Errorf("after reset labels/version = %v/%d, want none/4", cleared.GetLabels(), cleared.GetResourceVersion())
	}

	// Optimistic concurrency: a stale resource_version is rejected.
	_, err = d.upd(ctx, &commonpb.ResourceMetadata{Id: id, Name: "stale", ResourceVersion: 1})
	assertCode(t, err, codes.Aborted)
	_, err = d.upd(ctx, &commonpb.ResourceMetadata{Id: id, Name: "stale", ResourceVersion: 99})
	assertCode(t, err, codes.Aborted)
	// The current version is accepted.
	if err := waitOK(ctx)(d.upd(ctx, &commonpb.ResourceMetadata{Id: id, Name: "life-3", ResourceVersion: 4})); err != nil {
		t.Fatalf("update with current version: %v", err)
	}
	if got := d.mustGet(t, id).GetMetadata(); got.GetName() != "life-3" || got.GetResourceVersion() != 5 {
		t.Errorf("name/version = %q/%d, want life-3/5", got.GetName(), got.GetResourceVersion())
	}

	// Updating a missing resource fails.
	_, err = d.upd(ctx, &commonpb.ResourceMetadata{Id: id + "-gone", Name: "xx"})
	assertCode(t, err, codes.NotFound)

	// Duplicate names in one parent are rejected.
	_, err = d.create(ctx, "life-3", nil)
	assertCode(t, err, codes.AlreadyExists)

	if err := waitOK(ctx)(d.del(ctx, id)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := d.get(ctx, id); codeOf(err) != codes.NotFound {
		t.Errorf("Get after delete code = %v, want NotFound (err %v)", codeOf(err), err)
	}
	if _, err := d.getByName(ctx, "life-3"); codeOf(err) != codes.NotFound {
		t.Errorf("GetByName after delete code = %v, want NotFound", codeOf(err))
	}
	_, err = d.del(ctx, id)
	assertCode(t, err, codes.NotFound)
	// The name is free again.
	d.mustCreate(t, "life-3", nil)
}

func TestVPCPaging(t *testing.T) {
	t.Parallel()
	forEachDriver(t, func(t *testing.T, _ vpcClients, d vpcDriver) {
		t.Helper()
		ctx := t.Context()
		for i := range 7 {
			d.mustCreate(t, nameN("page", i), nil)
		}
		full, token, err := d.list(ctx, 0, "")
		if err != nil || token != "" {
			t.Fatalf("List(all) token %q err %v", token, err)
		}
		if len(full) < 7 {
			t.Fatalf("List(all) returned %d items, want at least 7", len(full))
		}
		want := idsOfRes(full)

		for _, size := range []int64{1, 2, 3, int64(len(full))} {
			got := pageAll(t, size, func(sz int64, tok string) ([]vpcRes, string, error) { return d.list(ctx, sz, tok) })
			if diff := cmp.Diff(want, idsOfRes(got)); diff != "" {
				t.Errorf("page_size=%d: ids differ from the full list (-full +paged):\n%s", size, diff)
			}
		}
		if d.aggregated != nil {
			got := pageAll(t, 3, func(sz int64, tok string) ([]vpcRes, string, error) { return d.aggregated(ctx, sz, tok) })
			if diff := cmp.Diff(want, idsOfRes(got)); diff != "" {
				t.Errorf("ListAggregated paged (-full +paged):\n%s", diff)
			}
		}

		// A page that holds exactly the remaining items has no next token.
		last, next, err := d.list(ctx, int64(len(full)), "")
		if err != nil || next != "" || len(last) != len(full) {
			t.Errorf("page_size == total: %d items, token %q, err %v", len(last), next, err)
		}

		_, _, err = d.list(ctx, -1, "")
		assertCode(t, err, codes.InvalidArgument)
		_, _, err = d.list(ctx, 100000, "")
		assertCode(t, err, codes.InvalidArgument)
		_, _, err = d.list(ctx, 2, "%%not-a-token%%")
		assertCode(t, err, codes.InvalidArgument)
		if d.aggregated != nil {
			_, _, err = d.aggregated(ctx, -1, "")
			assertCode(t, err, codes.InvalidArgument)
			_, _, err = d.aggregated(ctx, 2, "%%not-a-token%%")
			assertCode(t, err, codes.InvalidArgument)
		}
	})
}

func TestVPCCreateValidation(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	f := newFixture(t, c)
	publicPool := seedPoolID(t, c, seedPublicPool)

	tests := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"pool without parent", func() error {
			_, err := c.pools.Create(ctx, poolReq("", "xx", "", vpcpb.IpVisibility_PRIVATE, "10.9.0.0/16"))
			return err
		}, codes.InvalidArgument},
		{"pool in a non-project parent", func() error {
			_, err := c.pools.Create(ctx, poolReq("tenant-e00bogus", "xx", "", vpcpb.IpVisibility_PRIVATE, "10.9.0.0/16"))
			return err
		}, codes.InvalidArgument},
		{"pool without visibility", func() error {
			_, err := c.pools.Create(ctx, poolReq(f.project, "xx", "", vpcpb.IpVisibility_IP_VISIBILITY_UNSPECIFIED, "10.9.0.0/16"))
			return err
		}, codes.InvalidArgument},
		{"root pool with a bare prefix length", func() error {
			_, err := c.pools.Create(ctx, poolReq(f.project, "xx", "", vpcpb.IpVisibility_PRIVATE, "/16"))
			return err
		}, codes.InvalidArgument},
		{"pool with missing source", func() error {
			_, err := c.pools.Create(ctx, poolReq(f.project, "xx", "pool-e00doesnotexist", vpcpb.IpVisibility_PRIVATE, "/16"))
			return err
		}, codes.NotFound},
		{"pool with metadata id set", func() error {
			req := poolReq(f.project, "xx", "", vpcpb.IpVisibility_PRIVATE, "10.9.0.0/16")
			req.Metadata.Id = "pool-e00mine"
			_, err := c.pools.Create(ctx, req)
			return err
		}, codes.InvalidArgument},
		{"subnet in missing network", func() error {
			_, err := c.subnets.Create(ctx, createSubnetReq(c, "xx", subnetSpec("vpcnetwork-e00missing", "/24")))
			return err
		}, codes.NotFound},
		{"subnet without network", func() error {
			_, err := c.subnets.Create(ctx, createSubnetReq(c, "xx", subnetSpec("", "/24")))
			return err
		}, codes.InvalidArgument},
		{"route table in missing network", func() error {
			_, err := c.routeTables.Create(ctx, &vpcpb.CreateRouteTableRequest{Metadata: md(f.project, "xx"), Spec: &vpcpb.RouteTableSpec{NetworkId: "vpcnetwork-e00missing"}})
			return err
		}, codes.NotFound},
		{"security group in missing network", func() error {
			_, err := c.groups.Create(ctx, &vpcpb.CreateSecurityGroupRequest{Metadata: md(f.project, "xx"), Spec: &vpcpb.SecurityGroupSpec{NetworkId: "vpcnetwork-e00missing"}})
			return err
		}, codes.NotFound},
		{"route in missing route table", func() error {
			_, err := c.routes.Create(ctx, routeReq("vpcroutetable-e00missing", "xx", "10.1.0.0/16", allocHop(f.allocID)))
			return err
		}, codes.NotFound},
		{"route without next hop", func() error {
			_, err := c.routes.Create(ctx, routeReq(f.rtID, "xx", "10.1.0.0/16", nil))
			return err
		}, codes.InvalidArgument},
		{"route with bad destination", func() error {
			_, err := c.routes.Create(ctx, routeReq(f.rtID, "xx", "10.1.0.5/16", allocHop(f.allocID)))
			return err
		}, codes.InvalidArgument},
		{"route to missing allocation", func() error {
			_, err := c.routes.Create(ctx, routeReq(f.rtID, "xx", "10.1.0.0/16", allocHop("vpcallocation-e00missing")))
			return err
		}, codes.NotFound},
		{"rule in missing group", func() error {
			_, err := c.rules.Create(ctx, ruleReq("vpcsecuritygroup-e00missing", "xx", ingressRule([]string{"10.0.0.0/8"})))
			return err
		}, codes.NotFound},
		{"allocation without spec", func() error {
			_, err := c.allocations.Create(ctx, createAllocReq(c, "xx", &vpcpb.AllocationSpec{}))
			return err
		}, codes.InvalidArgument},
		{"private allocation without subnet or pool", func() error {
			_, err := c.allocations.Create(ctx, createAllocReq(c, "xx", privAllocSpec("", "", "")))
			return err
		}, codes.InvalidArgument},
		{"allocation in missing subnet", func() error {
			_, err := c.allocations.Create(ctx, createAllocReq(c, "xx", privAllocSpec("vpcsubnet-e00missing", "", "")))
			return err
		}, codes.NotFound},
		{"private allocation from a public pool", func() error {
			_, err := c.allocations.Create(ctx, createAllocReq(c, "xx", privAllocSpec("", publicPool, "")))
			return err
		}, codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertCode(t, tt.call(), tt.want)
		})
	}
}

func TestVPCListByVariants(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	defPool := seedPoolID(t, c, seedPrivatePool)

	// Two networks so the filters have something to exclude.
	poolA := newPrivatePool(t, c, "pool-a", defPool, "/16")
	poolB := newPrivatePool(t, c, "pool-b", defPool, "/16")
	child := newPrivatePool(t, c, "pool-a-child", poolA, "/20")
	netA := newNetwork(t, c, "net-a", poolA)
	netB := newNetwork(t, c, "net-b", poolB)
	subA1 := newSubnet(t, c, "sub-a1", netA, "/24")
	subA2 := newSubnet(t, c, "sub-a2", netA, "/24")
	subB := newSubnet(t, c, "sub-b", netB, "/24")
	allocA1 := newAlloc(t, c, "alloc-a1", privAllocSpec(subA1, "", ""))
	allocA2 := newAlloc(t, c, "alloc-a2", privAllocSpec(subA1, "", ""))
	allocB := newAlloc(t, c, "alloc-b", privAllocSpec(subB, "", ""))
	rtA := newRouteTable(t, c, "rt-a", netA)
	rtB := newRouteTable(t, c, "rt-b", netB)
	sgA := newGroup(t, c, "sg-a", netA)
	sgB := newGroup(t, c, "sg-b", netB)

	sorted := func(s []string) []string { return slices.Sorted(slices.Values(s)) }
	check := func(t *testing.T, what string, got, want []string) {
		t.Helper()
		if diff := cmp.Diff(sorted(want), sorted(got), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("%s (-want +got):\n%s", what, diff)
		}
	}

	t.Run("pools by source pool", func(t *testing.T) {
		t.Parallel()
		got := pageAll(t, 1, func(size int64, token string) ([]*vpcpb.Pool, string, error) {
			r, err := c.pools.ListBySourcePool(ctx, &vpcpb.ListPoolsBySourcePoolRequest{PoolId: defPool, PageSize: size, PageToken: token})
			return r.GetItems(), r.GetNextPageToken(), err
		})
		check(t, "children of the default pool", idsOfRes(got), []string{poolA, poolB})
		r, err := c.pools.ListBySourcePool(ctx, &vpcpb.ListPoolsBySourcePoolRequest{PoolId: poolA})
		if err != nil {
			t.Fatal(err)
		}
		check(t, "children of pool-a", idsOfRes(r.GetItems()), []string{child})
		r, err = c.pools.ListBySourcePool(ctx, &vpcpb.ListPoolsBySourcePoolRequest{PoolId: child})
		if err != nil || len(r.GetItems()) != 0 {
			t.Errorf("leaf pool children = %v, err %v, want none", r.GetItems(), err)
		}
	})
	t.Run("subnets by network", func(t *testing.T) {
		t.Parallel()
		got := pageAll(t, 1, func(size int64, token string) ([]*vpcpb.Subnet, string, error) {
			r, err := c.subnets.ListByNetwork(ctx, &vpcpb.ListSubnetsByNetworkRequest{NetworkId: netA, PageSize: size, PageToken: token})
			return r.GetItems(), r.GetNextPageToken(), err
		})
		check(t, "subnets of net-a", idsOfRes(got), []string{subA1, subA2})
		r, err := c.subnets.ListByNetwork(ctx, &vpcpb.ListSubnetsByNetworkRequest{NetworkId: netB})
		if err != nil {
			t.Fatal(err)
		}
		check(t, "subnets of net-b", idsOfRes(r.GetItems()), []string{subB})
	})
	t.Run("allocations by subnet and pool", func(t *testing.T) {
		t.Parallel()
		r, err := c.allocations.ListBySubnet(ctx, &vpcpb.ListAllocationsBySubnetRequest{SubnetId: subA1})
		if err != nil {
			t.Fatal(err)
		}
		check(t, "allocations of sub-a1", idsOfRes(r.GetItems()), []string{allocA1, allocA2})
		r, err = c.allocations.ListBySubnet(ctx, &vpcpb.ListAllocationsBySubnetRequest{SubnetId: subA2})
		if err != nil || len(r.GetItems()) != 0 {
			t.Errorf("allocations of empty sub-a2 = %v, err %v", r.GetItems(), err)
		}
		got := pageAll(t, 1, func(size int64, token string) ([]*vpcpb.Allocation, string, error) {
			r, err := c.allocations.ListByPool(ctx, &vpcpb.ListAllocationsByPoolRequest{PoolId: poolA, PageSize: size, PageToken: token})
			return r.GetItems(), r.GetNextPageToken(), err
		})
		check(t, "allocations from pool-a", idsOfRes(got), []string{allocA1, allocA2})
		r, err = c.allocations.ListByPool(ctx, &vpcpb.ListAllocationsByPoolRequest{PoolId: poolB})
		if err != nil {
			t.Fatal(err)
		}
		check(t, "allocations from pool-b", idsOfRes(r.GetItems()), []string{allocB})
	})
	t.Run("route tables by network", func(t *testing.T) {
		t.Parallel()
		r, err := c.routeTables.ListByNetwork(ctx, &vpcpb.ListRouteTablesByNetworkRequest{NetworkId: netA})
		if err != nil {
			t.Fatal(err)
		}
		na, err := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: netA})
		if err != nil {
			t.Fatal(err)
		}
		// The default route table plus the one created above.
		check(t, "route tables of net-a", idsOfRes(r.GetItems()), []string{rtA, na.GetStatus().GetDefaultRouteTableId()})
		r, err = c.routeTables.ListByNetwork(ctx, &vpcpb.ListRouteTablesByNetworkRequest{NetworkId: netB, PageSize: 1})
		if err != nil || len(r.GetItems()) != 1 || r.GetNextPageToken() == "" {
			t.Errorf("net-b first page = %d items, token %q, err %v; want 1 item and a token", len(r.GetItems()), r.GetNextPageToken(), err)
		}
		_ = rtB
	})
	t.Run("security groups by network", func(t *testing.T) {
		t.Parallel()
		r, err := c.groups.ListByNetwork(ctx, &vpcpb.ListSecurityGroupsByNetworkRequest{NetworkId: netA})
		if err != nil {
			t.Fatal(err)
		}
		got := idsOfRes(r.GetItems())
		if len(got) != 2 || !hasID(got, sgA) || hasID(got, sgB) {
			t.Errorf("groups of net-a = %v, want default group and %s", got, sgA)
		}
		for _, g := range r.GetItems() {
			if g.GetSpec().GetNetworkId() != netA {
				t.Errorf("group %s belongs to %s", g.GetMetadata().GetId(), g.GetSpec().GetNetworkId())
			}
		}
	})
	t.Run("unknown filter value is empty, not an error", func(t *testing.T) {
		t.Parallel()
		r, err := c.subnets.ListByNetwork(ctx, &vpcpb.ListSubnetsByNetworkRequest{NetworkId: "vpcnetwork-e00missing"})
		if err != nil || len(r.GetItems()) != 0 {
			t.Errorf("ListByNetwork(missing) = %v, err %v", r.GetItems(), err)
		}
	})
	t.Run("list by parent and aggregated", func(t *testing.T) {
		t.Parallel()
		for _, parent := range []string{project} {
			agg, err := c.subnets.ListAggregated(ctx, &vpcpb.ListAggregatedSubnetsRequest{ParentId: parent})
			if err != nil {
				t.Fatal(err)
			}
			if got := idsOfRes(agg.GetItems()); !hasID(got, subA1) || !hasID(got, subB) {
				t.Errorf("ListAggregated(%s) = %v, want to include created subnets", parent, got)
			}
		}
	})
}

func TestVPCDefaultNetwork(t *testing.T) {
	t.Parallel()
	c := startVPC(t)
	ctx := t.Context()
	project := c.inst.ProjectID

	def, err := c.networks.GetDefault(ctx, &vpcpb.GetDefaultNetworkRequest{ParentId: project})
	if err != nil {
		t.Fatalf("GetDefault: %v", err)
	}
	n, sn := def.GetNetwork(), def.GetSubnet()
	if n.GetMetadata().GetName() != "default-network" || !n.GetStatus().GetDefault() {
		t.Errorf("default network = %v", n)
	}
	if sn.GetMetadata().GetName() != "default-subnet" || !sn.GetStatus().GetDefault() || sn.GetSpec().GetNetworkId() != n.GetMetadata().GetId() {
		t.Errorf("default subnet = %v", sn)
	}
	fromGet, err := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: n.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(fromGet, n, protocmp.Transform()); diff != "" {
		t.Errorf("GetDefault network differs from Get (-get +default):\n%s", diff)
	}

	// The network sits on the seeded pools.
	priv, pub := seedPoolID(t, c, seedPrivatePool), seedPoolID(t, c, seedPublicPool)
	if got := n.GetSpec().GetIpv4PrivatePools().GetPools(); len(got) != 1 || got[0].GetId() != priv {
		t.Errorf("default network private pools = %v, want [%s]", got, priv)
	}
	if got := n.GetSpec().GetIpv4PublicPools().GetPools(); len(got) != 1 || got[0].GetId() != pub {
		t.Errorf("default network public pools = %v, want [%s]", got, pub)
	}
	for name, want := range map[string]string{seedPrivatePool: seedPrivateCIDR, seedPublicPool: seedPublicCIDR} {
		p, err := c.pools.GetByName(ctx, &vpcpb.GetPoolByNameRequest{ParentId: project, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff([]string{want}, p.GetStatus().GetCidrs()); diff != "" {
			t.Errorf("pool %s cidrs (-want +got):\n%s", name, diff)
		}
		if p.GetStatus().GetState() != vpcpb.PoolStatus_READY {
			t.Errorf("pool %s state = %v", name, p.GetStatus().GetState())
		}
	}
	if got := subnetCIDRs(t, c, sn.GetMetadata().GetId()); len(got) == 0 {
		t.Errorf("default subnet has no private CIDRs")
	}

	_, err = c.networks.CreateDefault(ctx, &vpcpb.CreateDefaultNetworkRequest{Metadata: md(project, "again")})
	assertCode(t, err, codes.AlreadyExists)
	_, err = c.networks.CreateDefault(ctx, &vpcpb.CreateDefaultNetworkRequest{Metadata: md("", "x")})
	assertCode(t, err, codes.InvalidArgument)
	_, err = c.networks.GetDefault(ctx, &vpcpb.GetDefaultNetworkRequest{ParentId: "project-e00nothere"})
	assertCode(t, err, codes.NotFound)

	// Every network gets a default route table (with a 0.0.0.0/0 route) and a default security group.
	netID := newNetwork(t, c, "extra")
	extra, err := c.networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: netID})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := c.routeTables.Get(ctx, &vpcpb.GetRouteTableRequest{Id: extra.GetStatus().GetDefaultRouteTableId()})
	if err != nil {
		t.Fatalf("default route table: %v", err)
	}
	if !rt.GetStatus().GetDefault() || rt.GetSpec().GetNetworkId() != netID {
		t.Errorf("default route table = %v", rt)
	}
	routes, err := c.routes.List(ctx, &vpcpb.ListRoutesRequest{ParentId: rt.GetMetadata().GetId()})
	if err != nil || len(routes.GetItems()) != 1 {
		t.Fatalf("default routes = %v, err %v", routes.GetItems(), err)
	}
	r := routes.GetItems()[0]
	if r.GetSpec().GetDestination().GetCidr() != "0.0.0.0/0" || !r.GetSpec().GetNextHop().GetDefaultEgressGateway() {
		t.Errorf("default route = %v, want 0.0.0.0/0 via the default egress gateway", r.GetSpec())
	}
	groups, err := c.groups.ListByNetwork(ctx, &vpcpb.ListSecurityGroupsByNetworkRequest{NetworkId: netID})
	if err != nil || len(groups.GetItems()) != 1 || !groups.GetItems()[0].GetStatus().GetDefault() {
		t.Errorf("default security groups = %v, err %v", groups.GetItems(), err)
	}
}
