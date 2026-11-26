package api_test

import (
	"testing"

	"google.golang.org/grpc"

	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/dns/api"
)

func newRules() *authz.Rules {
	r := authz.NewRules()
	api.RegisterAuthzRules(r)
	return r
}

func TestEveryRPCHasARule(t *testing.T) {
	t.Parallel()
	rules := newRules()
	for _, d := range []grpc.ServiceDesc{dnspb.ZoneService_ServiceDesc, dnspb.RecordService_ServiceDesc} {
		if len(d.Methods) == 0 {
			t.Fatalf("%s has no methods", d.ServiceName)
		}
		for _, m := range d.Methods {
			full := "/" + d.ServiceName + "/" + m.MethodName
			if _, ok := rules.Lookup(full); !ok {
				t.Errorf("no authz rule for %s", full)
			}
		}
	}
}

func TestPermissions(t *testing.T) {
	t.Parallel()
	rules := newRules()
	tests := []struct{ method, perm string }{
		{"/nebius.dns.v1.ZoneService/Create", "dns.zones.create"},
		{"/nebius.dns.v1.ZoneService/ListByNetwork", "dns.zones.list"},
		{"/nebius.dns.v1.RecordService/Get", "dns.records.get"},
		{"/nebius.dns.v1.RecordService/Delete", "dns.records.delete"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup(tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			if got := string(rule.Permission); got != tt.perm {
				t.Errorf("permission = %q, want %q", got, tt.perm)
			}
		})
	}
}

func TestListByNetworkTargetsTheNetwork(t *testing.T) {
	t.Parallel()
	rule, _ := newRules().Lookup("/nebius.dns.v1.ZoneService/ListByNetwork")
	got, err := rule.Target(t.Context(), &dnspb.ListZonesByNetworkRequest{NetworkId: "vpcnetwork-1"})
	if err != nil || got != "vpcnetwork-1" {
		t.Fatalf("Target = %q, %v; want vpcnetwork-1, nil", got, err)
	}
	if _, err := rule.Target(t.Context(), &dnspb.ListZonesByNetworkRequest{}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("empty network_id error = %v, want bad_request", err)
	}
}

func TestAuditorReadsZonesNotRecords(t *testing.T) {
	t.Parallel()
	c := authz.DefaultCatalog()
	if !c.Allows(authz.RoleAuditor, "dns.zones.get") || !c.Allows(authz.RoleAuditor, "dns.zones.list") {
		t.Error("auditor must read zones")
	}
	if c.Allows(authz.RoleAuditor, "dns.records.get") || c.Allows(authz.RoleAuditor, "dns.records.list") {
		t.Error("auditor must not read records")
	}
	if !c.Allows(authz.RoleViewer, "dns.records.get") {
		t.Error("viewer must read records")
	}
}
