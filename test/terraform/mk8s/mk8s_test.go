//go:build e2e

// Package mk8s runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's
// Managed Kubernetes service (clusters and node groups, v1 and v1alpha1).
// Nothing is patched or forked.
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package mk8s

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	mk8sv1svc "github.com/nebius/gosdk/services/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const nodeGroupLabel = "mk8s.nebius.ai/node-group-id"

func startMK8s(t *testing.T) *testkit.Instance {
	t.Helper()
	// A non-zero delay makes operations asynchronous, so the provider's
	// OperationService polling path is exercised too; rollouts pause between
	// batches, so the provider waits for a multi-step rollout.
	return testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New()},
		mockerOptions(testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(200*time.Millisecond),
			testkit.WithConfig(func(c *config.Config) { c.MK8s.RolloutStep = 300 * time.Millisecond }),
			testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
}

// clusterView and groupView are the fields the scenario checks, read through
// the Go SDK of either API version.
type clusterView struct {
	Name, State, PublicEndpoint, PrivateEndpoint, Version string
	Labels                                                map[string]string
	Etcd                                                  int64
	ServiceCIDRs                                          []string
}

type groupView struct {
	Name, State                          string
	Node, Ready, Target, Fixed, Min, Max int64
	TemplateLabels                       map[string]string
	Platform, Preset, DiskType, ParentID string
	DiskGiB                              int64
}

// sdkAPI reads clusters and node groups through the Go SDK.
type sdkAPI interface {
	cluster(t *testing.T, id string) clusterView
	group(t *testing.T, id string) groupView
	createCluster(t *testing.T, project, name string) string
	createGroup(t *testing.T, cluster, name string) string
}

type v1API struct {
	clusters mk8sv1svc.ClusterService
	groups   mk8sv1svc.NodeGroupService
}

func (a v1API) cluster(t *testing.T, id string) clusterView {
	t.Helper()
	c, err := a.clusters.Get(t.Context(), &mk8spb.GetClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("get cluster %s: %v", id, err)
	}
	cp := c.GetStatus().GetControlPlane()
	return clusterView{
		Name: c.GetMetadata().GetName(), State: c.GetStatus().GetState().String(), Labels: c.GetMetadata().GetLabels(),
		PublicEndpoint: cp.GetEndpoints().GetPublicEndpoint(), PrivateEndpoint: cp.GetEndpoints().GetPrivateEndpoint(),
		Version: cp.GetVersion(), Etcd: cp.GetEtcdClusterSize(), ServiceCIDRs: c.GetSpec().GetKubeNetwork().GetServiceCidrs(),
	}
}

func (a v1API) group(t *testing.T, id string) groupView {
	t.Helper()
	g, err := a.groups.Get(t.Context(), &mk8spb.GetNodeGroupRequest{Id: id})
	if err != nil {
		t.Fatalf("get node group %s: %v", id, err)
	}
	st, sp := g.GetStatus(), g.GetSpec()
	return groupView{
		Name: g.GetMetadata().GetName(), State: st.GetState().String(), ParentID: g.GetMetadata().GetParentId(),
		Node: st.GetNodeCount(), Ready: st.GetReadyNodeCount(), Target: st.GetTargetNodeCount(),
		Fixed: sp.GetFixedNodeCount(), Min: sp.GetAutoscaling().GetMinNodeCount(), Max: sp.GetAutoscaling().GetMaxNodeCount(),
		TemplateLabels: sp.GetTemplate().GetMetadata().GetLabels(),
		Platform:       sp.GetTemplate().GetResources().GetPlatform(), Preset: sp.GetTemplate().GetResources().GetPreset(),
		DiskType: sp.GetTemplate().GetBootDisk().GetType().String(), DiskGiB: sp.GetTemplate().GetBootDisk().GetSizeGibibytes(),
	}
}

