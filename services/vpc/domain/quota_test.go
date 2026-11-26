package domain

import (
	"context"
	"maps"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// fakeGate records the claims and runs apply, unless refuse is set.
type fakeGate struct {
	mu         sync.Mutex
	refuse     error // returned for claims that include refuseName
	refuseName string
	claims     []peer.QuotaClaim
}

func (g *fakeGate) Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.claims = append(g.claims, c)
	if g.refuse != nil && c.Amounts[g.refuseName] > 0 {
		return g.refuse
	}
	if apply == nil {
		return nil
	}
	return apply(ctx)
}

func (g *fakeGate) take() []peer.QuotaClaim {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.claims
	g.claims = nil
	return c
}

func (g *fakeGate) setRefuse(name string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse, g.refuseName = err, name
}

func usageMap(t *testing.T, e *env) map[string]uint64 {
	t.Helper()
	us, err := e.svc.QuotaUsage(t.Context())
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	out := map[string]uint64{}
	for _, u := range us {
		out[u.ContainerID+"/"+u.Name] = u.Amount
	}
	return out
}

type quotaCase struct {
	name   string
	create func(t *testing.T) error
	want   map[string]int64
}

// quotaCases create one resource of each charged kind, in the seeded project.
func quotaCases(e *env) []quotaCase {
	netID := idOf(e.def.Network)
	return []quotaCase{
		{"network", func(t *testing.T) error {
			t.Helper()
			_, err := tryCreate(t.Context(), e.svc.Networks().Create, e.svc.Networks().Get, testProject, "n", &vpcpb.NetworkSpec{})
			return err
		}, map[string]int64{quota.VPCNetworkCount: 1}},
		{"subnet", func(t *testing.T) error {
			t.Helper()
			_, err := e.trySubnet(t, "sn", netID, "10.0.1.0/24")
			return err
		}, map[string]int64{quota.VPCSubnetCount: 1}},
		{"pool", func(t *testing.T) error {
			t.Helper()
			_, err := tryCreate(t.Context(), e.svc.Pools().Create, e.svc.Pools().Get, testProject, "p", &vpcpb.PoolSpec{
				Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE,
				Cidrs: []*vpcpb.PoolCidr{{Cidr: "192.168.0.0/16"}},
			})
			return err
		}, map[string]int64{quota.VPCPoolCount: 1}},
		{"private allocation", func(t *testing.T) error {
			t.Helper()
			_, err := e.tryAlloc(t, "a", privateAllocSpec(idOf(e.def.Subnet), "/32"))
			return err
		}, map[string]int64{quota.VPCAllocationCount: 1}},
		{"public allocation", func(t *testing.T) error {
			t.Helper()
			_, err := e.tryAlloc(t, "ap", publicAllocSpec("/32"))
			return err
		}, map[string]int64{quota.VPCAllocationCount: 1, quota.VPCPublicIPv4Count: 1}},
		{"route table", func(t *testing.T) error {
			t.Helper()
			_, err := tryCreate(t.Context(), e.svc.RouteTables().Create, e.svc.RouteTables().Get, testProject, "rt", &vpcpb.RouteTableSpec{NetworkId: netID})
			return err
		}, map[string]int64{quota.VPCRouteTableCount: 1}},
		{"route", func(t *testing.T) error {
			t.Helper()
			rt := e.createRouteTable(t, "rt-for-route", netID)
			e.gate.take()
			_, err := tryCreate(t.Context(), e.svc.Routes().Create, e.svc.Routes().Get, idOf(rt), "r", &vpcpb.RouteSpec{
				Destination: &vpcpb.DestinationMatch{Cidr: "0.0.0.0/0"},
				NextHop:     &vpcpb.NextHop{NextHop: &vpcpb.NextHop_DefaultEgressGateway{DefaultEgressGateway: true}},
			})
			return err
		}, map[string]int64{quota.VPCRouteCount: 1}},
		{"security group", func(t *testing.T) error {
			t.Helper()
			_, err := tryCreate(t.Context(), e.svc.SecurityGroups().Create, e.svc.SecurityGroups().Get, testProject, "sg", &vpcpb.SecurityGroupSpec{NetworkId: netID})
			return err
		}, map[string]int64{quota.VPCSecurityGroupCount: 1}},
		{"security rule", func(t *testing.T) error {
			t.Helper()
			sgs, err := e.svc.SecurityGroups().ListByNetwork(t.Context(), netID, 0, "")
			if err != nil || len(sgs.Items) == 0 {
				t.Fatalf("default security group: %v", err)
			}
			e.gate.take()
			_, err = tryCreate(t.Context(), e.svc.SecurityRules().Create, e.svc.SecurityRules().Get, idOf(sgs.Items[0]), "rule", &vpcpb.SecurityRuleSpec{
				Access: vpcpb.RuleAccessAction_ALLOW, Protocol: vpcpb.RuleProtocol_ANY,
				Match: &vpcpb.SecurityRuleSpec_Ingress{Ingress: &vpcpb.RuleIngress{SourceCidrs: []string{"10.0.0.0/8"}}},
			})
			return err
		}, map[string]int64{quota.VPCSecurityRuleCount: 1}},
	}
}

