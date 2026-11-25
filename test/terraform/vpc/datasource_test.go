//go:build e2e

package vpc

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// dsKind describes one resource kind's data sources.
type dsKind struct {
	kind   string // data source suffix: nebius_vpc_v1_<kind>
	local  string // the resource's Terraform name in the stack
	name   string // its name in the mock
	parent string // HCL expression for parent_id
	extra  string // HCL expression (relative to the data source) exported as <kind>_extra
}

// dataSourceHCL renders by-id and by-name data sources and outputs for every kind.
func dataSourceHCL(project string, kinds []dsKind) string {
	var b strings.Builder
	for _, k := range kinds {
		res := fmt.Sprintf("nebius_vpc_v1_%s.%s", k.kind, k.local)
		fmt.Fprintf(&b, `
data "nebius_vpc_v1_%[1]s" "%[2]s_by_id" {
  id = %[3]s.id
}

data "nebius_vpc_v1_%[1]s" "%[2]s_by_name" {
  parent_id = %[4]s
  name      = %[5]q
  depends_on = [%[3]s]
}

output "%[2]s_by_id"   { value = data.nebius_vpc_v1_%[1]s.%[2]s_by_id.id }
output "%[2]s_by_name" { value = data.nebius_vpc_v1_%[1]s.%[2]s_by_name.id }
output "%[2]s_extra"   { value = data.nebius_vpc_v1_%[1]s.%[2]s_by_id.%[6]s }
`, k.kind, k.local, res, k.parent, k.name, k.extra)
	}
	fmt.Fprintf(&b, `
data "nebius_vpc_v1_default_network" "d" {
  parent_id = %[1]q
}

data "nebius_vpc_v1_pool" "default_private" {
  parent_id = %[1]q
  name      = "default-network-pool"
}

output "default_network_id"        { value = data.nebius_vpc_v1_default_network.d.network.metadata.id }
output "default_network_name"      { value = data.nebius_vpc_v1_default_network.d.network.metadata.name }
output "default_network_is_default" { value = data.nebius_vpc_v1_default_network.d.network.status.default }
output "default_subnet_id"         { value = data.nebius_vpc_v1_default_network.d.subnet.metadata.id }
output "default_pool_id"           { value = data.nebius_vpc_v1_pool.default_private.id }
output "default_pool_cidrs"        { value = data.nebius_vpc_v1_pool.default_private.status.cidrs }
`, project)
	return b.String()
}

