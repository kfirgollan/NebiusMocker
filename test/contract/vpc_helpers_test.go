package contract

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/operations"
	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// vpcRes is any VPC resource.
type vpcRes interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// Names and CIDRs of the pools seeded in every project.
const (
	seedPrivatePool = "default-network-pool"
	seedPublicPool  = "default-public-pool"
	seedPrivateCIDR = "10.0.0.0/13"
	seedPublicCIDR  = "203.0.113.0/24"
)

// resetCtx returns a context whose Update calls carry an X-ResetMask header.
func resetCtx(ctx context.Context, t *testing.T, path string) context.Context {
	t.Helper()
	out, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust(path))
	if err != nil {
		t.Fatalf("add reset mask: %v", err)
	}
	return out
}

// waitOK waits for a started operation and returns its error, if any:
// waitOK(ctx)(svc.Update(...)).
func waitOK(ctx context.Context) func(operations.Operation, error) error {
	return func(op operations.Operation, err error) error {
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
}

func seedPoolID(t *testing.T, c vpcClients, name string) string {
	t.Helper()
	p, err := c.pools.GetByName(t.Context(), &vpcpb.GetPoolByNameRequest{ParentId: c.inst.ProjectID, Name: name})
	if err != nil {
		t.Fatalf("seeded pool %s: %v", name, err)
	}
	return p.GetMetadata().GetId()
}

func poolReq(parent, name, source string, vis vpcpb.IpVisibility, cidrs ...string) *vpcpb.CreatePoolRequest {
	spec := &vpcpb.PoolSpec{SourcePoolId: source, Version: vpcpb.IpVersion_IPV4, Visibility: vis}
	for _, c := range cidrs {
		spec.Cidrs = append(spec.Cidrs, &vpcpb.PoolCidr{Cidr: c})
	}
	return &vpcpb.CreatePoolRequest{Metadata: md(parent, name), Spec: spec}
}

// newPrivatePool creates a private pool carved from source.
func newPrivatePool(t *testing.T, c vpcClients, name, source string, cidrs ...string) string {
	t.Helper()
	return wait(t)(c.pools.Create(t.Context(), poolReq(c.inst.ProjectID, name, source, vpcpb.IpVisibility_PRIVATE, cidrs...)))
}

// newNetwork creates a network; the given private pools replace the default one.
func newNetwork(t *testing.T, c vpcClients, name string, privatePools ...string) string {
	t.Helper()
	spec := &vpcpb.NetworkSpec{}
	if len(privatePools) > 0 {
		spec.Ipv4PrivatePools = &vpcpb.IPv4PrivateNetworkPools{}
		for _, id := range privatePools {
			spec.Ipv4PrivatePools.Pools = append(spec.Ipv4PrivatePools.Pools, &vpcpb.NetworkPool{Id: id})
		}
	}
	return wait(t)(c.networks.Create(t.Context(), &vpcpb.CreateNetworkRequest{Metadata: md(c.inst.ProjectID, name), Spec: spec}))
}

// netWithPool creates a private pool of the given size from the default pool
// and a network on it.
func netWithPool(t *testing.T, c vpcClients, name, size string) (poolID, netID string) {
	t.Helper()
	poolID = newPrivatePool(t, c, name+"-pool", seedPoolID(t, c, seedPrivatePool), size)
	return poolID, newNetwork(t, c, name, poolID)
}

func subnetSpec(netID string, cidrs ...string) *vpcpb.SubnetSpec {
	spec := &vpcpb.SubnetSpec{NetworkId: netID}
	if len(cidrs) > 0 {
		sp := &vpcpb.SubnetPool{}
		for _, c := range cidrs {
			sp.Cidrs = append(sp.Cidrs, &vpcpb.SubnetCidr{Cidr: c})
		}
		spec.Ipv4PrivatePools = &vpcpb.IPv4PrivateSubnetPools{Pools: []*vpcpb.SubnetPool{sp}}
	}
	return spec
}

func createSubnetReq(c vpcClients, name string, spec *vpcpb.SubnetSpec) *vpcpb.CreateSubnetRequest {
	return &vpcpb.CreateSubnetRequest{Metadata: md(c.inst.ProjectID, name), Spec: spec}
}

// newSubnet creates a subnet with explicit (or "/N") private CIDRs.
func newSubnet(t *testing.T, c vpcClients, name, netID string, cidrs ...string) string {
	t.Helper()
	return wait(t)(c.subnets.Create(t.Context(), createSubnetReq(c, name, subnetSpec(netID, cidrs...))))
}

// subnetCIDRs returns the private blocks a subnet reserved.
func subnetCIDRs(t *testing.T, c vpcClients, id string) []string {
	t.Helper()
	sn, err := c.subnets.Get(t.Context(), &vpcpb.GetSubnetRequest{Id: id})
	if err != nil {
		t.Fatalf("Get subnet %s: %v", id, err)
	}
	var out []string
	for _, p := range sn.GetStatus().GetIpv4PrivatePools() {
		out = append(out, p.GetCidrs()...)
	}
	return out
}

func privAllocSpec(subnetID, poolID, cidr string) *vpcpb.AllocationSpec {
	p := &vpcpb.IPv4PrivateAllocationSpec{Cidr: cidr}
	switch {
	case subnetID != "":
		p.Pool = &vpcpb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: subnetID}
	case poolID != "":
		p.Pool = &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: poolID}
	}
	return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: p}}
}

