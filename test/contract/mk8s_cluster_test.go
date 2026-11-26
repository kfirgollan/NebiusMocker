package contract

import (
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	mk8ssvc "github.com/nebius/gosdk/services/nebius/mk8s/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// mk8sEnv is a mocker with IAM, VPC, Compute and mk8s (mock backend).
type mk8sEnv struct {
	inst      *testkit.Instance
	project   string
	clusters  mk8ssvc.ClusterService
	groups    mk8ssvc.NodeGroupService
	instances computesvc.InstanceService
	subnet    string // the seeded default subnet
}

func startMK8s(t *testing.T, opts ...testkit.Option) *mk8sEnv {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(20 * time.Millisecond)}, opts...)
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New()}, opts...)
	sdk := inst.SDK(t)
	e := &mk8sEnv{
		inst: inst, project: inst.ProjectID, clusters: mk8ssvc.NewClusterService(sdk), groups: mk8ssvc.NewNodeGroupService(sdk),
		instances: computesvc.NewInstanceService(sdk),
	}
	sn, err := vpcsvc.NewSubnetService(sdk).GetByName(t.Context(), &vpcpb.GetSubnetByNameRequest{ParentId: e.project, Name: "default-subnet"})
	if err != nil {
		t.Fatalf("default subnet: %v", err)
	}
	e.subnet = sn.GetMetadata().GetId()
	return e
}

// opWait returns a function that waits for an operation and returns its
// resource ID: opWait(t)(client.Create(…)).
func opWait(t *testing.T) func(operations.Operation, error) string {
	t.Helper()
	return func(op operations.Operation, err error) string {
		t.Helper()
		return mustOp(t, op, err)
	}
}

// nodes lists the compute instances labeled with the node group.
func (e *mk8sEnv) nodes(t *testing.T, groupID string) []*computepb.Instance {
	t.Helper()
	res, err := e.instances.List(t.Context(), &computepb.ListInstancesRequest{ParentId: e.project})
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	var out []*computepb.Instance
	for _, in := range res.GetItems() {
		if in.GetMetadata().GetLabels()["mk8s.nebius.ai/node-group-id"] == groupID {
			out = append(out, in)
		}
	}
	return out
}

func TestMK8sClusterAndNodeGroupLifecycle(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()

	clusterID := opWait(t)(e.clusters.Create(ctx, &mk8spb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.project, Name: "main"},
		Spec: &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{
			Endpoints: &mk8spb.ControlPlaneEndpointsSpec{PublicEndpoint: &mk8spb.PublicEndpointSpec{}},
		}},
	}))
	c, err := e.clusters.Get(ctx, &mk8spb.GetClusterRequest{Id: clusterID})
	if err != nil {
		t.Fatalf("get cluster: %v", err)
	}
	checkRunningCluster(t, c, e.subnet)

	groupID := opWait(t)(e.groups.Create(ctx, &mk8spb.CreateNodeGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: clusterID, Name: "cpu"},
		Spec: &mk8spb.NodeGroupSpec{
			Size: &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: 2},
			Template: &mk8spb.NodeTemplate{
				Resources:         &mk8spb.ResourcesSpec{Platform: "cpu-d3", Size: &mk8spb.ResourcesSpec_Preset{Preset: "2vcpu-8gb"}},
				NetworkInterfaces: []*mk8spb.NetworkInterfaceTemplate{{PublicIpAddress: &mk8spb.PublicIPAddress{}}},
			},
		},
	}))
	g, err := e.groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: groupID})
	if err != nil {
		t.Fatalf("get node group: %v", err)
	}
	st := g.GetStatus()
	if st.GetState() != mk8spb.NodeGroupStatus_RUNNING || st.GetNodeCount() != 2 || st.GetReadyNodeCount() != 2 || st.GetVersion() != "1.36.0-nebius-node.1" {
		t.Fatalf("node group status = %v, want RUNNING with 2 ready nodes on 1.36.0-nebius-node.1", st)
	}
	if got := g.GetSpec().GetTemplate().GetNetworkInterfaces()[0].GetSubnetId(); got != e.subnet {
		t.Errorf("node subnet = %q, want the cluster's %q", got, e.subnet)
	}
	nodes := e.nodes(t, groupID)
	if len(nodes) != 2 {
		t.Fatalf("node instances = %d, want 2", len(nodes))
	}
	for _, n := range nodes {
		checkNodeInstance(t, n, clusterID)
	}

	scaleTo(t, e, g, 1)
	if n := len(e.nodes(t, groupID)); n != 1 {
		t.Fatalf("node instances after scaling to 1 = %d, want 1", n)
	}

	opWait(t)(e.clusters.Delete(ctx, &mk8spb.DeleteClusterRequest{Id: clusterID}))
	if _, err := e.groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: groupID}); status.Code(err) != codes.NotFound {
		t.Errorf("get node group after cluster delete: %v, want NotFound", err)
	}
	if _, err := e.clusters.Get(ctx, &mk8spb.GetClusterRequest{Id: clusterID}); status.Code(err) != codes.NotFound {
		t.Errorf("get cluster after delete: %v, want NotFound", err)
	}
	if n := len(e.nodes(t, groupID)); n != 0 {
		t.Errorf("node instances after cluster delete = %d, want 0", n)
	}
}