// TestDataSources reads every VPC data source by ID and by name and compares
// what Terraform sees with the mock's own answers.
func TestDataSources(t *testing.T) {
	inst, c, tf := startVPC(t)
	s := coreStack(t, inst.ProjectID, rootPoolID(t, c, inst))
	p := inst.ProjectID
	kinds := []dsKind{
		{"pool", "pool", "e2e-pool", `"` + p + `"`, "status.cidrs"},
		{"network", "net", "e2e-net", `"` + p + `"`, "status.default_route_table_id"},
		{"subnet", "sub", "e2e-sub", `"` + p + `"`, "network_id"},
		{"allocation", "alloc", "e2e-alloc", `"` + p + `"`, "status.details.allocated_cidr"},
		{"route_table", "rt", "e2e-rt", `"` + p + `"`, "network_id"},
		{"route", "route", "e2e-route", "nebius_vpc_v1_route_table.rt.id", "destination.cidr"},
		{"security_group", "sg", "e2e-sg", `"` + p + `"`, "network_id"},
		{"security_rule", "rule", "e2e-rule", "nebius_vpc_v1_security_group.sg.id", "protocol"},
	}
	tf.writeConfig(t, s.render(t, corePieces...)+dataSourceHCL(p, kinds))
	tf.init(t)
	// The data sources depend on resources that do not exist yet: their reads
	// are deferred to apply and show in the plan as "read".
	first := createAll(coreAll)
	for _, k := range kinds {
		first[k.local+"_by_id"] = "read"
		first[k.local+"_by_name"] = "read"
	}
	tf.expectChanges(t, nil, first)
	tf.apply(t)
	st := tf.state(t)
	c.matchState(t, st)
	tf.expectNoChanges(t)

	out := tf.outputs(t)
	str := func(key string) string {
		t.Helper()
		v, ok := out[key].(string)
		if !ok {
			t.Fatalf("output %s = %v (%T), want a string", key, out[key], out[key])
		}
		return v
	}
	for _, k := range kinds {
		want := st[k.local].ID
		if got := str(k.local + "_by_id"); got != want {
			t.Errorf("%s by id = %s, want %s", k.kind, got, want)
		}
		if got := str(k.local + "_by_name"); got != want {
			t.Errorf("%s by name = %s, want %s", k.kind, got, want)
		}
	}

	ctx := t.Context()
	net, err := c.nets.Get(ctx, &vpcpb.GetNetworkRequest{Id: st["net"].ID})
	if err != nil {
		t.Fatalf("Get network: %v", err)
	}
	pool, err := c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: st["pool"].ID})
	if err != nil {
		t.Fatalf("Get pool: %v", err)
	}
	alloc, err := c.allocs.Get(ctx, &vpcpb.GetAllocationRequest{Id: st["alloc"].ID})
	if err != nil {
		t.Fatalf("Get allocation: %v", err)
	}
	extras := map[string]any{
		"net_extra":   net.GetStatus().GetDefaultRouteTableId(),
		"sub_extra":   st["net"].ID,
		"alloc_extra": alloc.GetStatus().GetDetails().GetAllocatedCidr(),
		"rt_extra":    st["net"].ID,
		"route_extra": "192.168.0.0/16",
		"sg_extra":    st["net"].ID,
		"rule_extra":  "TCP",
	}
	for key, want := range extras {
		if out[key] != want {
			t.Errorf("output %s = %v, want %v", key, out[key], want)
		}
	}
	if got := toStrings(t, out["pool_extra"]); !slices.Equal(got, pool.GetStatus().GetCidrs()) {
		t.Errorf("pool data source cidrs = %v, want %v", got, pool.GetStatus().GetCidrs())
	}

	defNet, err := c.nets.GetByName(ctx, &vpcpb.GetNetworkByNameRequest{ParentId: p, Name: "default-network"})
	if err != nil {
		t.Fatalf("GetByName default-network: %v", err)
	}
	defSub, err := c.subs.GetByName(ctx, &vpcpb.GetSubnetByNameRequest{ParentId: p, Name: "default-subnet"})
	if err != nil {
		t.Fatalf("GetByName default-subnet: %v", err)
	}
	defPool, err := c.pools.GetByName(ctx, &vpcpb.GetPoolByNameRequest{ParentId: p, Name: "default-network-pool"})
	if err != nil {
		t.Fatalf("GetByName default-network-pool: %v", err)
	}
	if got := str("default_network_id"); got != defNet.GetMetadata().GetId() {
		t.Errorf("default network id = %s, want %s", got, defNet.GetMetadata().GetId())
	}
	if got := str("default_network_name"); got != "default-network" {
		t.Errorf("default network name = %s", got)
	}
	if out["default_network_is_default"] != true || !defNet.GetStatus().GetDefault() {
		t.Errorf("default network status.default: terraform %v, mock %v, want true", out["default_network_is_default"], defNet.GetStatus().GetDefault())
	}
	if got := str("default_subnet_id"); got != defSub.GetMetadata().GetId() {
		t.Errorf("default subnet id = %s, want %s", got, defSub.GetMetadata().GetId())
	}
	if got := str("default_pool_id"); got != defPool.GetMetadata().GetId() {
		t.Errorf("default pool id = %s, want %s", got, defPool.GetMetadata().GetId())
	}
	if got := toStrings(t, out["default_pool_cidrs"]); !slices.Equal(got, defPool.GetStatus().GetCidrs()) || len(got) == 0 {
		t.Errorf("default pool cidrs = %v, want %v", got, defPool.GetStatus().GetCidrs())
	}

	tf.destroy(t)
	for _, r := range st {
		c.gone(t, r.Kind, r.ID)
	}
}

func toStrings(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("value %v (%T), want a list", v, v)
	}
	out := make([]string, len(list))
	for i, x := range list {
		out[i], _ = x.(string)
	}
	return out
}
