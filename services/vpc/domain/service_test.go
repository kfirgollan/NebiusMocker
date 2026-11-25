package domain

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

const (
	testProject     = "project-e00test0000"
	testPrivateCIDR = "10.0.0.0/16"
	testPublicCIDR  = "203.0.113.0/24"
)

// env is a VPC service over in-memory fakes with the project's defaults seeded.
type env struct {
	svc *Service
	clk *clock.Manual
	def DefaultNetwork
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	svc, err := New(Deps{
		Repos: Repos{
			Pools:          newFakeRepo[*vpcpb.Pool](),
			Networks:       newFakeRepo[*vpcpb.Network](),
			Subnets:        newFakeRepo[*vpcpb.Subnet](),
			Allocations:    newFakeRepo[*vpcpb.Allocation](),
			RouteTables:    newFakeRepo[*vpcpb.RouteTable](),
			Routes:         newFakeRepo[*vpcpb.Route](),
			SecurityGroups: newFakeRepo[*vpcpb.SecurityGroup](),
			SecurityRules:  newFakeRepo[*vpcpb.SecurityRule](),
		},
		Ops:       &inlineOps{},
		IDs:       &seqIDs{},
		Clock:     clk,
		Hierarchy: fakeHierarchy{known: map[string]bool{"project-e00other": true}},
		Config:    Config{ProjectID: testProject, DefaultPrivateCIDR: testPrivateCIDR, PublicCIDR: testPublicCIDR},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Networks().SeedDefault(t.Context(), testProject); err != nil {
		t.Fatalf("SeedDefault: %v", err)
	}
	def, err := svc.Networks().GetDefault(t.Context(), testProject)
	if err != nil {
		t.Fatalf("GetDefault: %v", err)
	}
	return &env{svc: svc, clk: clk, def: def}
}

// --- generic helpers over the exported sub-services ---

type (
	createFn[T Resource, S proto.Message] func(context.Context, CreateCmd[S]) (*operations.Operation, error)
	getFn[T Resource]                     func(context.Context, string) (T, error)
	updateFn[S proto.Message]             func(context.Context, UpdateCmd[S]) (*operations.Operation, error)
	deleteFn                              func(context.Context, string, proto.Message) (*operations.Operation, error)
)

// tryCreate creates a resource and returns it, or the request or operation error.
func tryCreate[T Resource, S proto.Message](ctx context.Context, create createFn[T, S], get getFn[T], parent, name string, spec S) (T, error) {
	var zero T
	op, err := create(ctx, CreateCmd[S]{Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: spec})
	if err != nil {
		return zero, err
	}
	if op.Err != nil {
		return zero, op.Err
	}
	return get(ctx, op.ResourceID)
}

func mustCreate[T Resource, S proto.Message](t *testing.T, create createFn[T, S], get getFn[T], parent, name string, spec S) T {
	t.Helper()
	r, err := tryCreate(t.Context(), create, get, parent, name, spec)
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return r
}

// tryUpdate runs an update and returns the re-read resource.
func tryUpdate[T Resource, S proto.Message](ctx context.Context, update updateFn[S], get getFn[T], cmd UpdateCmd[S]) (T, error) {
	var zero T
	op, err := update(ctx, cmd)
	if err != nil {
		return zero, err
	}
	if op.Err != nil {
		return zero, op.Err
	}
	return get(ctx, cmd.Metadata.GetId())
}

// tryDelete runs a delete and returns the request or operation error.
func tryDelete(ctx context.Context, del deleteFn, id string) error {
	op, err := del(ctx, id, nil)
	if err != nil {
		return err
	}
	return op.Err
}

func (e *env) createNetwork(t *testing.T, name string) *vpcpb.Network {
	t.Helper()
	return mustCreate(t, e.svc.Networks().Create, e.svc.Networks().Get, testProject, name, &vpcpb.NetworkSpec{})
}

func subnetSpec(networkID string, cidrs ...string) *vpcpb.SubnetSpec {
	spec := &vpcpb.SubnetSpec{NetworkId: networkID}
	if len(cidrs) > 0 {
		sp := &vpcpb.SubnetPool{}
		for _, c := range cidrs {
			sp.Cidrs = append(sp.Cidrs, &vpcpb.SubnetCidr{Cidr: c})
		}
		spec.Ipv4PrivatePools = &vpcpb.IPv4PrivateSubnetPools{Pools: []*vpcpb.SubnetPool{sp}}
	}
	return spec
}

func (e *env) trySubnet(t *testing.T, name, networkID string, cidrs ...string) (*vpcpb.Subnet, error) {
	t.Helper()
	return tryCreate(t.Context(), e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, name, subnetSpec(networkID, cidrs...))
}

func (e *env) createSubnet(t *testing.T, name string, cidrs ...string) *vpcpb.Subnet {
	t.Helper()
	return mustCreate(t, e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, name, subnetSpec(e.def.Network.GetMetadata().GetId(), cidrs...))
}

func privateCIDRs(sn *vpcpb.Subnet) []string {
	var out []string
	for _, p := range sn.GetStatus().GetIpv4PrivatePools() {
		out = append(out, p.GetCidrs()...)
	}
	return out
}

func privateAllocSpec(subnetID, cidr string) *vpcpb.AllocationSpec {
	return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
		Cidr: cidr, Pool: &vpcpb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: subnetID},
	}}}
}

func publicAllocSpec(cidr string) *vpcpb.AllocationSpec {
	return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{Cidr: cidr}}}
}

func (e *env) tryAlloc(t *testing.T, name string, spec *vpcpb.AllocationSpec) (*vpcpb.Allocation, error) {
	t.Helper()
	return tryCreate(t.Context(), e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, name, spec)
}

func (e *env) createAlloc(t *testing.T, name, subnetID, cidr string) *vpcpb.Allocation {
	t.Helper()
	return mustCreate(t, e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, name, privateAllocSpec(subnetID, cidr))
}

func (e *env) poolByName(t *testing.T, name string) *vpcpb.Pool {
	t.Helper()
	p, err := e.svc.Pools().GetByName(t.Context(), testProject, name)
	if err != nil {
		t.Fatalf("pool %q: %v", name, err)
	}
	return p
}

func (e *env) createRouteTable(t *testing.T, name, networkID string) *vpcpb.RouteTable {
	t.Helper()
	return mustCreate(t, e.svc.RouteTables().Create, e.svc.RouteTables().Get, testProject, name, &vpcpb.RouteTableSpec{NetworkId: networkID})
}

func (e *env) createSecurityGroup(t *testing.T, name, networkID string) *vpcpb.SecurityGroup {
	t.Helper()
	return mustCreate(t, e.svc.SecurityGroups().Create, e.svc.SecurityGroups().Get, testProject, name, &vpcpb.SecurityGroupSpec{NetworkId: networkID})
}

func idOf(r Resource) string { return r.GetMetadata().GetId() }

func wantKind(t *testing.T, what string, err error, want errs.Kind) {
	t.Helper()
	if got := errs.KindOf(err); got != want {
		t.Errorf("%s: kind = %v, want %v (err: %v)", what, got, want, err)
	}
}

// --- seeded defaults ---

func TestSeededDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()

	n := e.def.Network
	if n.GetMetadata().GetName() != DefaultNetworkName || !n.GetStatus().GetDefault() {
		t.Errorf("default network = %q default=%v, want %q true", n.GetMetadata().GetName(), n.GetStatus().GetDefault(), DefaultNetworkName)
	}
	if n.GetMetadata().GetParentId() != testProject {
		t.Errorf("default network parent = %q, want %q", n.GetMetadata().GetParentId(), testProject)
	}
	sn := e.def.Subnet
	if sn == nil || sn.GetMetadata().GetName() != DefaultSubnetName || !sn.GetStatus().GetDefault() {
		t.Fatalf("default subnet = %v, want %q default", sn, DefaultSubnetName)
	}
	if sn.GetSpec().GetNetworkId() != idOf(n) {
		t.Errorf("default subnet network = %q, want %q", sn.GetSpec().GetNetworkId(), idOf(n))
	}
	if got := privateCIDRs(sn); !slices.Equal(got, []string{testPrivateCIDR}) {
		t.Errorf("default subnet private cidrs = %v, want [%s]", got, testPrivateCIDR)
	}

	priv := e.poolByName(t, DefaultPrivatePoolName)
	if priv.GetSpec().GetVisibility() != vpcpb.IpVisibility_PRIVATE || !slices.Equal(priv.GetStatus().GetCidrs(), []string{testPrivateCIDR}) {
		t.Errorf("private pool = %v", priv)
	}
	pub := e.poolByName(t, DefaultPublicPoolName)
	if pub.GetSpec().GetVisibility() != vpcpb.IpVisibility_PUBLIC || !slices.Equal(pub.GetStatus().GetCidrs(), []string{testPublicCIDR}) {
		t.Errorf("public pool = %v", pub)
	}
	if got := priv.GetStatus().GetAssignment().GetNetworks(); !slices.Equal(got, []string{idOf(n)}) {
		t.Errorf("private pool assignment networks = %v, want [%s]", got, idOf(n))
	}

	rt, err := e.svc.RouteTables().Get(ctx, n.GetStatus().GetDefaultRouteTableId())
	if err != nil {
		t.Fatalf("default route table: %v", err)
	}
	if !rt.GetStatus().GetDefault() || rt.GetSpec().GetNetworkId() != idOf(n) {
		t.Errorf("default route table = %v", rt)
	}
	if rt.GetMetadata().GetName() == "" || rt.GetMetadata().GetName() == DefaultRouteTablePrefix {
		t.Errorf("default route table name = %q, want %q<suffix>", rt.GetMetadata().GetName(), DefaultRouteTablePrefix)
	}
	if got := sn.GetStatus().GetRouteTable(); got.GetId() != idOf(rt) || !got.GetDefault() {
		t.Errorf("default subnet route table = %v, want default %s", got, idOf(rt))
	}
	routes, err := e.svc.Routes().List(ctx, idOf(rt), 0, "")
	if err != nil || len(routes.Items) != 1 {
		t.Fatalf("default routes = %v, %v; want one", routes.Items, err)
	}
	r := routes.Items[0]
	if r.GetSpec().GetDestination().GetCidr() != "0.0.0.0/0" || !r.GetSpec().GetNextHop().GetDefaultEgressGateway() {
		t.Errorf("default route = %v, want 0.0.0.0/0 via default egress gateway", r)
	}

	sgs, err := e.svc.SecurityGroups().ListByNetwork(ctx, idOf(n), 0, "")
	if err != nil || len(sgs.Items) != 1 || !sgs.Items[0].GetStatus().GetDefault() {
		t.Fatalf("default security groups = %v, %v; want one default", sgs.Items, err)
	}
}

func TestSeedDefaultIsIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.svc.Networks().SeedDefault(t.Context(), testProject); err != nil {
		t.Fatalf("second SeedDefault: %v", err)
	}
	nets, err := e.svc.Networks().List(t.Context(), testProject, 0, "")
	if err != nil || len(nets.Items) != 1 {
		t.Fatalf("networks after reseed = %d, %v; want 1", len(nets.Items), err)
	}
	pools, err := e.svc.Pools().List(t.Context(), testProject, 0, "")
	if err != nil || len(pools.Items) != 2 {
		t.Fatalf("pools after reseed = %d, %v; want 2", len(pools.Items), err)
	}
}

func TestCreateDefaultRefusedWhenOneExists(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.svc.Networks().CreateDefault(t.Context(), &commonpb.ResourceMetadata{ParentId: testProject, Name: "again"}, nil)
	wantKind(t, "CreateDefault twice", err, errs.KindAlreadyExists)
}

// --- create validation ---

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tests := []struct {
		name string
		md   *commonpb.ResourceMetadata
		want errs.Kind
	}{
		{"ok", &commonpb.ResourceMetadata{ParentId: testProject, Name: "ok"}, errs.KindUnknown},
		{"ok other known project", &commonpb.ResourceMetadata{ParentId: "project-e00other", Name: "ok"}, errs.KindNotFound}, // no default pools there
		{"client supplied id", &commonpb.ResourceMetadata{Id: "vpcnetwork-x", ParentId: testProject, Name: "a"}, errs.KindBadRequest},
		{"no parent", &commonpb.ResourceMetadata{Name: "a"}, errs.KindBadRequest},
		{"parent not a project", &commonpb.ResourceMetadata{ParentId: "tenant-e00test", Name: "a"}, errs.KindBadRequest},
		{"unknown project", &commonpb.ResourceMetadata{ParentId: "project-e00nope", Name: "a"}, errs.KindNotFound},
		{"duplicate name", &commonpb.ResourceMetadata{ParentId: testProject, Name: DefaultNetworkName}, errs.KindAlreadyExists},
	}
	for _, tt := range tests {
		_, err := e.svc.Networks().Create(t.Context(), CreateCmd[*vpcpb.NetworkSpec]{Metadata: tt.md, Spec: &vpcpb.NetworkSpec{}})
		wantKind(t, tt.name, err, tt.want)
	}
}

func TestCreatedMetadata(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	n := mustCreate(t, e.svc.Networks().Create, e.svc.Networks().Get, testProject, "meta", (*vpcpb.NetworkSpec)(nil))
	md := n.GetMetadata()
	if md.GetId() == "" || md.GetParentId() != testProject || md.GetName() != "meta" {
		t.Errorf("metadata = %v", md)
	}
	if md.GetResourceVersion() != 1 {
		t.Errorf("resource_version = %d, want 1", md.GetResourceVersion())
	}
	if !md.GetCreatedAt().AsTime().Equal(e.clk.Now()) || !md.GetUpdatedAt().AsTime().Equal(e.clk.Now()) {
		t.Errorf("timestamps = %v / %v, want %v", md.GetCreatedAt().AsTime(), md.GetUpdatedAt().AsTime(), e.clk.Now())
	}
	if n.GetStatus().GetState() != vpcpb.NetworkStatus_READY || n.GetStatus().GetDefault() {
		t.Errorf("status = %v, want READY and not default", n.GetStatus())
	}
	// Default pools are filled in.
	if got := n.GetSpec().GetIpv4PrivatePools().GetPools(); len(got) != 1 || got[0].GetId() != idOf(e.poolByName(t, DefaultPrivatePoolName)) {
		t.Errorf("private pools = %v, want the default private pool", got)
	}
	if got := n.GetSpec().GetIpv4PublicPools().GetPools(); len(got) != 1 || got[0].GetId() != idOf(e.poolByName(t, DefaultPublicPoolName)) {
		t.Errorf("public pools = %v, want the default public pool", got)
	}
}

func TestListPaging(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		e.createNetwork(t, n)
	}
	var names []string
	token := ""
	for range 10 {
		p, err := e.svc.Networks().List(t.Context(), testProject, 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, n := range p.Items {
			names = append(names, n.GetMetadata().GetName())
		}
		if p.NextPageToken == "" {
			break
		}
		token = p.NextPageToken
	}
	if want := []string{DefaultNetworkName, "a", "b", "c", "d"}; !slices.Equal(names, want) {
		t.Errorf("paged names = %v, want %v", names, want)
	}
	for _, size := range []int64{-1, maxPageSize + 1} {
		_, err := e.svc.Networks().List(t.Context(), testProject, size, "")
		wantKind(t, "page size", err, errs.KindBadRequest)
	}
}

// --- subnets and IPAM ---

func TestSubnetAutoAllocationTakesLowestFreeBlock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s1 := e.createSubnet(t, "s1", "/24")
	s2 := e.createSubnet(t, "s2", "/24")
	s3 := e.createSubnet(t, "s3", "/25")
	for i, tt := range []struct {
		sn   *vpcpb.Subnet
		want string
	}{{s1, "10.0.0.0/24"}, {s2, "10.0.1.0/24"}, {s3, "10.0.2.0/25"}} {
		if got := privateCIDRs(tt.sn); !slices.Equal(got, []string{tt.want}) {
			t.Errorf("subnet %d cidrs = %v, want [%s]", i+1, got, tt.want)
		}
	}
	// A freed block is reused.
	if err := tryDelete(t.Context(), e.svc.Subnets().Delete, idOf(s1)); err != nil {
		t.Fatalf("delete s1: %v", err)
	}
	s4 := e.createSubnet(t, "s4", "/24")
	if got := privateCIDRs(s4); !slices.Equal(got, []string{"10.0.0.0/24"}) {
		t.Errorf("subnet after free cidrs = %v, want [10.0.0.0/24]", got)
	}
}