func checkRunningCluster(t *testing.T, c *mk8spb.Cluster, subnet string) {
	t.Helper()
	if got := c.GetSpec().GetControlPlane().GetSubnetId(); got != subnet {
		t.Errorf("spec.control_plane.subnet_id = %q, want the default subnet %q", got, subnet)
	}
	cp := c.GetStatus().GetControlPlane()
	if c.GetStatus().GetState() != mk8spb.ClusterStatus_RUNNING || c.GetStatus().GetReconciling() {
		t.Fatalf("cluster status = %v, want RUNNING and not reconciling", c.GetStatus())
	}
	if cp.GetVersion() != "1.36.0-nebius-cp.1" || cp.GetEtcdClusterSize() != 3 {
		t.Errorf("control plane = %v, want version 1.36.0-nebius-cp.1 and etcd size 3", cp)
	}
	if ep := cp.GetEndpoints().GetPublicEndpoint(); !strings.HasPrefix(ep, "https://") {
		t.Errorf("public endpoint = %q, want https://…", ep)
	}
	if ep := cp.GetEndpoints().GetPrivateEndpoint(); !strings.HasPrefix(ep, "https://") {
		t.Errorf("private endpoint = %q, want https://…", ep)
	}
	if b, _ := pem.Decode([]byte(cp.GetAuth().GetClusterCaCertificate())); b == nil || b.Type != "CERTIFICATE" {
		t.Errorf("cluster CA is not a PEM certificate: %q", cp.GetAuth().GetClusterCaCertificate())
	}
}

func checkNodeInstance(t *testing.T, n *computepb.Instance, clusterID string) {
	t.Helper()
	if got := n.GetMetadata().GetLabels()["mk8s.nebius.ai/cluster-id"]; got != clusterID {
		t.Errorf("node %s cluster label = %q, want %q", n.GetMetadata().GetName(), got, clusterID)
	}
	if n.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
		t.Errorf("node %s state = %v, want RUNNING", n.GetMetadata().GetName(), n.GetStatus().GetState())
	}
	if !strings.HasPrefix(n.GetMetadata().GetName(), "cpu-") || n.GetSpec().GetResources().GetPlatform() != "cpu-d3" {
		t.Errorf("node = %s on %s, want cpu-… on cpu-d3", n.GetMetadata().GetName(), n.GetSpec().GetResources().GetPlatform())
	}
	nics := n.GetStatus().GetNetworkInterfaces()
	if len(nics) != 1 || nics[0].GetIpAddress().GetAddress() == "" || nics[0].GetPublicIpAddress().GetAddress() == "" {
		t.Errorf("node %s interfaces = %v, want one with a private and a public address", n.GetMetadata().GetName(), nics)
	}
}

// scaleTo updates a fixed node group's size the way Terraform does: the full
// spec back with the new count.
func scaleTo(t *testing.T, e *mk8sEnv, g *mk8spb.NodeGroup, count int64) {
	t.Helper()
	spec := g.GetSpec()
	spec.Size = &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: count}
	opWait(t)(e.groups.Update(t.Context(), &mk8spb.UpdateNodeGroupRequest{Metadata: g.GetMetadata(), Spec: spec}))
	got, err := e.groups.Get(t.Context(), &mk8spb.GetNodeGroupRequest{Id: g.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("get node group: %v", err)
	}
	if st := got.GetStatus(); st.GetNodeCount() != count || st.GetTargetNodeCount() != count || st.GetReadyNodeCount() != count {
		t.Fatalf("status after scaling to %d = %v", count, st)
	}
}
