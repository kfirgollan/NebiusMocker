//go:build e2e

package vpc

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"text/template"

	tfjson "github.com/hashicorp/terraform-json"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// stateRes is one managed resource in the Terraform state.
type stateRes struct {
	Kind string // vpc resource kind, e.g. "route_table"
	ID   string
}

// state returns local name → managed resource from the Terraform state.
func (r *tfRun) state(t *testing.T) map[string]stateRes {
	t.Helper()
	st, err := r.tf.Show(t.Context())
	if err != nil {
		t.Fatalf("terraform show: %v", err)
	}
	out := map[string]stateRes{}
	if st.Values == nil || st.Values.RootModule == nil {
		return out
	}
	for _, res := range st.Values.RootModule.Resources {
		if res.Mode != tfjson.ManagedResourceMode {
			continue
		}
		id, _ := res.AttributeValues["id"].(string)
		out[res.Name] = stateRes{Kind: strings.TrimPrefix(res.Type, "nebius_vpc_v1_"), ID: id}
	}
	return out
}

// outputs returns the root module outputs decoded from JSON.
func (r *tfRun) outputs(t *testing.T) map[string]any {
	t.Helper()
	raw, err := r.tf.Output(t.Context())
	if err != nil {
		t.Fatalf("terraform output: %v", err)
	}
	out := map[string]any{}
	for k, v := range raw {
		var x any
		if err := json.Unmarshal(v.Value, &x); err != nil {
			t.Fatalf("decode output %s: %v", k, err)
		}
		out[k] = x
	}
	return out
}

// expectChanges asserts the plan: names in changed get the given action, every
// other name in all must be a no-op.
func (r *tfRun) expectChanges(t *testing.T, all []string, changed map[string]string) {
	t.Helper()
	want := map[string]string{}
	for _, n := range all {
		want[n] = "no-op"
	}
	maps.Copy(want, changed)
	r.expectPlan(t, want)
}

// stack renders HCL from named pieces, with data overriding the defaults.
type stack struct {
	project string
	data    map[string]string
}

func (s stack) render(t *testing.T, pieces ...string) string {
	t.Helper()
	d := map[string]string{"proj": s.project}
	maps.Copy(d, defaults)
	maps.Copy(d, s.data)
	var b strings.Builder
	for _, p := range pieces {
		src, ok := pieceHCL[p]
		if !ok {
			t.Fatalf("unknown piece %q", p)
		}
		tpl, err := template.New(p).Option("missingkey=error").Parse(src)
		if err != nil {
			t.Fatalf("piece %s: %v", p, err)
		}
		if err := tpl.Execute(&b, d); err != nil {
			t.Fatalf("piece %s: %v", p, err)
		}
	}
	return b.String()
}