func pubAllocSpec(subnetID, poolID, cidr string) *vpcpb.AllocationSpec {
	p := &vpcpb.IPv4PublicAllocationSpec{Cidr: cidr}
	switch {
	case subnetID != "":
		p.Pool = &vpcpb.IPv4PublicAllocationSpec_SubnetId{SubnetId: subnetID}
	case poolID != "":
		p.Pool = &vpcpb.IPv4PublicAllocationSpec_PoolId{PoolId: poolID}
	}
	return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: p}}
}

func createAllocReq(c vpcClients, name string, spec *vpcpb.AllocationSpec) *vpcpb.CreateAllocationRequest {
	return &vpcpb.CreateAllocationRequest{Metadata: md(c.inst.ProjectID, name), Spec: spec}
}

func newAlloc(t *testing.T, c vpcClients, name string, spec *vpcpb.AllocationSpec) string {
	t.Helper()
	return wait(t)(c.allocations.Create(t.Context(), createAllocReq(c, name, spec)))
}

// allocCIDR returns the address an allocation received.
func allocCIDR(t *testing.T, c vpcClients, id string) netip.Prefix {
	t.Helper()
	a, err := c.allocations.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: id})
	if err != nil {
		t.Fatalf("Get allocation %s: %v", id, err)
	}
	p, err := netip.ParsePrefix(a.GetStatus().GetDetails().GetAllocatedCidr())
	if err != nil {
		t.Fatalf("allocation %s: allocated_cidr %q: %v", id, a.GetStatus().GetDetails().GetAllocatedCidr(), err)
	}
	return p
}

func newRouteTable(t *testing.T, c vpcClients, name, netID string) string {
	t.Helper()
	return wait(t)(c.routeTables.Create(t.Context(), &vpcpb.CreateRouteTableRequest{
		Metadata: md(c.inst.ProjectID, name), Spec: &vpcpb.RouteTableSpec{NetworkId: netID},
	}))
}

func allocHop(id string) *vpcpb.NextHop {
	return &vpcpb.NextHop{NextHop: &vpcpb.NextHop_Allocation{Allocation: &vpcpb.AllocationNextHop{Id: id}}}
}

func egressHop() *vpcpb.NextHop {
	return &vpcpb.NextHop{NextHop: &vpcpb.NextHop_DefaultEgressGateway{DefaultEgressGateway: true}}
}

func routeReq(rtID, name, dst string, hop *vpcpb.NextHop) *vpcpb.CreateRouteRequest {
	return &vpcpb.CreateRouteRequest{Metadata: md(rtID, name), Spec: &vpcpb.RouteSpec{
		Destination: &vpcpb.DestinationMatch{Cidr: dst}, NextHop: hop,
	}}
}

func newRoute(t *testing.T, c vpcClients, rtID, name, dst string, hop *vpcpb.NextHop) string {
	t.Helper()
	return wait(t)(c.routes.Create(t.Context(), routeReq(rtID, name, dst, hop)))
}