func TestSubnetExplicitBlocks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createSubnet(t, "base", "10.0.4.0/24")
	tests := []struct {
		name string
		cidr string
		want errs.Kind
	}{
		{"free", "10.0.5.0/24", errs.KindUnknown},
		{"identical", "10.0.4.0/24", errs.KindBadRequest},
		{"inside used", "10.0.4.128/25", errs.KindBadRequest},
		{"covers used", "10.0.4.0/22", errs.KindBadRequest},
		{"outside network space", "192.168.0.0/24", errs.KindBadRequest},
		{"host bits set", "10.0.6.1/24", errs.KindBadRequest},
		{"garbage", "banana", errs.KindBadRequest},
		{"ipv6", "fd00::/64", errs.KindBadRequest},
	}
	for _, tt := range tests {
		_, err := e.trySubnet(t, "s-"+tt.name, idOf(e.def.Network), tt.cidr)
		wantKind(t, tt.name, err, tt.want)
	}
}

func TestSubnetOverlapsAcrossNetworksSharingAPool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createSubnet(t, "a", "10.0.0.0/24")
	other := e.createNetwork(t, "other")
	_, err := e.trySubnet(t, "b", idOf(other), "10.0.0.0/24")
	wantKind(t, "same block in another network on the same pool", err, errs.KindBadRequest)
	sn, err := e.trySubnet(t, "c", idOf(other), "/24")
	if err != nil {
		t.Fatalf("auto in other network: %v", err)
	}
	if got := privateCIDRs(sn); !slices.Equal(got, []string{"10.0.1.0/24"}) {
		t.Errorf("auto block in other network = %v, want [10.0.1.0/24]", got)
	}
}

func TestSubnetSpaceExhaustion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pool := mustCreate(t, e.svc.Pools().Create, e.svc.Pools().Get, testProject, "tiny", &vpcpb.PoolSpec{
		Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: "172.16.0.0/24"}},
	})
	// A network with only the tiny pool.
	n := mustCreate(t, e.svc.Networks().Create, e.svc.Networks().Get, testProject, "tiny-net", &vpcpb.NetworkSpec{
		Ipv4PrivatePools: &vpcpb.IPv4PrivateNetworkPools{Pools: []*vpcpb.NetworkPool{{Id: idOf(pool)}}},
	})
	if _, err := e.trySubnet(t, "one", idOf(n), "/25"); err != nil {
		t.Fatalf("first /25: %v", err)
	}
	if _, err := e.trySubnet(t, "two", idOf(n), "/25"); err != nil {
		t.Fatalf("second /25: %v", err)
	}
	_, err := e.trySubnet(t, "three", idOf(n), "/25")
	wantKind(t, "third /25", err, errs.KindBadState)
}

func TestSubnetUpdateKeepsResolvedBlock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.createSubnet(t, "first", "/24")
	second := e.createSubnet(t, "second", "/24")
	if got := privateCIDRs(second); !slices.Equal(got, []string{"10.0.1.0/24"}) {
		t.Fatalf("second cidrs = %v, want [10.0.1.0/24]", got)
	}
	if err := tryDelete(t.Context(), e.svc.Subnets().Delete, idOf(first)); err != nil {
		t.Fatalf("delete first: %v", err)
	}
	// 10.0.0.0/24 is free now, but "/24" must keep the block it already holds.
	e.clk.Advance(time.Minute)
	upd, err := tryUpdate(t.Context(), e.svc.Subnets().Update, e.svc.Subnets().Get, UpdateCmd[*vpcpb.SubnetSpec]{
		Metadata: &commonpb.ResourceMetadata{Id: idOf(second), Name: "second", Labels: map[string]string{"k": "v"}},
		Spec:     subnetSpec(e.def.Network.GetMetadata().GetId(), "/24"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := privateCIDRs(upd); !slices.Equal(got, []string{"10.0.1.0/24"}) {
		t.Errorf("cidrs after update = %v, want [10.0.1.0/24]", got)
	}
	if upd.GetMetadata().GetResourceVersion() != second.GetMetadata().GetResourceVersion()+1 {
		t.Errorf("resource_version = %d, want %d", upd.GetMetadata().GetResourceVersion(), second.GetMetadata().GetResourceVersion()+1)
	}
}

func TestSubnetUsesNetworkPoolsByDefault(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "shared")
	if !sn.GetSpec().GetIpv4PrivatePools().GetUseNetworkPools() {
		t.Errorf("use_network_pools = false, want true when no pools are given")
	}
	if got := privateCIDRs(sn); !slices.Equal(got, []string{testPrivateCIDR}) {
		t.Errorf("cidrs = %v, want the whole network pool [%s]", got, testPrivateCIDR)
	}
	both := subnetSpec(idOf(e.def.Network), "/24")
	both.Ipv4PrivatePools.UseNetworkPools = true
	_, err := tryCreate(t.Context(), e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, "both", both)
	wantKind(t, "pools and use_network_pools", err, errs.KindBadRequest)
}

func TestSubnetReferencesAreValidated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.trySubnet(t, "nonet", "")
	wantKind(t, "missing network", err, errs.KindBadRequest)
	_, err = e.trySubnet(t, "badnet", "vpcnetwork-e00missing")
	wantKind(t, "unknown network", err, errs.KindNotFound)

	otherNet := e.createNetwork(t, "other")
	rt := e.createRouteTable(t, "rt-other", idOf(otherNet))
	spec := subnetSpec(idOf(e.def.Network))
	spec.RouteTableId = idOf(rt)
	_, err = tryCreate(t.Context(), e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, "wrong-rt", spec)
	wantKind(t, "route table of another network", err, errs.KindBadRequest)
}

// --- pools ---

func newRootPool(t *testing.T, e *env, cidr string) *vpcpb.Pool {
	t.Helper()
	return mustCreate(t, e.svc.Pools().Create, e.svc.Pools().Get, testProject, "root", &vpcpb.PoolSpec{
		Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: cidr}},
	})
}

func childPoolSpec(sourceID string, cidrs ...string) *vpcpb.PoolSpec {
	spec := &vpcpb.PoolSpec{SourcePoolId: sourceID, Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE}
	for _, c := range cidrs {
		spec.Cidrs = append(spec.Cidrs, &vpcpb.PoolCidr{Cidr: c})
	}
	return spec
}

func TestPoolExhaustion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := newRootPool(t, e, "10.9.0.0/24")
	var got []string
	for _, name := range []string{"c1", "c2"} {
		p := mustCreate(t, e.svc.Pools().Create, e.svc.Pools().Get, testProject, name, childPoolSpec(idOf(root), "/25"))
		got = append(got, p.GetStatus().GetCidrs()...)
	}
	if want := []string{"10.9.0.0/25", "10.9.0.128/25"}; !slices.Equal(got, want) {
		t.Errorf("child pool cidrs = %v, want %v", got, want)
	}
	_, err := tryCreate(t.Context(), e.svc.Pools().Create, e.svc.Pools().Get, testProject, "c3", childPoolSpec(idOf(root), "/25"))
	wantKind(t, "third /25 child", err, errs.KindBadState)
	_, err = tryCreate(t.Context(), e.svc.Pools().Create, e.svc.Pools().Get, testProject, "c4", childPoolSpec(idOf(root), "/32"))
	wantKind(t, "/32 child of a full pool", err, errs.KindBadState)
}

func TestPoolValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := newRootPool(t, e, "10.9.0.0/24")
	tests := []struct {
		name string
		spec *vpcpb.PoolSpec
		want errs.Kind
	}{
		{"ok explicit child", childPoolSpec(idOf(root), "10.9.0.64/26"), errs.KindUnknown},
		{"child outside source", childPoolSpec(idOf(root), "10.8.0.0/24"), errs.KindBadRequest},
		{"unknown source", childPoolSpec("vpcpool-e00missing", "/25"), errs.KindNotFound},
		{"no visibility", &vpcpb.PoolSpec{Version: vpcpb.IpVersion_IPV4, Cidrs: []*vpcpb.PoolCidr{{Cidr: "10.1.0.0/24"}}}, errs.KindBadRequest},
		{"ipv6", &vpcpb.PoolSpec{Version: vpcpb.IpVersion_IPV6, Visibility: vpcpb.IpVisibility_PRIVATE}, errs.KindBadRequest},
		{"root with prefix length", &vpcpb.PoolSpec{Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE, Cidrs: []*vpcpb.PoolCidr{{Cidr: "/24"}}}, errs.KindBadRequest},
		{"visibility differs from source", &vpcpb.PoolSpec{SourcePoolId: idOf(root), Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PUBLIC, Cidrs: []*vpcpb.PoolCidr{{Cidr: "/25"}}}, errs.KindBadRequest},
	}
	for i, tt := range tests {
		_, err := tryCreate(t.Context(), e.svc.Pools().Create, e.svc.Pools().Get, testProject, "p"+string(rune('a'+i)), tt.spec)
		wantKind(t, tt.name, err, tt.want)
	}
}

func TestPoolListBySource(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := newRootPool(t, e, "10.9.0.0/24")
	for _, name := range []string{"c1", "c2", "c3"} {
		mustCreate(t, e.svc.Pools().Create, e.svc.Pools().Get, testProject, name, childPoolSpec(idOf(root), "/26"))
	}
	var names []string
	token := ""
	for range 5 {
		p, err := e.svc.Pools().ListBySourcePool(t.Context(), idOf(root), 2, token)
		if err != nil {
			t.Fatalf("ListBySourcePool: %v", err)
		}
		for _, c := range p.Items {
			names = append(names, c.GetMetadata().GetName())
		}
		if p.NextPageToken == "" {
			break
		}
		token = p.NextPageToken
	}
	if want := []string{"c1", "c2", "c3"}; !slices.Equal(names, want) {
		t.Errorf("children = %v, want %v", names, want)
	}
}

// --- allocations ---

func TestAllocationSkipsReservedAddresses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "alloc", "10.0.0.0/24")
	var got []string
	for _, name := range []string{"a1", "a2", "a3"} {
		a := e.createAlloc(t, name, idOf(sn), "/32")
		got = append(got, a.GetStatus().GetDetails().GetAllocatedCidr())
		if a.GetStatus().GetState() != vpcpb.AllocationStatus_ALLOCATED || a.GetStatus().GetDetails().GetSubnetId() != idOf(sn) {
			t.Errorf("allocation %s status = %v", name, a.GetStatus())
		}
	}
	// .0 network, .1 gateway and .255 broadcast are reserved.
	if want := []string{"10.0.0.2/32", "10.0.0.3/32", "10.0.0.4/32"}; !slices.Equal(got, want) {
		t.Fatalf("allocated = %v, want %v", got, want)
	}
	if got := e.createAlloc(t, "a4", idOf(sn), "/32").GetStatus().GetDetails().GetPoolId(); got != idOf(e.poolByName(t, DefaultPrivatePoolName)) {
		t.Errorf("allocation pool = %q, want the default private pool", got)
	}
	// The lowest free address is reused after a delete.
	a2, err := e.svc.Allocations().GetByName(t.Context(), testProject, "a2")
	if err != nil {
		t.Fatal(err)
	}
	if err := tryDelete(t.Context(), e.svc.Allocations().Delete, idOf(a2)); err != nil {
		t.Fatalf("delete a2: %v", err)
	}
	if got := e.createAlloc(t, "a5", idOf(sn), "/32").GetStatus().GetDetails().GetAllocatedCidr(); got != "10.0.0.3/32" {
		t.Errorf("allocated after delete = %s, want 10.0.0.3/32", got)
	}
}

func TestAllocationExplicitAddresses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "alloc", "10.0.0.0/24")
	e.createAlloc(t, "taken", idOf(sn), "10.0.0.10")
	tests := []struct {
		name string
		cidr string
		want errs.Kind
	}{
		{"free address", "10.0.0.11", errs.KindUnknown},
		{"already allocated", "10.0.0.10", errs.KindBadRequest},
		{"network address", "10.0.0.0", errs.KindBadRequest},
		{"gateway address", "10.0.0.1", errs.KindBadRequest},
		{"broadcast address", "10.0.0.255", errs.KindBadRequest},
		{"outside subnet", "10.0.1.5", errs.KindBadRequest},
		{"block overlapping allocation", "10.0.0.8/29", errs.KindBadRequest},
		{"garbage", "banana", errs.KindBadRequest},
	}
	for i, tt := range tests {
		_, err := e.tryAlloc(t, "x"+string(rune('a'+i)), privateAllocSpec(idOf(sn), tt.cidr))
		wantKind(t, tt.name, err, tt.want)
	}
}

func TestAllocationExhaustion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "tiny", "/30") // 10.0.0.0/30: only .2 is usable
	if got := privateCIDRs(sn); !slices.Equal(got, []string{"10.0.0.0/30"}) {
		t.Fatalf("subnet cidrs = %v", got)
	}
	if got := e.createAlloc(t, "only", idOf(sn), "/32").GetStatus().GetDetails().GetAllocatedCidr(); got != "10.0.0.2/32" {
		t.Errorf("allocated = %s, want 10.0.0.2/32", got)
	}
	_, err := e.tryAlloc(t, "more", privateAllocSpec(idOf(sn), "/32"))
	wantKind(t, "allocation in a full subnet", err, errs.KindBadState)
}

func TestAllocationInNetworkPoolSubnet(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.createAlloc(t, "a", idOf(e.def.Subnet), "/32")
	if got := a.GetStatus().GetDetails().GetAllocatedCidr(); got != "10.0.0.2/32" {
		t.Errorf("first allocation = %s, want 10.0.0.2/32 (network and gateway reserved)", got)
	}
}

func TestAllocationFromPools(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pub := e.poolByName(t, DefaultPublicPoolName)
	a := mustCreate(t, e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, "pub", publicAllocSpec("/32"))
	if got := a.GetStatus().GetDetails().GetAllocatedCidr(); got != "203.0.113.0/32" {
		t.Errorf("public allocation = %s, want 203.0.113.0/32 (default public pool)", got)
	}
	if a.GetStatus().GetDetails().GetPoolId() != idOf(pub) {
		t.Errorf("public allocation pool = %q, want %q", a.GetStatus().GetDetails().GetPoolId(), idOf(pub))
	}
	b := mustCreate(t, e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, "pub2", publicAllocSpec("/32"))
	if got := b.GetStatus().GetDetails().GetAllocatedCidr(); got != "203.0.113.1/32" {
		t.Errorf("second public allocation = %s, want 203.0.113.1/32", got)
	}

	// A private allocation needs a subnet or a pool; a pool of the wrong visibility is refused.
	_, err := e.tryAlloc(t, "nowhere", &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{Cidr: "/32"}}})
	wantKind(t, "private without subnet or pool", err, errs.KindBadRequest)
	_, err = e.tryAlloc(t, "wrongpool", &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
		Cidr: "/32", Pool: &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: idOf(pub)},
	}}})
	wantKind(t, "private from a public pool", err, errs.KindBadRequest)
	_, err = e.tryAlloc(t, "empty", &vpcpb.AllocationSpec{})
	wantKind(t, "empty spec", err, errs.KindBadRequest)

	priv := e.poolByName(t, DefaultPrivatePoolName)
	c, err := e.tryAlloc(t, "fromprivate", &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
		Cidr: "/32", Pool: &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: idOf(priv)},
	}}})
	if err != nil {
		t.Fatalf("allocation from private pool: %v", err)
	}
	if got := c.GetStatus().GetDetails().GetAllocatedCidr(); got != "10.0.0.0/32" {
		t.Errorf("pool allocation = %s, want 10.0.0.0/32 (pools reserve nothing)", got)
	}
}