func (s stack) with(kv ...string) stack {
	d := maps.Clone(s.data)
	if d == nil {
		d = map[string]string{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		d[kv[i]] = kv[i+1]
	}
	return stack{project: s.project, data: d}
}

// clients are the official Go SDK services used to check the mock's state.
type clients struct {
	pools  vpcsvc.PoolService
	nets   vpcsvc.NetworkService
	subs   vpcsvc.SubnetService
	allocs vpcsvc.AllocationService
	rts    vpcsvc.RouteTableService
	routes vpcsvc.RouteService
	sgs    vpcsvc.SecurityGroupService
	rules  vpcsvc.SecurityRuleService
}

func newClients(t *testing.T, inst *testkit.Instance) *clients {
	t.Helper()
	sdk := inst.SDK(t)
	return &clients{
		pools: vpcsvc.NewPoolService(sdk), nets: vpcsvc.NewNetworkService(sdk), subs: vpcsvc.NewSubnetService(sdk),
		allocs: vpcsvc.NewAllocationService(sdk), rts: vpcsvc.NewRouteTableService(sdk), routes: vpcsvc.NewRouteService(sdk),
		sgs: vpcsvc.NewSecurityGroupService(sdk), rules: vpcsvc.NewSecurityRuleService(sdk),
	}
}

// metaOf is implemented by every VPC resource message.
type metaOf interface {
	GetMetadata() *commonpb.ResourceMetadata
}

// get fetches a resource of the given kind from the mock.
func (c *clients) get(t *testing.T, kind, id string) (metaOf, error) {
	t.Helper()
	ctx := t.Context()
	switch kind {
	case "pool":
		return c.pools.Get(ctx, &vpcpb.GetPoolRequest{Id: id})
	case "network":
		return c.nets.Get(ctx, &vpcpb.GetNetworkRequest{Id: id})
	case "subnet":
		return c.subs.Get(ctx, &vpcpb.GetSubnetRequest{Id: id})
	case "allocation":
		return c.allocs.Get(ctx, &vpcpb.GetAllocationRequest{Id: id})
	case "route_table":
		return c.rts.Get(ctx, &vpcpb.GetRouteTableRequest{Id: id})
	case "route":
		return c.routes.Get(ctx, &vpcpb.GetRouteRequest{Id: id})
	case "security_group":
		return c.sgs.Get(ctx, &vpcpb.GetSecurityGroupRequest{Id: id})
	case "security_rule":
		return c.rules.Get(ctx, &vpcpb.GetSecurityRuleRequest{Id: id})
	}
	t.Fatalf("unknown kind %q", kind)
	return nil, nil //nolint:nilnil // unreachable
}

// mustGet fetches a resource and fails the test when it is missing.
func (c *clients) mustGet(t *testing.T, kind, id string) metaOf {
	t.Helper()
	m, err := c.get(t, kind, id)
	if err != nil {
		t.Fatalf("Get %s %s: %v", kind, id, err)
	}
	return m
}

func (c *clients) gone(t *testing.T, kind, id string) {
	t.Helper()
	if _, err := c.get(t, kind, id); status.Code(err) != codes.NotFound {
		t.Fatalf("Get %s %s after delete: code %v, want NotFound (err: %v)", kind, id, status.Code(err), err)
	}
}

// matchState checks that every managed resource in the state exists in the
// mock under the same ID.
func (c *clients) matchState(t *testing.T, st map[string]stateRes) {
	t.Helper()
	for name, res := range st {
		if got := c.mustGet(t, res.Kind, res.ID).GetMetadata().GetId(); got != res.ID {
			t.Fatalf("%s: mock id %q, state id %q", name, got, res.ID)
		}
	}
}

// version returns the resource_version of a resource.
func (c *clients) version(t *testing.T, kind, id string) int64 {
	t.Helper()
	return c.mustGet(t, kind, id).GetMetadata().GetResourceVersion()
}

// nameLabels returns the name and labels of a resource.
func (c *clients) nameLabels(t *testing.T, kind, id string) (name string, labels map[string]string) {
	t.Helper()
	md := c.mustGet(t, kind, id).GetMetadata()
	return md.GetName(), md.GetLabels()
}

// defaults are the template values every piece falls back to.
var defaults = map[string]string{
	"pool_name": "e2e-pool", "pool_labels": `team = "platform"`, "pool_cidrs": `{ cidr = "/16" }`,
	"pool_source": "", "pool_visibility": "PRIVATE",
	"net_name": "e2e-net", "net_labels": `team = "platform"`, "net_pools": "",
	"sub_name": "e2e-sub", "sub_labels": `team = "platform"`,
	"alloc_name": "e2e-alloc", "alloc_labels": `team = "platform"`, "alloc_cidr": "/32", "alloc_subnet": "sub",
	"rt_name": "e2e-rt", "rt_labels": `team = "platform"`, "rt_net": "net",
	"route_name": "e2e-route", "route_labels": `team = "platform"`, "route_desc": "to the appliance", "route_dest": "192.168.0.0/16",
	"sg_name": "e2e-sg", "sg_labels": `team = "platform"`, "sg_net": "net",
	"rule_name": "e2e-rule", "rule_labels": `team = "platform"`, "rule_priority": "100", "rule_protocol": "TCP", "rule_ingress": `source_cidrs = ["0.0.0.0/0"], destination_ports = [443]`,
}

// pieceHCL are the building blocks of every config; a scenario picks pieces.
var pieceHCL = map[string]string{
	"pool": `
resource "nebius_vpc_v1_pool" "pool" {
  parent_id      = "{{.proj}}"
  name           = "{{.pool_name}}"
  labels         = { {{.pool_labels}} }
  source_pool_id = "{{.pool_source}}"
  version        = "IPV4"
  visibility     = "{{.pool_visibility}}"
  cidrs          = [{{.pool_cidrs}}]
}
`,
	"net": `
resource "nebius_vpc_v1_network" "net" {
  parent_id = "{{.proj}}"
  name      = "{{.net_name}}"
  labels    = { {{.net_labels}} }
  {{.net_pools}}
}
`,
	"net2": `
resource "nebius_vpc_v1_network" "net2" {
  parent_id = "{{.proj}}"
  name      = "e2e-net2"
}
`,
	"sub": `
resource "nebius_vpc_v1_subnet" "sub" {
  parent_id  = "{{.proj}}"
  name       = "{{.sub_name}}"
  labels     = { {{.sub_labels}} }
  network_id = nebius_vpc_v1_network.net.id
  ipv4_private_pools = { pools = [{ cidrs = [{ cidr = "/24" }] }] }
}
`,
	"sub2": `
resource "nebius_vpc_v1_subnet" "sub2" {
  parent_id  = "{{.proj}}"
  name       = "e2e-sub2"
  network_id = nebius_vpc_v1_network.net.id
  ipv4_private_pools = { pools = [{ cidrs = [{ cidr = "/24" }] }] }
  depends_on = [nebius_vpc_v1_subnet.sub]
}
`,
	"alloc": `
resource "nebius_vpc_v1_allocation" "alloc" {
  parent_id = "{{.proj}}"
  name      = "{{.alloc_name}}"
  labels    = { {{.alloc_labels}} }
  ipv4_private = {
    subnet_id = nebius_vpc_v1_subnet.{{.alloc_subnet}}.id
    cidr      = "{{.alloc_cidr}}"
  }
}
`,
	"rt": `
resource "nebius_vpc_v1_route_table" "rt" {
  parent_id  = "{{.proj}}"
  name       = "{{.rt_name}}"
  labels     = { {{.rt_labels}} }
  network_id = nebius_vpc_v1_network.{{.rt_net}}.id
}
`,
	"route": `
resource "nebius_vpc_v1_route" "route" {
  parent_id   = nebius_vpc_v1_route_table.rt.id
  name        = "{{.route_name}}"
  labels      = { {{.route_labels}} }
  description = "{{.route_desc}}"
  destination = { cidr = "{{.route_dest}}" }
  next_hop    = { allocation = { id = nebius_vpc_v1_allocation.alloc.id } }
}
`,
	"sg": `
resource "nebius_vpc_v1_security_group" "sg" {
  parent_id  = "{{.proj}}"
  name       = "{{.sg_name}}"
  labels     = { {{.sg_labels}} }
  network_id = nebius_vpc_v1_network.{{.sg_net}}.id
}
`,
	"rule": `
resource "nebius_vpc_v1_security_rule" "rule" {
  parent_id = nebius_vpc_v1_security_group.sg.id
  name      = "{{.rule_name}}"
  labels    = { {{.rule_labels}} }
  access    = "ALLOW"
  protocol  = "{{.rule_protocol}}"
  priority  = {{.rule_priority}}
  ingress   = { {{.rule_ingress}} }
}
`,
}
