//go:build e2e

package vpc

import (
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// TestReplaceOnImmutableChange changes one immutable attribute at a time and
// checks Terraform plans a replacement (with the dependents that follow), the
// resource gets a new ID, the old one is NotFound in the mock, and no drift
// remains.
func TestReplaceOnImmutableChange(t *testing.T) {
	inst, c, tf := startVPC(t)
	base := coreStack(t, inst.ProjectID, rootPoolID(t, c, inst))
	// The route table is created without a route so changing its network does
	// not force a route into a network its next hop is not part of.
	noRoute := []string{"pool", "net", "net2", "sub", "sub2", "alloc", "rt", "sg", "rule"}

	tf.writeConfig(t, base.render(t, corePieces...))
	tf.init(t)
	tf.expectChanges(t, nil, createAll(coreAll))
	tf.apply(t)
	tf.expectNoChanges(t)

	cases := []struct {
		name    string
		kv      []string
		pieces  []string
		changed map[string]string
		check   func(t *testing.T, st map[string]stateRes)
	}{
		{
			name: "allocation cidr", kv: []string{"alloc_cidr", "/31"}, pieces: corePieces,
			changed: map[string]string{"alloc": "replace", "route": "replace"},
			check: func(t *testing.T, st map[string]stateRes) {
				t.Helper()
				a, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: st["alloc"].ID})
				if err != nil {
					t.Fatalf("Get allocation: %v", err)
				}
				if got := a.GetSpec().GetIpv4Private().GetCidr(); got != "/31" {
					t.Errorf("allocation spec cidr = %q, want /31", got)
				}
			},
		},
		{
			name: "route destination", kv: []string{"route_dest", "172.16.0.0/12"}, pieces: corePieces,
			changed: map[string]string{"route": "replace"},
			check: func(t *testing.T, st map[string]stateRes) {
				t.Helper()
				r, err := c.routes.Get(t.Context(), &vpcpb.GetRouteRequest{Id: st["route"].ID})
				if err != nil {
					t.Fatalf("Get route: %v", err)
				}
				if got := r.GetSpec().GetDestination().GetCidr(); got != "172.16.0.0/12" {
					t.Errorf("route destination = %q, want 172.16.0.0/12", got)
				}
			},
		},
		{
			name: "security rule priority", kv: []string{"rule_priority", "200"}, pieces: corePieces,
			changed: map[string]string{"rule": "replace"},
			check:   checkRule(c, func(r *vpcpb.SecurityRule) bool { return r.GetSpec().GetPriority() == 200 }),
		},
		{
			name: "security rule protocol", kv: []string{"rule_protocol", "UDP"}, pieces: corePieces,
			changed: map[string]string{"rule": "replace"},
			check:   checkRule(c, func(r *vpcpb.SecurityRule) bool { return r.GetSpec().GetProtocol() == vpcpb.RuleProtocol_UDP }),
		},
		{
			name: "security rule ingress", kv: []string{"rule_ingress", `source_cidrs = ["10.0.0.0/8"], destination_ports = [8443]`}, pieces: corePieces,
			changed: map[string]string{"rule": "replace"},
			check: checkRule(c, func(r *vpcpb.SecurityRule) bool {
				return r.GetSpec().GetIngress().GetSourceCidrs()[0] == "10.0.0.0/8" && r.GetSpec().GetIngress().GetDestinationPorts()[0] == 8443
			}),
		},
		{
			name: "security rule ingress ports dropped", kv: []string{"rule_ingress", `source_cidrs = ["10.0.0.0/8"]`}, pieces: corePieces,
			changed: map[string]string{"rule": "replace"},
			check: checkRule(c, func(r *vpcpb.SecurityRule) bool {
				return r.GetSpec().GetIngress().GetSourceCidrs()[0] == "10.0.0.0/8" && len(r.GetSpec().GetIngress().GetDestinationPorts()) == 0
			}),
		},
		{
			name: "security group network", kv: []string{"sg_net", "net2"}, pieces: corePieces,
			changed: map[string]string{"sg": "replace", "rule": "replace"},
			check: func(t *testing.T, st map[string]stateRes) {
				t.Helper()
				g, err := c.sgs.Get(t.Context(), &vpcpb.GetSecurityGroupRequest{Id: st["sg"].ID})
				if err != nil {
					t.Fatalf("Get security group: %v", err)
				}
				if got := g.GetSpec().GetNetworkId(); got != st["net2"].ID {
					t.Errorf("security group network = %q, want %s", got, st["net2"].ID)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tf.state(t)
			base = base.with(tc.kv...)
			tf.writeConfig(t, base.render(t, tc.pieces...))
			tf.expectChanges(t, coreAll, tc.changed)
			tf.apply(t)
			after := tf.state(t)
			for name := range tc.changed {
				if after[name].ID == before[name].ID {
					t.Errorf("%s kept ID %s, want a replacement", name, before[name].ID)
				}
				c.gone(t, before[name].Kind, before[name].ID)
			}
			c.matchState(t, after)
			tc.check(t, after)
			tf.expectNoChanges(t)
		})
	}

	// The provider does not mark the allocation's subnet_id as RequiresReplace,
	// so it plans an in-place update, which the API rejects: the spec is immutable.
	t.Run("allocation subnet is immutable", func(t *testing.T) {
		before := tf.state(t)
		bv := c.version(t, "allocation", before["alloc"].ID)
		tf.writeConfig(t, base.with("alloc_subnet", "sub2").render(t, corePieces...))
		tf.expectChanges(t, coreAll, map[string]string{"alloc": "update"})
		if err := tf.tf.Apply(t.Context(), tfexec.DirOrPlan(planFile)); err == nil {
			t.Fatal("terraform apply succeeded, want the mock to reject changing the allocation's subnet")
		}
		a, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: before["alloc"].ID})
		if err != nil {
			t.Fatalf("Get allocation: %v", err)
		}
		if a.GetSpec().GetIpv4Private().GetSubnetId() != before["sub"].ID || a.GetMetadata().GetResourceVersion() != bv {
			t.Errorf("allocation changed by a rejected update: %v", a.GetSpec())
		}
		tf.writeConfig(t, base.render(t, corePieces...))
		tf.expectNoChanges(t)
	})

	// The route table moves networks in a stack without routes.
	t.Run("route table network", func(t *testing.T) {
		// First drop the route (an in-place stack change: only a delete).
		tf.writeConfig(t, base.render(t, noRoute...))
		tf.expectChanges(t, coreAll, map[string]string{"route": "delete"})
		tf.apply(t)
		before := tf.state(t)
		base = base.with("rt_net", "net2")
		tf.writeConfig(t, base.render(t, noRoute...))
		tf.expectChanges(t, []string{"pool", "net", "net2", "sub", "sub2", "alloc", "sg", "rule"}, map[string]string{"rt": "replace"})
		tf.apply(t)
		after := tf.state(t)
		if after["rt"].ID == before["rt"].ID {
			t.Errorf("route table kept ID %s, want a replacement", before["rt"].ID)
		}
		c.gone(t, "route_table", before["rt"].ID)
		rt, err := c.rts.Get(t.Context(), &vpcpb.GetRouteTableRequest{Id: after["rt"].ID})
		if err != nil {
			t.Fatalf("Get route table: %v", err)
		}
		if got := rt.GetSpec().GetNetworkId(); got != after["net2"].ID {
			t.Errorf("route table network = %q, want %s", got, after["net2"].ID)
		}
		tf.expectNoChanges(t)
	})
}