// eachQuotaCase runs fn for every quota case, each in its own parallel subtest
// over a fresh env.
func eachQuotaCase(t *testing.T, fn func(t *testing.T, e *env, tc quotaCase)) {
	t.Helper()
	probe := quotaCases(newEnv(t))
	for i, c := range probe {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			fn(t, e, quotaCases(e)[i])
		})
	}
}

func TestCreateClaimsQuotaOfTheProject(t *testing.T) {
	t.Parallel()
	eachQuotaCase(t, func(t *testing.T, e *env, tc quotaCase) {
		t.Helper()
		e.gate.take()
		if err := tc.create(t); err != nil {
			t.Fatalf("create: %v", err)
		}
		claims := e.gate.take()
		if len(claims) != 1 || claims[0].ContainerID != testProject || claims[0].Region != "" {
			t.Fatalf("claims = %+v, want one claim on %s", claims, testProject)
		}
		if diff := cmp.Diff(tc.want, claims[0].Amounts); diff != "" {
			t.Errorf("amounts (-want +got):\n%s", diff)
		}
	})
}

func TestRefusedCreateStoresNothing(t *testing.T) {
	t.Parallel()
	eachQuotaCase(t, func(t *testing.T, e *env, tc quotaCase) {
		t.Helper()
		for name := range tc.want {
			e.gate.setRefuse(name, errs.QuotaExceeded("limit reached"))
		}
		before := usageMap(t, e)
		wantKind(t, "create", tc.create(t), errs.KindQuotaExceeded)
		after := usageMap(t, e)
		for name := range tc.want {
			key := testProject + "/" + name
			if after[key] != before[key] {
				t.Errorf("usage[%s] = %d after a refused create, want %d", name, after[key], before[key])
			}
		}
	})
}

func TestUsageCountsEveryStoredResource(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// The seeded default network, subnet, pools, route table, route and
	// security group are system-created; they count.
	base := usageMap(t, e)
	for name, want := range map[string]uint64{
		quota.VPCNetworkCount: 1, quota.VPCSubnetCount: 1, quota.VPCPoolCount: 2,
		quota.VPCRouteTableCount: 1, quota.VPCRouteCount: 1, quota.VPCSecurityGroupCount: 1,
	} {
		if got := base[testProject+"/"+name]; got != want {
			t.Errorf("seeded usage[%s] = %d, want %d", name, got, want)
		}
	}
	for _, tc := range quotaCases(e) {
		if err := tc.create(t); err != nil {
			t.Fatalf("create %s: %v", tc.name, err)
		}
	}
	// One of each user-created resource, plus what the new network and
	// security group bring along by themselves (default route table, route,
	// security group): counted, never admitted.
	want := maps.Clone(base)
	for name, d := range map[string]uint64{
		quota.VPCNetworkCount: 1, quota.VPCSubnetCount: 1, quota.VPCPoolCount: 1,
		quota.VPCAllocationCount: 2, quota.VPCPublicIPv4Count: 1,
		quota.VPCRouteTableCount: 3, quota.VPCRouteCount: 2,
		quota.VPCSecurityGroupCount: 2, quota.VPCSecurityRuleCount: 1,
	} {
		want[testProject+"/"+name] += d
	}
	if diff := cmp.Diff(want, usageMap(t, e)); diff != "" {
		t.Errorf("usage (-want +got):\n%s", diff)
	}
}

func TestNICEphemeralAddressesClaimQuota(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	e.gate.take()
	b := e.nicBinding(sn, "eth0")
	b.Public = &NICPublic{}
	e.bind(t, b)
	claims := e.gate.take()
	if len(claims) != 1 || claims[0].ContainerID != testProject {
		t.Fatalf("claims = %+v, want one on %s", claims, testProject)
	}
	want := map[string]int64{quota.VPCAllocationCount: 2, quota.VPCPublicIPv4Count: 1}
	if diff := cmp.Diff(want, claims[0].Amounts); diff != "" {
		t.Errorf("amounts (-want +got):\n%s", diff)
	}
	// Binding again changes nothing, so it claims nothing.
	e.bind(t, b)
	if claims := e.gate.take(); len(claims) != 0 {
		t.Errorf("idempotent bind claims = %+v, want none", claims)
	}
}

func TestNICRefusedChangesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	e.gate.setRefuse(quota.VPCAllocationCount, errs.QuotaExceeded("limit reached"))
	before := len(e.allocs(t))
	_, err := e.svc.NICs().BindInterface(t.Context(), e.nicBinding(sn, "eth0"))
	wantKind(t, "bind", err, errs.KindQuotaExceeded)
	if got := len(e.allocs(t)); got != before {
		t.Errorf("allocations = %d after a refused bind, want %d", got, before)
	}
}