func (a v1API) createCluster(t *testing.T, project, name string) string {
	t.Helper()
	return waitOp(t)(a.clusters.Create(t.Context(), &mk8spb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: name},
		Spec: &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{
			Endpoints: &mk8spb.ControlPlaneEndpointsSpec{PublicEndpoint: &mk8spb.PublicEndpointSpec{}},
		}},
	}))
}

func (a v1API) createGroup(t *testing.T, cluster, name string) string {
	t.Helper()
	return waitOp(t)(a.groups.Create(t.Context(), &mk8spb.CreateNodeGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: cluster, Name: name},
		Spec: &mk8spb.NodeGroupSpec{
			Size: &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: 1},
			Template: &mk8spb.NodeTemplate{
				Resources: &mk8spb.ResourcesSpec{Platform: "cpu-d3", Size: &mk8spb.ResourcesSpec_Preset{Preset: "2vcpu-8gb"}},
			},
		},
	}))
}

// waitOp returns a function that waits for an operation and returns its
// resource ID: waitOp(t)(client.Create(…)).
func waitOp(t *testing.T) func(operations.Operation, error) string {
	t.Helper()
	return func(op operations.Operation, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if _, err := op.Wait(t.Context()); err != nil {
			t.Fatalf("operation: %v", err)
		}
		return op.ResourceID()
	}
}

// nodes lists the compute instances of one node group (by label) and, as the
// second result, every instance in the project.
func nodes(t *testing.T, inst computesvc.InstanceService, project, groupID string) (inGroup, all int) {
	t.Helper()
	res, err := inst.List(t.Context(), &computepb.ListInstancesRequest{ParentId: project})
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range res.GetItems() {
		if in.GetMetadata().GetLabels()[nodeGroupLabel] == groupID {
			inGroup++
		}
	}
	return inGroup, len(res.GetItems())
}

// nodeIDs are the instance IDs of a node group's nodes.
func nodeIDs(t *testing.T, inst computesvc.InstanceService, project, groupID string) []string {
	t.Helper()
	res, err := inst.List(t.Context(), &computepb.ListInstancesRequest{ParentId: project})
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	var out []string
	for _, in := range res.GetItems() {
		if in.GetMetadata().GetLabels()[nodeGroupLabel] == groupID {
			out = append(out, in.GetMetadata().GetId())
		}
	}
	return out
}

// cfg describes the rendered Terraform config.
type cfg struct {
	V, Proj, ClusterLabels, CIDR string
	Fixed                        int  // fixed_node_count; ignored with Autoscale
	Autoscale                    bool // autoscaling {min 1, max 3}
	TemplateLabels               string
	DataSources                  bool
	DiskGiB                      int // boot disk size; changing it rolls the nodes out
}

var cfgTmpl = template.Must(template.New("cfg").Parse(`
{{- if eq .V "v1alpha1"}}
# v1alpha1 requires the control plane subnet.
data "nebius_vpc_v1_subnet" "default" {
  parent_id = "{{.Proj}}"
  name      = "default-subnet"
}
{{end}}
resource "nebius_mk8s_{{.V}}_cluster" "c" {
  parent_id = "{{.Proj}}"
  name      = "tf-cluster"
  labels    = { {{.ClusterLabels}} }
  control_plane = {
    version           = "1.35"
{{- if eq .V "v1alpha1"}}
    subnet_id         = data.nebius_vpc_v1_subnet.default.id
{{- end}}
    endpoints         = { public_endpoint = {} }
    etcd_cluster_size = 1
  }
  kube_network = { service_cidrs = ["{{.CIDR}}"] }
}

resource "nebius_mk8s_{{.V}}_node_group" "g" {
  parent_id = nebius_mk8s_{{.V}}_cluster.c.id
  name      = "tf-pool"
{{- if .Autoscale}}
  autoscaling = {
    min_node_count = 1
    max_node_count = 3
  }
{{- else}}
  fixed_node_count = {{.Fixed}}
{{- end}}
  template = {
    metadata = { labels = { {{.TemplateLabels}} } }
    resources = {
      platform = "cpu-d3"
      preset   = "2vcpu-8gb"
    }
    boot_disk = {
      size_gibibytes = {{.DiskGiB}}
      type           = "NETWORK_SSD"
    }
    network_interfaces = [{ public_ip_address = {} }]
  }
}
{{if .DataSources}}
data "nebius_mk8s_{{.V}}_cluster" "by_id" {
  id = nebius_mk8s_{{.V}}_cluster.c.id
}
data "nebius_mk8s_{{.V}}_cluster" "by_name" {
  parent_id = "{{.Proj}}"
  name      = nebius_mk8s_{{.V}}_cluster.c.name
}
data "nebius_mk8s_{{.V}}_node_group" "by_id" {
  id = nebius_mk8s_{{.V}}_node_group.g.id
}
data "nebius_mk8s_{{.V}}_node_group" "by_name" {
  parent_id = nebius_mk8s_{{.V}}_cluster.c.id
  name      = nebius_mk8s_{{.V}}_node_group.g.name
}
{{end}}`))