func TestPoolExhaustedByAllocations(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := newRootPool(t, e, "10.9.0.0/31")
	spec := func() *vpcpb.AllocationSpec {
		return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
			Cidr: "/32", Pool: &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: idOf(root)},
		}}}
	}
	for i, want := range []string{"10.9.0.0/32", "10.9.0.1/32"} {
		a, err := e.tryAlloc(t, "a"+string(rune('0'+i)), spec())
		if err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
		if got := a.GetStatus().GetDetails().GetAllocatedCidr(); got != want {
			t.Errorf("allocation %d = %s, want %s", i, got, want)
		}
	}
	_, err := e.tryAlloc(t, "a2", spec())
	wantKind(t, "allocation in an exhausted pool", err, errs.KindBadState)
}

func TestAllocationListings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s1 := e.createSubnet(t, "s1", "/24")
	s2 := e.createSubnet(t, "s2", "/24")
	for _, n := range []string{"a", "b", "c"} {
		e.createAlloc(t, n+"1", idOf(s1), "/32")
		e.createAlloc(t, n+"2", idOf(s2), "/32")
	}
	var names []string
	token := ""
	for range 5 {
		p, err := e.svc.Allocations().ListBySubnet(t.Context(), idOf(s2), 2, token)
		if err != nil {
			t.Fatalf("ListBySubnet: %v", err)
		}
		for _, a := range p.Items {
			names = append(names, a.GetMetadata().GetName())
		}
		if p.NextPageToken == "" {
			break
		}
		token = p.NextPageToken
	}
	if want := []string{"a2", "b2", "c2"}; !slices.Equal(names, want) {
		t.Errorf("allocations of s2 = %v, want %v", names, want)
	}
	byPool, err := e.svc.Allocations().ListByPool(t.Context(), idOf(e.poolByName(t, DefaultPrivatePoolName)), 0, "")
	if err != nil || len(byPool.Items) != 6 {
		t.Errorf("allocations of the private pool = %d, %v; want 6", len(byPool.Items), err)
	}
}

// --- delete rules ---

func TestDeleteRefusedWhileReferenced(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	netID := idOf(e.def.Network)

	other := e.createNetwork(t, "other")
	sn := mustCreate(t, e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, "sn", subnetSpec(idOf(other), "/24"))
	alloc := e.createAlloc(t, "alloc", idOf(sn), "/32")
	rt := e.createRouteTable(t, "rt", idOf(other))
	route := mustCreate(t, e.svc.Routes().Create, e.svc.Routes().Get, idOf(rt), "to-vm", &vpcpb.RouteSpec{
		Destination: &vpcpb.DestinationMatch{Cidr: "10.99.0.0/16"},
		NextHop:     &vpcpb.NextHop{NextHop: &vpcpb.NextHop_Allocation{Allocation: &vpcpb.AllocationNextHop{Id: idOf(alloc)}}},
	})
	if got := route.GetStatus().GetNextHop().GetAllocation().GetCidr(); got != alloc.GetStatus().GetDetails().GetAllocatedCidr() {
		t.Errorf("route next hop status cidr = %q, want %q", got, alloc.GetStatus().GetDetails().GetAllocatedCidr())
	}
	srt := e.createRouteTable(t, "subnet-rt", idOf(other))
	rtSpec := subnetSpec(idOf(other))
	rtSpec.RouteTableId = idOf(srt)
	rtSubnet := mustCreate(t, e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, "sn-rt", rtSpec)
	sgA := e.createSecurityGroup(t, "sg-a", idOf(other))
	sgB := e.createSecurityGroup(t, "sg-b", idOf(other))
	rule := mustCreate(t, e.svc.SecurityRules().Create, e.svc.SecurityRules().Get, idOf(sgA), "from-b", &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_ANY,
		Match: &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceSecurityGroupId: idOf(sgB)}},
	})

	defaultRT := e.def.Network.GetStatus().GetDefaultRouteTableId()
	defaultSG, err := e.svc.SecurityGroups().ListByNetwork(ctx, netID, 0, "")
	if err != nil || len(defaultSG.Items) != 1 {
		t.Fatalf("default SG list = %v, %v", defaultSG.Items, err)
	}
	privPool := e.poolByName(t, DefaultPrivatePoolName)
	pubPool := e.poolByName(t, DefaultPublicPoolName)

	refused := []struct {
		name string
		del  deleteFn
		id   string
	}{
		{"pool used by a network", e.svc.Pools().Delete, idOf(privPool)},
		{"default public pool", e.svc.Pools().Delete, idOf(pubPool)},
		{"network with a subnet", e.svc.Networks().Delete, idOf(other)},
		{"subnet with an allocation", e.svc.Subnets().Delete, idOf(sn)},
		{"allocation used as next hop", e.svc.Allocations().Delete, idOf(alloc)},
		{"route table used by a subnet", e.svc.RouteTables().Delete, idOf(srt)},
		{"default route table", e.svc.RouteTables().Delete, defaultRT},
		{"default security group", e.svc.SecurityGroups().Delete, idOf(defaultSG.Items[0])},
		{"security group referenced by a rule", e.svc.SecurityGroups().Delete, idOf(sgB)},
	}
	for _, tt := range refused {
		wantKind(t, tt.name, tryDelete(ctx, tt.del, tt.id), errs.KindBadState)
	}

	// Release in dependency order; every step now succeeds.
	steps := []struct {
		name string
		del  deleteFn
		id   string
	}{
		{"route", e.svc.Routes().Delete, idOf(route)},
		{"allocation", e.svc.Allocations().Delete, idOf(alloc)},
		{"subnet", e.svc.Subnets().Delete, idOf(sn)},
		{"route table subnet", e.svc.Subnets().Delete, idOf(rtSubnet)},
		{"route table", e.svc.RouteTables().Delete, idOf(srt)},
		{"rule", e.svc.SecurityRules().Delete, idOf(rule)},
		{"referenced security group", e.svc.SecurityGroups().Delete, idOf(sgB)},
	}
	for _, s := range steps {
		if err := tryDelete(ctx, s.del, s.id); err != nil {
			t.Errorf("delete %s: %v", s.name, err)
		}
	}
	if _, err := e.svc.Allocations().Get(ctx, idOf(alloc)); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get deleted allocation err = %v, want NotFound", err)
	}
}

func TestDeleteUnknownIsNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	wantKind(t, "delete unknown network", tryDelete(t.Context(), e.svc.Networks().Delete, "vpcnetwork-e00missing"), errs.KindNotFound)
}

func TestNetworkDeleteCascades(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	n := e.createNetwork(t, "doomed")
	netID := idOf(n)

	rtID := n.GetStatus().GetDefaultRouteTableId()
	if rtID == "" {
		t.Fatal("network has no default route table")
	}
	routes, err := e.svc.Routes().List(ctx, rtID, 0, "")
	if err != nil || len(routes.Items) != 1 {
		t.Fatalf("routes of default route table = %v, %v; want 1", routes.Items, err)
	}
	sgs, err := e.svc.SecurityGroups().ListByNetwork(ctx, netID, 0, "")
	if err != nil || len(sgs.Items) != 1 {
		t.Fatalf("security groups = %v, %v; want 1", sgs.Items, err)
	}
	rule := mustCreate(t, e.svc.SecurityRules().Create, e.svc.SecurityRules().Get, idOf(sgs.Items[0]), "r", &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP,
		Match: &vpcpb.SecurityRuleSpec_Egress{Egress: &vpcpb.RuleEgress{DestinationCidrs: []string{"0.0.0.0/0"}}},
	})

	if err := tryDelete(ctx, e.svc.Networks().Delete, netID); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	if _, err := e.svc.Networks().Get(ctx, netID); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("network after delete err = %v, want NotFound", err)
	}
	if _, err := e.svc.RouteTables().Get(ctx, rtID); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("default route table after delete err = %v, want NotFound", err)
	}
	if got, _ := e.svc.Routes().List(ctx, rtID, 0, ""); len(got.Items) != 0 {
		t.Errorf("routes after delete = %d, want 0", len(got.Items))
	}
	if got, _ := e.svc.SecurityGroups().ListByNetwork(ctx, netID, 0, ""); len(got.Items) != 0 {
		t.Errorf("security groups after delete = %d, want 0", len(got.Items))
	}
	if _, err := e.svc.SecurityRules().Get(ctx, idOf(rule)); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("rule after delete err = %v, want NotFound", err)
	}
	// The shared pools are untouched and the default network survives.
	if _, err := e.svc.Networks().GetDefault(ctx, testProject); err != nil {
		t.Errorf("GetDefault after deleting another network: %v", err)
	}
	e.poolByName(t, DefaultPrivatePoolName)
}

