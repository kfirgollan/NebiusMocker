package api_test

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/api"
)

func newRules() *authz.Rules {
	r := authz.NewRules()
	api.RegisterAuthzRules(r)
	return r
}

func TestEveryRPCHasARule(t *testing.T) {
	t.Parallel()
	rules := newRules()
	d := tunnelpb.TunnelService_ServiceDesc
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

func TestPermissionsAndTargets(t *testing.T) {
	t.Parallel()
	rules := newRules()
	const svc = "/nebius.tunnel.v1.TunnelService/"
	tests := []struct {
		method, perm string
		req          any
		target       string
	}{
		{"Get", "applicationtunnel.tunnels.get", &tunnelpb.GetTunnelRequest{Id: "applicationtunnel-1"}, "applicationtunnel-1"},
		{"List", "applicationtunnel.tunnels.list", &tunnelpb.ListTunnelRequest{ParentId: "project-1"}, "project-1"},
		{"Create", "applicationtunnel.tunnels.create", &tunnelpb.CreateTunnelRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "project-1"}}, "project-1"},
		{"Update", "applicationtunnel.tunnels.update", &tunnelpb.UpdateTunnelRequest{Metadata: &commonpb.ResourceMetadata{Id: "applicationtunnel-1"}}, "applicationtunnel-1"},
		{"Delete", "applicationtunnel.tunnels.delete", &tunnelpb.DeleteTunnelRequest{Id: "applicationtunnel-1"}, "applicationtunnel-1"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup(svc + tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			if got := string(rule.Permission); got != tt.perm {
				t.Errorf("permission = %q, want %q", got, tt.perm)
			}
			got, err := rule.Target(t.Context(), tt.req)
			if err != nil || got != tt.target {
				t.Errorf("Target = %q, %v; want %q, nil", got, err, tt.target)
			}
		})
	}
	rule, _ := rules.Lookup(svc + "Get")
	if _, err := rule.Target(t.Context(), &tunnelpb.GetTunnelRequest{}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("empty id error = %v, want bad_request", err)
	}
}

// The applicationtunnel.agent role reads tunnels and nothing else.
func TestTunnelAgentRole(t *testing.T) {
	t.Parallel()
	c := authz.DefaultCatalog()
	if !c.Has(authz.RoleTunnelAgent) {
		t.Fatalf("role %s missing from the default catalog", authz.RoleTunnelAgent)
	}
	for perm, want := range map[authz.Permission]bool{
		"applicationtunnel.tunnels.get":    true,
		"applicationtunnel.tunnels.list":   false,
		"applicationtunnel.tunnels.update": false,
		"applicationtunnel.tunnels.delete": false,
		"applicationtunnel.tunnels.create": false,
		"compute.instances.get":            false,
	} {
		if got := c.Allows(authz.RoleTunnelAgent, perm); got != want {
			t.Errorf("%s allows %s = %v, want %v", authz.RoleTunnelAgent, perm, got, want)
		}
	}
}