func (c *cfg) render(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := cfgTmpl.Execute(&b, c); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func TestV1ClusterAndNodeGroupLifecycle(t *testing.T) {
	inst := startMK8s(t)
	sdk := inst.SDK(t)
	runLifecycle(t, inst, "v1", v1API{mk8sv1svc.NewClusterService(sdk), mk8sv1svc.NewNodeGroupService(sdk)})
}

// runLifecycle drives create, in-place update, an autoscaling switch, a
// replace and destroy; after each apply the plan must be empty and the mock
// (read through the Go SDK, including the node instances) must match.
func runLifecycle(t *testing.T, inst *testkit.Instance, v string, api sdkAPI) {
	t.Helper()
	instances := computesvc.NewInstanceService(inst.SDK(t))
	tf := newTerraform(t, inst)
	c := cfg{V: v, Proj: inst.ProjectID, ClusterLabels: `env = "dev"`, CIDR: "/20", Fixed: 2, TemplateLabels: `pool = "a"`, DiskGiB: 64}
	cName, gName := "c", "g"

	check := func(step string, wantNodes func(groupVersionNodes int64) bool, wantLabels, tmplLabels map[string]string, autoscale bool, fixed int64) {
		t.Helper()
		tf.expectNoChanges(t)
		ids := tf.stateIDs(t)
		cl := api.cluster(t, ids["c"])
		if cl.State != "RUNNING" || cl.Name != "tf-cluster" || cl.Etcd != 1 || !strings.HasPrefix(cl.PublicEndpoint, "https://") ||
			!strings.HasPrefix(cl.PrivateEndpoint, "https://") || !strings.HasPrefix(cl.Version, "1.35.") {
			t.Errorf("%s: cluster = %+v, want RUNNING etcd 1 on 1.35.x with https endpoints", step, cl)
		}
		for k, want := range wantLabels {
			if cl.Labels[k] != want {
				t.Errorf("%s: cluster labels = %v, want %s=%s", step, cl.Labels, k, want)
			}
		}
		g := api.group(t, ids["g"])
		if g.State != "RUNNING" || g.ParentID != ids["c"] || g.Node != g.Ready || g.Node != g.Target || !wantNodes(g.Node) ||
			g.Platform != "cpu-d3" || g.Preset != "2vcpu-8gb" || g.DiskType != "NETWORK_SSD" || g.DiskGiB != int64(c.DiskGiB) {
			t.Errorf("%s: node group = %+v", step, g)
		}
		if autoscale && (g.Min != 1 || g.Max != 3 || g.Fixed != 0) || !autoscale && (g.Fixed != fixed || g.Max != 0) {
			t.Errorf("%s: node group size spec = %+v, want autoscale=%v fixed=%d", step, g, autoscale, fixed)
		}
		for k, want := range tmplLabels {
			if g.TemplateLabels[k] != want {
				t.Errorf("%s: template labels = %v, want %s=%s", step, g.TemplateLabels, k, want)
			}
		}
		inGroup, all := nodes(t, instances, inst.ProjectID, ids["g"])
		if int64(inGroup) != g.Node || all != inGroup {
			t.Errorf("%s: instances in group = %d (project total %d), node group reports %d", step, inGroup, all, g.Node)
		}
	}
	exactly := func(n int64) func(int64) bool { return func(got int64) bool { return got == n } }
	between := func(lo, hi int64) func(int64) bool { return func(got int64) bool { return got >= lo && got <= hi } }

	// Cluster and node group in one apply: on plan the node group's parent is
	// unknown, which the mock's preflight (and the provider) must tolerate.
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	tf.expectPlan(t, map[string]string{cName: "create", gName: "create"})
	tf.apply(t)
	check("create", exactly(2), map[string]string{"env": "dev"}, map[string]string{"pool": "a"}, false, 2)
	firstGroup := tf.stateIDs(t)["g"]

	// Data sources by ID and by name read what Terraform created.
	c.DataSources = true
	tf.writeConfig(t, c.render(t))
	tf.apply(t)
	check("data sources", exactly(2), nil, nil, false, 2)
	checkDataSources(t, tf, v)

	// In place: cluster labels, node group size and template labels.
	c.ClusterLabels, c.Fixed, c.TemplateLabels = `env = "prod", team = "ml"`, 3, `pool = "b"`
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{cName: "update", gName: "update"})
	tf.apply(t)
	check("update", exactly(3), map[string]string{"env": "prod", "team": "ml"}, map[string]string{"pool": "b"}, false, 3)
	if got := tf.stateIDs(t)["g"]; got != firstGroup {
		t.Errorf("node group was replaced by an in-place update: %s -> %s", firstGroup, got)
	}

	// A template change rolls the nodes out (in batches, per the default
	// strategy): every node instance is replaced, the node group is not.
	before := nodeIDs(t, instances, inst.ProjectID, firstGroup)
	c.DiskGiB = 96
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{cName: "no-op", gName: "update"})
	tf.apply(t)
	check("rollout", exactly(3), nil, map[string]string{"pool": "b"}, false, 3)
	for _, id := range nodeIDs(t, instances, inst.ProjectID, firstGroup) {
		if slices.Contains(before, id) {
			t.Errorf("rollout: node instance %s was not replaced", id)
		}
	}

	// Fixed size to autoscaling.
	c.Autoscale = true
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{cName: "no-op", gName: "update"})
	tf.apply(t)
	check("autoscaling", between(1, 3), nil, map[string]string{"pool": "b"}, true, 0)

	// The service CIDR is immutable: the cluster is replaced, and the node
	// group (its parent changes) with it.
	c.CIDR = "/18"
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{cName: "replace", gName: "replace"})
	tf.apply(t)
	check("replace", between(1, 3), nil, nil, true, 0)
	if got := tf.stateIDs(t)["g"]; got == firstGroup {
		t.Errorf("node group kept its ID across the cluster replacement")
	}
	if cidrs := api.cluster(t, tf.stateIDs(t)["c"]).ServiceCIDRs; len(cidrs) != 1 || cidrs[0] != "/18" {
		t.Errorf("service_cidrs after replace = %v, want [/18]", cidrs)
	}
	if left, _ := nodes(t, instances, inst.ProjectID, firstGroup); left != 0 {
		t.Errorf("replaced node group still has %d instances", left)
	}

	lastGroup := tf.stateIDs(t)["g"]
	tf.destroy(t)
	if left, all := nodes(t, instances, inst.ProjectID, lastGroup); left != 0 || all != 0 {
		t.Errorf("after destroy: %d group instances, %d in project, want none", left, all)
	}
}

