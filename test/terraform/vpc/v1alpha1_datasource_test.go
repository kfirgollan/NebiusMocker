//go:build e2e

package vpc

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"
)

// alphaDS is one v1alpha1 data source kind: looked up by name from a root
// object and again by id from the by-name result.
type alphaDS struct {
	kind   string // data source suffix: nebius_vpc_v1alpha1_<kind>
	name   string // its name in the mock
	fields []string
	// dependsOn is an optional HCL line delaying the by-name read until apply.
	dependsOn string
}

// alphaDataHCL renders a by-name data source, a by-id data source fed by the
// by-name id, and one output per field for each of them.
func alphaDataHCL(project string, kinds []alphaDS) string {
	var b strings.Builder
	for _, k := range kinds {
		fmt.Fprintf(&b, `
data "nebius_vpc_v1alpha1_%[1]s" "%[1]s_by_name" {
  parent_id = %[2]q
  name      = %[3]q
  %[4]s
}

data "nebius_vpc_v1alpha1_%[1]s" "%[1]s_by_id" {
  id = data.nebius_vpc_v1alpha1_%[1]s.%[1]s_by_name.id
}

output "%[1]s_id_by_name" { value = data.nebius_vpc_v1alpha1_%[1]s.%[1]s_by_name.id }
output "%[1]s_id_by_id"   { value = data.nebius_vpc_v1alpha1_%[1]s.%[1]s_by_id.id }
`, k.kind, project, k.name, k.dependsOn)
		for _, f := range k.fields {
			o := strings.ReplaceAll(f, ".", "_")
			fmt.Fprintf(&b, `
output "%[1]s_%[3]s_by_name" { value = data.nebius_vpc_v1alpha1_%[1]s.%[1]s_by_name.%[2]s }
output "%[1]s_%[3]s_by_id"   { value = data.nebius_vpc_v1alpha1_%[1]s.%[1]s_by_id.%[2]s }
`, k.kind, f, o)
		}
	}
	return b.String()
}

// TestV1alpha1DataSources reads every v1alpha1 data source by name and by id and
// compares the outputs with the v1alpha1 SDK's own answers.
func TestV1alpha1DataSources(t *testing.T) {
	inst, _, tf := startVPC(t)
	a := newAlphaClients(t, inst)
	p := inst.ProjectID

	kinds := []alphaDS{
		{"network", "default-network", []string{"status.scope_id", "status.default"}, ""},
		{"subnet", "default-subnet", []string{"network_id", "status.default", "status.ipv4_cidrs"}, ""},
		{"pool", "default-network-pool", []string{"status.cidrs", "status.scope_id", "version"}, ""},
		{"scope", "default-private-scope", []string{"type", "status.state"}, ""},
		{"allocation", "alpha-ds-alloc", []string{"status.details.allocated_cidr", "status.state"}, "depends_on = [nebius_vpc_v1alpha1_allocation.ds]"},
	}
	tf.writeConfig(t, fmt.Sprintf(`
resource "nebius_vpc_v1alpha1_allocation" "ds" {
  parent_id    = %q
  name         = "alpha-ds-alloc"
  ipv4_private = { pool_id = data.nebius_vpc_v1alpha1_pool.pool_by_name.id }
}
`, p)+alphaDataHCL(p, kinds))
	tf.init(t)
	tf.expectChanges(t, nil, map[string]string{"ds": "create", "allocation_by_name": "read", "allocation_by_id": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	out := tf.outputs(t)

	ctx := t.Context()
	net, err := a.nets.GetByName(ctx, &vpcv1alpha1pb.GetNetworkByNameRequest{ParentId: p, Name: "default-network"})
	if err != nil {
		t.Fatalf("v1alpha1 GetByName network: %v", err)
	}
	sub, err := a.subs.GetByName(ctx, &vpcv1alpha1pb.GetSubnetByNameRequest{ParentId: p, Name: "default-subnet"})
	if err != nil {
		t.Fatalf("v1alpha1 GetByName subnet: %v", err)
	}
	pool, err := a.pools.GetByName(ctx, &vpcv1alpha1pb.GetPoolByNameRequest{ParentId: p, Name: "default-network-pool"})
	if err != nil {
		t.Fatalf("v1alpha1 GetByName pool: %v", err)
	}
	scope, err := a.scopes.GetByName(ctx, &vpcv1alpha1pb.GetScopeByNameRequest{ParentId: p, Name: "default-private-scope"})
	if err != nil {
		t.Fatalf("v1alpha1 GetByName scope: %v", err)
	}
	alloc, err := a.allocs.GetByName(ctx, &vpcv1alpha1pb.GetAllocationByNameRequest{ParentId: p, Name: "alpha-ds-alloc"})
	if err != nil {
		t.Fatalf("v1alpha1 GetByName allocation: %v", err)
	}

	// Cross-links between the objects themselves.
	if net.GetStatus().GetScopeId() != scope.GetMetadata().GetId() || pool.GetStatus().GetScopeId() != scope.GetMetadata().GetId() {
		t.Errorf("scope ids: network %q pool %q, want the default-private-scope %q", net.GetStatus().GetScopeId(), pool.GetStatus().GetScopeId(), scope.GetMetadata().GetId())
	}
	if sub.GetSpec().GetNetworkId() != net.GetMetadata().GetId() {
		t.Errorf("default subnet network_id = %q, want %q", sub.GetSpec().GetNetworkId(), net.GetMetadata().GetId())
	}
	if len(pool.GetStatus().GetCidrs()) == 0 || pool.GetStatus().GetCidrs()[0] != "10.0.0.0/13" {
		t.Errorf("default-network-pool status.cidrs = %v, want 10.0.0.0/13", pool.GetStatus().GetCidrs())
	}
	if alloc.GetStatus().GetDetails().GetPoolId() != pool.GetMetadata().GetId() {
		t.Errorf("allocation pool = %q, want %q", alloc.GetStatus().GetDetails().GetPoolId(), pool.GetMetadata().GetId())
	}

	list := func(s []string) any {
		r := make([]any, len(s))
		for i, x := range s {
			r[i] = x
		}
		return r
	}
	want := map[string]map[string]any{
		"network": {
			"id": net.GetMetadata().GetId(), "status_scope_id": net.GetStatus().GetScopeId(), "status_default": true,
		},
		"subnet": {
			"id": sub.GetMetadata().GetId(), "network_id": sub.GetSpec().GetNetworkId(), "status_default": true,
			"status_ipv4_cidrs": list(sub.GetStatus().GetIpv4Cidrs()),
		},
		"pool": {
			"id": pool.GetMetadata().GetId(), "status_cidrs": list(pool.GetStatus().GetCidrs()),
			"status_scope_id": pool.GetStatus().GetScopeId(), "version": "IPV4",
		},
		"scope": {
			"id": scope.GetMetadata().GetId(), "type": "PRIVATE", "status_state": "READY",
		},
		"allocation": {
			"id": alloc.GetMetadata().GetId(), "status_details_allocated_cidr": alloc.GetStatus().GetDetails().GetAllocatedCidr(),
			"status_state": "ALLOCATED",
		},
	}
	for _, k := range kinds {
		for field, w := range want[k.kind] {
			for _, via := range []string{"by_name", "by_id"} {
				key := fmt.Sprintf("%s_%s_%s", k.kind, field, via)
				if got := out[key]; !reflect.DeepEqual(got, w) {
					t.Errorf("output %s = %#v, want %#v", key, got, w)
				}
			}
		}
	}
}