// TestReplacePool changes the source pool and visibility of a pool.
func TestReplacePool(t *testing.T) {
	inst, c, tf := startVPC(t)
	base := coreStack(t, inst.ProjectID, rootPoolID(t, c, inst))
	var public string
	pub, err := c.pools.GetByName(t.Context(), &vpcpb.GetPoolByNameRequest{ParentId: inst.ProjectID, Name: "default-public-pool"})
	if err != nil {
		t.Fatalf("GetByName default-public-pool: %v", err)
	}
	public = pub.GetMetadata().GetId()
	tf.writeConfig(t, base.render(t, "pool"))
	tf.init(t)
	tf.expectChanges(t, nil, map[string]string{"pool": "create"})
	tf.apply(t)
	before := tf.state(t)
	base = base.with("pool_source", public, "pool_visibility", "PUBLIC", "pool_cidrs", `{ cidr = "/28" }`)
	tf.writeConfig(t, base.render(t, "pool"))
	tf.expectChanges(t, nil, map[string]string{"pool": "replace"})
	tf.apply(t)
	after := tf.state(t)
	if after["pool"].ID == before["pool"].ID {
		t.Errorf("pool kept ID %s, want a replacement", before["pool"].ID)
	}
	c.gone(t, "pool", before["pool"].ID)
	p, err := c.pools.Get(t.Context(), &vpcpb.GetPoolRequest{Id: after["pool"].ID})
	if err != nil {
		t.Fatalf("Get pool: %v", err)
	}
	if p.GetSpec().GetSourcePoolId() != public || p.GetSpec().GetVisibility() != vpcpb.IpVisibility_PUBLIC {
		t.Errorf("pool spec = %v, want source %s and PUBLIC", p.GetSpec(), public)
	}
	tf.expectNoChanges(t)
}

func checkRule(c *clients, ok func(*vpcpb.SecurityRule) bool) func(*testing.T, map[string]stateRes) {
	return func(t *testing.T, st map[string]stateRes) {
		t.Helper()
		r, err := c.rules.Get(t.Context(), &vpcpb.GetSecurityRuleRequest{Id: st["rule"].ID})
		if err != nil {
			t.Fatalf("Get security rule: %v", err)
		}
		if !ok(r) {
			t.Errorf("security rule spec = %v does not reflect the config", r.GetSpec())
		}
	}
}