// checkDataSources asserts the data sources' attributes in the Terraform
// state against the resources.
func checkDataSources(t *testing.T, tf *tfRun, v string) {
	t.Helper()
	attrs := tf.stateAttrs(t)
	ids := tf.stateIDs(t)
	for _, name := range []string{"by_id", "by_name"} {
		cl := attrs["data.nebius_mk8s_"+v+"_cluster."+name]
		if cl["id"] != ids["c"] || cl["name"] != "tf-cluster" {
			t.Errorf("cluster data source %s = id %v name %v, want %s tf-cluster", name, cl["id"], cl["name"], ids["c"])
		}
		if st, _ := cl["status"].(map[string]any); st["state"] != "RUNNING" {
			t.Errorf("cluster data source %s status = %v, want RUNNING", name, cl["status"])
		}
		if labels, _ := cl["labels"].(map[string]any); labels["env"] != "dev" {
			t.Errorf("cluster data source %s labels = %v", name, cl["labels"])
		}
		g := attrs["data.nebius_mk8s_"+v+"_node_group."+name]
		if g["id"] != ids["g"] || g["name"] != "tf-pool" || g["parent_id"] != ids["c"] || fmt.Sprint(g["fixed_node_count"]) != "2" {
			t.Errorf("node group data source %s = %v", name, g)
		}
		if st, _ := g["status"].(map[string]any); st["state"] != "RUNNING" || fmt.Sprint(st["node_count"]) != "2" {
			t.Errorf("node group data source %s status = %v, want RUNNING with 2 nodes", name, g["status"])
		}
	}
}

