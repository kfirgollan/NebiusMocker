package api_test

import (
	"testing"

	"google.golang.org/grpc"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/services/billing/api"
)

func newRules() *authz.Rules {
	r := authz.NewRules()
	api.RegisterAuthzRules(r)
	return r
}

func TestEveryPricingPolicyRPCHasARule(t *testing.T) {
	t.Parallel()
	rules := newRules()
	d := billingpb.PricingPolicyService_ServiceDesc
	if len(d.Methods) != 6 {
		t.Fatalf("%s has %d methods, want 6", d.ServiceName, len(d.Methods))
	}
	for _, m := range grpcMethods(d) {
		full := "/" + d.ServiceName + "/" + m
		if _, ok := rules.Lookup(full); !ok {
			t.Errorf("no authz rule for %s", full)
		}
	}
}

func grpcMethods(d grpc.ServiceDesc) []string {
	var out []string
	for _, m := range d.Methods {
		out = append(out, m.MethodName)
	}
	return out
}

func TestPricingPolicyPermissions(t *testing.T) {
	t.Parallel()
	rules := newRules()
	tests := []struct{ method, perm string }{
		{"Create", "billing.pricingPolicies.create"},
		{"Get", "billing.pricingPolicies.get"},
		{"GetByName", "billing.pricingPolicies.get"},
		{"List", "billing.pricingPolicies.list"},
		{"Update", "billing.pricingPolicies.update"},
		{"Delete", "billing.pricingPolicies.delete"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup("/nebius.billing.v1.PricingPolicyService/" + tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			if got := string(rule.Permission); got != tt.perm {
				t.Errorf("permission = %q, want %q", got, tt.perm)
			}
		})
	}
}