func TestNetworkDeleteRefusedByCustomSecurityGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	n := e.createNetwork(t, "n")
	sg := e.createSecurityGroup(t, "custom", idOf(n))
	wantKind(t, "network with custom security group", tryDelete(t.Context(), e.svc.Networks().Delete, idOf(n)), errs.KindBadState)
	if err := tryDelete(t.Context(), e.svc.SecurityGroups().Delete, idOf(sg)); err != nil {
		t.Fatalf("delete security group: %v", err)
	}
	if err := tryDelete(t.Context(), e.svc.Networks().Delete, idOf(n)); err != nil {
		t.Errorf("delete network: %v", err)
	}
}

// --- updates ---

func TestUpdateImmutableFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	netID := idOf(e.def.Network)
	other := e.createNetwork(t, "other")

	root := newRootPool(t, e, "10.9.0.0/24")
	child := mustCreate(t, e.svc.Pools().Create, e.svc.Pools().Get, testProject, "child", childPoolSpec(idOf(root), "/25"))
	privPoolID := idOf(e.poolByName(t, DefaultPrivatePoolName))
	sn := e.createSubnet(t, "sn", "/24")
	alloc := e.createAlloc(t, "alloc", idOf(sn), "/32")
	rt := e.createRouteTable(t, "rt", netID)
	route := mustCreate(t, e.svc.Routes().Create, e.svc.Routes().Get, idOf(rt), "r", &vpcpb.RouteSpec{
		Destination: &vpcpb.DestinationMatch{Cidr: "10.77.0.0/16"},
		NextHop:     &vpcpb.NextHop{NextHop: &vpcpb.NextHop_Allocation{Allocation: &vpcpb.AllocationNextHop{Id: idOf(alloc)}}},
	})
	sg := e.createSecurityGroup(t, "sg", netID)
	rule := mustCreate(t, e.svc.SecurityRules().Create, e.svc.SecurityRules().Get, idOf(sg), "rule", &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP,
		Match: &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceCidrs: []string{"10.0.0.0/8"}}},
	})

	md := func(r Resource) *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: idOf(r), Name: r.GetMetadata().GetName()}
	}
	tests := []struct {
		name string
		do   func() error
	}{
		{"pool source", func() error {
			_, err := e.svc.Pools().Update(ctx, UpdateCmd[*vpcpb.PoolSpec]{Metadata: md(child), Spec: &vpcpb.PoolSpec{
				SourcePoolId: privPoolID, Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
				Cidrs: []*vpcpb.PoolCidr{{Cidr: "/25"}},
			}})
			return err
		}},
		{"pool visibility", func() error {
			spec := childPoolSpec(idOf(root), "/25")
			spec.Visibility = vpcpb.IpVisibility_PUBLIC
			_, err := e.svc.Pools().Update(ctx, UpdateCmd[*vpcpb.PoolSpec]{Metadata: md(child), Spec: spec})
			return err
		}},
		{"subnet network", func() error {
			_, err := e.svc.Subnets().Update(ctx, UpdateCmd[*vpcpb.SubnetSpec]{Metadata: md(sn), Spec: subnetSpec(idOf(other), "/24")})
			return err
		}},
		{"allocation spec", func() error {
			_, err := e.svc.Allocations().Update(ctx, UpdateCmd[*vpcpb.AllocationSpec]{Metadata: md(alloc), Spec: privateAllocSpec(idOf(sn), "10.0.0.50")})
			return err
		}},
		{"route table network", func() error {
			_, err := e.svc.RouteTables().Update(ctx, UpdateCmd[*vpcpb.RouteTableSpec]{Metadata: md(rt), Spec: &vpcpb.RouteTableSpec{NetworkId: idOf(other)}})
			return err
		}},
		{"route destination", func() error {
			_, err := e.svc.Routes().Update(ctx, UpdateCmd[*vpcpb.RouteSpec]{Metadata: md(route), Spec: &vpcpb.RouteSpec{
				Destination: &vpcpb.DestinationMatch{Cidr: "10.78.0.0/16"}, NextHop: route.GetSpec().GetNextHop(),
			}})
			return err
		}},
		{"route next hop", func() error {
			_, err := e.svc.Routes().Update(ctx, UpdateCmd[*vpcpb.RouteSpec]{Metadata: md(route), Spec: &vpcpb.RouteSpec{
				Destination: route.GetSpec().GetDestination(), NextHop: &vpcpb.NextHop{NextHop: &vpcpb.NextHop_DefaultEgressGateway{DefaultEgressGateway: true}},
			}})
			return err
		}},
		{"security group network", func() error {
			_, err := e.svc.SecurityGroups().Update(ctx, UpdateCmd[*vpcpb.SecurityGroupSpec]{Metadata: md(sg), Spec: &vpcpb.SecurityGroupSpec{NetworkId: idOf(other)}})
			return err
		}},
		{"security rule spec", func() error {
			_, err := e.svc.SecurityRules().Update(ctx, UpdateCmd[*vpcpb.SecurityRuleSpec]{Metadata: md(rule), Spec: &vpcpb.SecurityRuleSpec{
				Access: vpcpb.RuleAccessAction_DENY, Protocol: vpcpb.RuleProtocol_TCP,
				Match: &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceCidrs: []string{"10.0.0.0/8"}}},
			}})
			return err
		}},
		{"parent id", func() error {
			m := md(e.def.Network)
			m.ParentId = "project-e00other"
			_, err := e.svc.Networks().Update(ctx, UpdateCmd[*vpcpb.NetworkSpec]{Metadata: m, Spec: &vpcpb.NetworkSpec{}})
			return err
		}},
	}
	for _, tt := range tests {
		wantKind(t, tt.name, tt.do(), errs.KindBadRequest)
	}

	_, err := e.svc.Networks().Update(ctx, UpdateCmd[*vpcpb.NetworkSpec]{Metadata: &commonpb.ResourceMetadata{Name: "x"}, Spec: &vpcpb.NetworkSpec{}})
	wantKind(t, "missing id", err, errs.KindBadRequest)
	_, err = e.svc.Networks().Update(ctx, UpdateCmd[*vpcpb.NetworkSpec]{Metadata: &commonpb.ResourceMetadata{Id: "vpcnetwork-e00missing", Name: "x"}, Spec: &vpcpb.NetworkSpec{}})
	wantKind(t, "unknown id", err, errs.KindNotFound)
}