// TestV1ViewerReadsButCannotCreate: the read-only principal resolves data
// sources of existing clusters and node groups, and is denied creating one.
func TestV1ViewerReadsButCannotCreate(t *testing.T) {
	inst := startMK8s(t)
	sdk := inst.SDK(t)
	api := v1API{mk8sv1svc.NewClusterService(sdk), mk8sv1svc.NewNodeGroupService(sdk)}
	clusterID := api.createCluster(t, inst.ProjectID, "seed")
	groupID := api.createGroup(t, clusterID, "seed-pool")

	tf := newTerraformAs(t, inst, "nmock-dev-viewer")
	tf.writeConfig(t, `
data "nebius_mk8s_v1_cluster" "by_id" {
  id = "`+clusterID+`"
}
data "nebius_mk8s_v1_node_group" "by_id" {
  id = "`+groupID+`"
}
data "nebius_mk8s_v1_node_group" "by_name" {
  parent_id = "`+clusterID+`"
  name      = "seed-pool"
}
`)
	tf.init(t)
	tf.applyNow(t)
	attrs := tf.stateAttrs(t)
	if got := attrs["data.nebius_mk8s_v1_cluster.by_id"]["name"]; got != "seed" {
		t.Errorf("viewer read cluster name = %v, want seed", got)
	}
	for _, name := range []string{"by_id", "by_name"} {
		if got := attrs["data.nebius_mk8s_v1_node_group."+name]["id"]; got != groupID {
			t.Errorf("viewer read node group %s id = %v, want %s", name, got, groupID)
		}
	}

	tf.writeConfig(t, `
resource "nebius_mk8s_v1_cluster" "denied" {
  parent_id     = "`+inst.ProjectID+`"
  name          = "viewer-cluster"
  control_plane = { endpoints = { public_endpoint = {} } }
}
`)
	err := tf.tf.Apply(t.Context())
	if err == nil {
		t.Fatalf("terraform apply as viewer succeeded, want a permission error")
	}
	if out := tf.out.String(); !strings.Contains(out, "PermissionDenied") && !strings.Contains(strings.ToLower(out), "permission") {
		t.Errorf("apply as viewer failed, but not with a permission error: %v\n%s", err, out)
	}
	if res, err := api.clusters.List(t.Context(), &mk8spb.ListClustersRequest{ParentId: inst.ProjectID}); err != nil || len(res.GetItems()) != 1 {
		t.Errorf("clusters after the denied create = %v (%v), want only the seeded one", res.GetItems(), err)
	}
}