func newGroup(t *testing.T, c vpcClients, name, netID string) string {
	t.Helper()
	return wait(t)(c.groups.Create(t.Context(), &vpcpb.CreateSecurityGroupRequest{
		Metadata: md(c.inst.ProjectID, name), Spec: &vpcpb.SecurityGroupSpec{NetworkId: netID},
	}))
}

func ingressRule(cidrs []string, ports ...int32) *vpcpb.SecurityRuleSpec {
	return &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP,
		Match: &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceCidrs: cidrs, DestinationPorts: ports}},
	}
}

func ruleReq(sgID, name string, spec *vpcpb.SecurityRuleSpec) *vpcpb.CreateSecurityRuleRequest {
	return &vpcpb.CreateSecurityRuleRequest{Metadata: md(sgID, name), Spec: spec}
}

func newRule(t *testing.T, c vpcClients, sgID, name string, spec *vpcpb.SecurityRuleSpec) string {
	t.Helper()
	return wait(t)(c.rules.Create(t.Context(), ruleReq(sgID, name, spec)))
}

// pageAll walks every page of a list call with the given page size.
func pageAll[T any](t *testing.T, size int64, fetch func(size int64, token string) ([]T, string, error)) []T {
	t.Helper()
	var out []T
	token := ""
	for pages := 0; ; pages++ {
		if pages > 200 {
			t.Fatal("paging did not terminate")
		}
		items, next, err := fetch(size, token)
		if err != nil {
			t.Fatalf("list page %d: %v", pages, err)
		}
		if size > 0 && int64(len(items)) > size {
			t.Fatalf("page %d has %d items, page_size %d", pages, len(items), size)
		}
		out = append(out, items...)
		if token = next; token == "" {
			return out
		}
	}
}

func idsOfRes[T vpcRes](items []T) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.GetMetadata().GetId())
	}
	return out
}

func asRes[T vpcRes](items []T) []vpcRes {
	out := make([]vpcRes, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	return out
}

func hasID(ids []string, id string) bool { return slices.Contains(ids, id) }

func nameN(prefix string, n int) string { return fmt.Sprintf("%s-%d", prefix, n) }

func codeOf(err error) codes.Code { return status.Code(err) }

func getPool(t *testing.T, c vpcClients, id string) *vpcpb.Pool {
	t.Helper()
	r, err := c.pools.Get(t.Context(), &vpcpb.GetPoolRequest{Id: id})
	if err != nil {
		t.Fatalf("Get Pool %s: %v", id, err)
	}
	return r
}

func getNetwork(t *testing.T, c vpcClients, id string) *vpcpb.Network {
	t.Helper()
	r, err := c.networks.Get(t.Context(), &vpcpb.GetNetworkRequest{Id: id})
	if err != nil {
		t.Fatalf("Get Network %s: %v", id, err)
	}
	return r
}

func getSubnet(t *testing.T, c vpcClients, id string) *vpcpb.Subnet {
	t.Helper()
	r, err := c.subnets.Get(t.Context(), &vpcpb.GetSubnetRequest{Id: id})
	if err != nil {
		t.Fatalf("Get Subnet %s: %v", id, err)
	}
	return r
}

func getAllocation(t *testing.T, c vpcClients, id string) *vpcpb.Allocation {
	t.Helper()
	r, err := c.allocations.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: id})
	if err != nil {
		t.Fatalf("Get Allocation %s: %v", id, err)
	}
	return r
}

func getRoute(t *testing.T, c vpcClients, id string) *vpcpb.Route {
	t.Helper()
	r, err := c.routes.Get(t.Context(), &vpcpb.GetRouteRequest{Id: id})
	if err != nil {
		t.Fatalf("Get Route %s: %v", id, err)
	}
	return r
}

func getSecurityRule(t *testing.T, c vpcClients, id string) *vpcpb.SecurityRule {
	t.Helper()
	r, err := c.rules.Get(t.Context(), &vpcpb.GetSecurityRuleRequest{Id: id})
	if err != nil {
		t.Fatalf("Get SecurityRule %s: %v", id, err)
	}
	return r
}