func TestUpdateKeepsImmutableFieldsOnSameValues(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	n := e.createNetwork(t, "n")
	e.clk.Advance(time.Hour)
	got, err := tryUpdate(t.Context(), e.svc.Networks().Update, e.svc.Networks().Get, UpdateCmd[*vpcpb.NetworkSpec]{
		Metadata: &commonpb.ResourceMetadata{Id: idOf(n), ParentId: testProject, Name: "renamed"},
		Spec:     &vpcpb.NetworkSpec{},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	md := got.GetMetadata()
	if md.GetName() != "renamed" || md.GetParentId() != testProject || md.GetId() != idOf(n) {
		t.Errorf("metadata = %v", md)
	}
	if !md.GetCreatedAt().AsTime().Equal(n.GetMetadata().GetCreatedAt().AsTime()) {
		t.Errorf("created_at changed: %v -> %v", n.GetMetadata().GetCreatedAt().AsTime(), md.GetCreatedAt().AsTime())
	}
	if !got.GetStatus().GetDefault() == n.GetStatus().GetDefault() && got.GetStatus().GetDefaultRouteTableId() != n.GetStatus().GetDefaultRouteTableId() {
		t.Errorf("server-owned status changed: %v -> %v", n.GetStatus(), got.GetStatus())
	}
}

func TestUpdateResetMaskClearsLabels(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	n, err := tryCreate(ctx, e.svc.Networks().Create, e.svc.Networks().Get, testProject, "labeled", &vpcpb.NetworkSpec{})
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.svc.Networks().Update(ctx, UpdateCmd[*vpcpb.NetworkSpec]{
		Metadata: &commonpb.ResourceMetadata{Id: idOf(n), Name: "labeled", Labels: map[string]string{"env": "dev", "team": "a"}},
		Spec:     &vpcpb.NetworkSpec{},
	})
	if err != nil || op.Err != nil {
		t.Fatalf("set labels: %v / %v", err, op)
	}
	n, _ = e.svc.Networks().Get(ctx, idOf(n))
	if got := n.GetMetadata().GetLabels(); len(got) != 2 {
		t.Fatalf("labels = %v, want 2", got)
	}

	// Without a reset mask an absent field keeps its value.
	e.clk.Advance(time.Minute)
	kept, err := tryUpdate(ctx, e.svc.Networks().Update, e.svc.Networks().Get, UpdateCmd[*vpcpb.NetworkSpec]{
		Metadata: &commonpb.ResourceMetadata{Id: idOf(n), Name: "labeled"}, Spec: &vpcpb.NetworkSpec{},
	})
	if err != nil {
		t.Fatalf("update without mask: %v", err)
	}
	if got := kept.GetMetadata().GetLabels(); len(got) != 2 {
		t.Errorf("labels without reset mask = %v, want unchanged", got)
	}
	if kept.GetMetadata().GetResourceVersion() != n.GetMetadata().GetResourceVersion() {
		t.Errorf("no-op update changed resource_version %d -> %d", n.GetMetadata().GetResourceVersion(), kept.GetMetadata().GetResourceVersion())
	}

	// With the mask the absent field is cleared.
	cleared, err := tryUpdate(ctx, e.svc.Networks().Update, e.svc.Networks().Get, UpdateCmd[*vpcpb.NetworkSpec]{
		Metadata:  &commonpb.ResourceMetadata{Id: idOf(n), Name: "labeled"},
		Spec:      &vpcpb.NetworkSpec{},
		ResetMask: mask.ParseMust("metadata.labels"),
	})
	if err != nil {
		t.Fatalf("update with mask: %v", err)
	}
	if got := cleared.GetMetadata().GetLabels(); len(got) != 0 {
		t.Errorf("labels with reset mask = %v, want none", got)
	}
	if cleared.GetMetadata().GetName() != "labeled" {
		t.Errorf("name = %q, want labeled", cleared.GetMetadata().GetName())
	}
	if cleared.GetMetadata().GetResourceVersion() != n.GetMetadata().GetResourceVersion()+1 {
		t.Errorf("resource_version = %d, want %d", cleared.GetMetadata().GetResourceVersion(), n.GetMetadata().GetResourceVersion()+1)
	}
}

func TestUpdateResourceVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	n := e.createNetwork(t, "versioned")
	created := n.GetMetadata().GetUpdatedAt().AsTime()
	v := n.GetMetadata().GetResourceVersion()

	update := func(labels map[string]string, version uint64) (*vpcpb.Network, error) {
		return tryUpdate(ctx, e.svc.Networks().Update, e.svc.Networks().Get, UpdateCmd[*vpcpb.NetworkSpec]{
			Metadata: &commonpb.ResourceMetadata{Id: idOf(n), Name: "versioned", Labels: labels, ResourceVersion: int64(version)},
			Spec:     &vpcpb.NetworkSpec{},
		})
	}

	e.clk.Advance(time.Minute)
	changed, err := update(map[string]string{"a": "1"}, 0)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := changed.GetMetadata().GetResourceVersion(); got != v+1 {
		t.Errorf("resource_version after change = %d, want %d", got, v+1)
	}
	if got := changed.GetMetadata().GetUpdatedAt().AsTime(); !got.Equal(e.clk.Now()) || !got.After(created) {
		t.Errorf("updated_at after change = %v, want %v", got, e.clk.Now())
	}

	e.clk.Advance(time.Minute)
	same, err := update(map[string]string{"a": "1"}, 0)
	if err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if same.GetMetadata().GetResourceVersion() != v+1 || !same.GetMetadata().GetUpdatedAt().AsTime().Equal(changed.GetMetadata().GetUpdatedAt().AsTime()) {
		t.Errorf("no-op update changed version/updated_at: %v", same.GetMetadata())
	}

	// A matching version is accepted; a stale one is a conflict.
	if _, err := update(map[string]string{"a": "2"}, uint64(v+1)); err != nil {
		t.Errorf("update with current version: %v", err)
	}
	_, err = update(map[string]string{"a": "3"}, uint64(v+1))
	wantKind(t, "stale resource_version", err, errs.KindConflict)
}

func TestPoolUpdateCannotDropUsedBlock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := newRootPool(t, e, "10.9.0.0/24")
	child := mustCreate(t, e.svc.Pools().Create, e.svc.Pools().Get, testProject, "child", childPoolSpec(idOf(root), "/25"))

	shrink := UpdateCmd[*vpcpb.PoolSpec]{
		Metadata: &commonpb.ResourceMetadata{Id: idOf(root), Name: "root"},
		Spec: &vpcpb.PoolSpec{
			Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
			Cidrs: []*vpcpb.PoolCidr{{Cidr: "10.9.5.0/24"}},
		},
		ResetMask: mask.ParseMust("spec.cidrs"),
	}
	_, err := tryUpdate(t.Context(), e.svc.Pools().Update, e.svc.Pools().Get, shrink)
	wantKind(t, "dropping a block with a child", err, errs.KindBadState)

	if err := tryDelete(t.Context(), e.svc.Pools().Delete, idOf(root)); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("delete pool with a child err = %v, want BadState", err)
	}
	if err := tryDelete(t.Context(), e.svc.Pools().Delete, idOf(child)); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	if _, err := tryUpdate(t.Context(), e.svc.Pools().Update, e.svc.Pools().Get, shrink); err != nil {
		t.Errorf("dropping an unused block: %v", err)
	}
}

// --- routes and security ---

func TestRouteValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rt := e.createRouteTable(t, "rt", idOf(e.def.Network))
	sn := e.createSubnet(t, "sn", "/24")
	alloc := e.createAlloc(t, "alloc", idOf(sn), "/32")
	otherNet := e.createNetwork(t, "other")
	otherSn := mustCreate(t, e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, "osn", subnetSpec(idOf(otherNet), "/24"))
	foreign := e.createAlloc(t, "foreign", idOf(otherSn), "/32")

	hop := func(id string) *vpcpb.NextHop {
		return &vpcpb.NextHop{NextHop: &vpcpb.NextHop_Allocation{Allocation: &vpcpb.AllocationNextHop{Id: id}}}
	}
	egress := &vpcpb.NextHop{NextHop: &vpcpb.NextHop_DefaultEgressGateway{DefaultEgressGateway: true}}
	tests := []struct {
		name string
		spec *vpcpb.RouteSpec
		want errs.Kind
	}{
		{"allocation hop", &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "10.50.0.0/16"}, NextHop: hop(idOf(alloc))}, errs.KindUnknown},
		{"duplicate destination", &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "10.50.0.0/16"}, NextHop: hop(idOf(alloc))}, errs.KindAlreadyExists},
		{"default route via egress", &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "0.0.0.0/0"}, NextHop: egress}, errs.KindUnknown},
		{"egress for a specific destination", &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "10.51.0.0/16"}, NextHop: egress}, errs.KindBadRequest},
		{"no next hop", &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "10.52.0.0/16"}}, errs.KindBadRequest},
		{"bad destination", &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "10.52.0.1/16"}, NextHop: hop(idOf(alloc))}, errs.KindBadRequest},
		{"unknown allocation", &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "10.53.0.0/16"}, NextHop: hop("vpcallocation-e00missing")}, errs.KindNotFound},
		{"allocation of another network", &vpcpb.RouteSpec{Destination: &vpcpb.DestinationMatch{Cidr: "10.54.0.0/16"}, NextHop: hop(idOf(foreign))}, errs.KindBadRequest},
	}
	for i, tt := range tests {
		_, err := tryCreate(t.Context(), e.svc.Routes().Create, e.svc.Routes().Get, idOf(rt), "r"+string(rune('a'+i)), tt.spec)
		wantKind(t, tt.name, err, tt.want)
	}
}

func TestRouteTableAssignmentTracksSubnets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rt := e.createRouteTable(t, "rt", idOf(e.def.Network))
	spec := subnetSpec(idOf(e.def.Network))
	spec.RouteTableId = idOf(rt)
	sn := mustCreate(t, e.svc.Subnets().Create, e.svc.Subnets().Get, testProject, "with-rt", spec)
	got, err := e.svc.RouteTables().Get(t.Context(), idOf(rt))
	if err != nil {
		t.Fatal(err)
	}
	if subs := got.GetStatus().GetAssignment().GetSubnets(); !slices.Equal(subs, []string{idOf(sn)}) {
		t.Errorf("route table assignment = %v, want [%s]", subs, idOf(sn))
	}
	if sn.GetStatus().GetRouteTable().GetId() != idOf(rt) || sn.GetStatus().GetRouteTable().GetDefault() {
		t.Errorf("subnet route table = %v, want non-default %s", sn.GetStatus().GetRouteTable(), idOf(rt))
	}
}

func TestSecurityRuleDefaultsAndStatus(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sgs, err := e.svc.SecurityGroups().ListByNetwork(t.Context(), idOf(e.def.Network), 0, "")
	if err != nil || len(sgs.Items) != 1 {
		t.Fatalf("security groups = %v, %v", sgs.Items, err)
	}
	sgID := idOf(sgs.Items[0])
	create := func(name string, spec *vpcpb.SecurityRuleSpec) (*vpcpb.SecurityRule, error) {
		return tryCreate(t.Context(), e.svc.SecurityRules().Create, e.svc.SecurityRules().Get, sgID, name, spec)
	}

	allow, err := create("allow", &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP,
		Match: &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceCidrs: []string{"10.0.0.0/8"}, DestinationPorts: []int32{22, 443}}},
	})
	if err != nil {
		t.Fatalf("create allow: %v", err)
	}
	if allow.GetSpec().GetPriority() != DefaultRulePriority || allow.GetSpec().GetType() != vpcpb.RuleType_STATEFUL {
		t.Errorf("defaults = priority %d type %v, want %d STATEFUL", allow.GetSpec().GetPriority(), allow.GetSpec().GetType(), DefaultRulePriority)
	}
	st := allow.GetStatus()
	if st.GetState() != vpcpb.SecurityRuleStatus_READY || st.GetDirection() != vpcpb.RuleDirection_INGRESS {
		t.Errorf("status = %v, want READY INGRESS", st)
	}
	if st.GetEffectivePriority() != 2*DefaultRulePriority+1 {
		t.Errorf("effective priority of ALLOW = %d, want %d", st.GetEffectivePriority(), 2*DefaultRulePriority+1)
	}
	if st.GetDestination().GetSecurityGroupId() != sgID || !slices.Equal(st.GetDestination().GetPorts(), []int32{22, 443}) || !slices.Equal(st.GetSource().GetCidrs(), []string{"10.0.0.0/8"}) {
		t.Errorf("match status = src %v dst %v", st.GetSource(), st.GetDestination())
	}

	deny, err := create("deny", &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_DENY, Protocol: vpcpb.RuleProtocol_ANY,
		Match: &vpcpb.SecurityRuleSpec_Egress{Egress: &vpcpb.RuleEgress{DestinationCidrs: []string{"192.0.2.0/24"}}},
	})
	if err != nil {
		t.Fatalf("create deny: %v", err)
	}
	if got := deny.GetStatus().GetEffectivePriority(); got != 2*DefaultRulePriority {
		t.Errorf("effective priority of DENY = %d, want %d (before ALLOW at equal priority)", got, 2*DefaultRulePriority)
	}
	if deny.GetStatus().GetDirection() != vpcpb.RuleDirection_EGRESS || deny.GetStatus().GetSource().GetSecurityGroupId() != sgID {
		t.Errorf("egress status = %v", deny.GetStatus())
	}
}

func TestSecurityRuleValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sgs, err := e.svc.SecurityGroups().ListByNetwork(t.Context(), idOf(e.def.Network), 0, "")
	if err != nil || len(sgs.Items) != 1 {
		t.Fatalf("security groups = %v, %v", sgs.Items, err)
	}
	sgID := idOf(sgs.Items[0])
	otherNet := e.createNetwork(t, "other")
	foreignSG := e.createSecurityGroup(t, "foreign", idOf(otherNet))

	ingress := func(in *vpcpb.RuleIngress) *vpcpb.SecurityRuleSpec_Ingress {
		return &vpcpb.SecurityRuleSpec_Ingress{Ingress: in}
	}
	tests := []struct {
		name string
		spec *vpcpb.SecurityRuleSpec
		want errs.Kind
	}{
		{"no access", &vpcpb.SecurityRuleSpec{Protocol: vpcpb.RuleProtocol_TCP, Match: ingress(&vpcpb.RuleIngress{})}, errs.KindBadRequest},
		{"no protocol", &vpcpb.SecurityRuleSpec{Access: vpcpb.RuleAccessAction_ALLOW, Match: ingress(&vpcpb.RuleIngress{})}, errs.KindBadRequest},
		{"no match", &vpcpb.SecurityRuleSpec{Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP}, errs.KindBadRequest},
		{"bad cidr", &vpcpb.SecurityRuleSpec{Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP, Match: ingress(&vpcpb.RuleIngress{SourceCidrs: []string{"10.0.0.1/8"}})}, errs.KindBadRequest},
		{"icmp with ports", &vpcpb.SecurityRuleSpec{Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_ICMP, Match: ingress(&vpcpb.RuleIngress{DestinationPorts: []int32{80}})}, errs.KindBadRequest},
		{"icmp without ports", &vpcpb.SecurityRuleSpec{Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_ICMP, Match: ingress(&vpcpb.RuleIngress{})}, errs.KindUnknown},
		{"unknown source group", &vpcpb.SecurityRuleSpec{Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP, Match: ingress(&vpcpb.RuleIngress{SourceSecurityGroupId: "vpcsecuritygroup-e00missing"})}, errs.KindNotFound},
		{"source group of another network", &vpcpb.SecurityRuleSpec{Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_TCP, Match: ingress(&vpcpb.RuleIngress{SourceSecurityGroupId: idOf(foreignSG)})}, errs.KindBadRequest},
	}
	for i, tt := range tests {
		_, err := tryCreate(t.Context(), e.svc.SecurityRules().Create, e.svc.SecurityRules().Get, sgID, "r"+string(rune('a'+i)), tt.spec)
		wantKind(t, tt.name, err, tt.want)
	}
	_, err = tryCreate(t.Context(), e.svc.SecurityRules().Create, e.svc.SecurityRules().Get, "vpcsecuritygroup-e00missing", "x", &vpcpb.SecurityRuleSpec{})
	wantKind(t, "unknown security group", err, errs.KindNotFound)
}

func TestSecurityGroupDeleteCascadesToRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	n := e.createNetwork(t, "n")
	sg := e.createSecurityGroup(t, "sg", idOf(n))
	rule := mustCreate(t, e.svc.SecurityRules().Create, e.svc.SecurityRules().Get, idOf(sg), "r", &vpcpb.SecurityRuleSpec{
		Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_ANY,
		Match: &vpcpb.SecurityRuleSpec_Egress{Egress: &vpcpb.RuleEgress{}},
	})
	if err := tryDelete(t.Context(), e.svc.SecurityGroups().Delete, idOf(sg)); err != nil {
		t.Fatalf("delete security group: %v", err)
	}
	if _, err := e.svc.SecurityRules().Get(t.Context(), idOf(rule)); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("rule after group delete err = %v, want NotFound", err)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	t.Parallel()
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New(Deps{}) = nil error, want an error")
	}
}
