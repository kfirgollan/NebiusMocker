//go:build e2e

//nolint:staticcheck // SA1019: nebius.mk8s.v1alpha1 and its operations are deprecated upstream; the provider still serves them and this file tests exactly that
package mk8s

import (
	"testing"

	"github.com/nebius/gosdk/operations/alphaops"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8sv1alpha1pb "github.com/nebius/gosdk/proto/nebius/mk8s/v1alpha1"
	mk8sv1alpha1svc "github.com/nebius/gosdk/services/nebius/mk8s/v1alpha1"
)

type alphaAPI struct {
	clusters mk8sv1alpha1svc.ClusterService
	groups   mk8sv1alpha1svc.NodeGroupService
}

func (a alphaAPI) cluster(t *testing.T, id string) clusterView {
	t.Helper()
	c, err := a.clusters.Get(t.Context(), &mk8sv1alpha1pb.GetClusterRequest{Id: id})
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

func (a alphaAPI) group(t *testing.T, id string) groupView {
	t.Helper()
	g, err := a.groups.Get(t.Context(), &mk8sv1alpha1pb.GetNodeGroupRequest{Id: id})
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

func (a alphaAPI) createCluster(t *testing.T, project, name string) string {
	t.Helper()
	return waitAlphaOp(t)(a.clusters.Create(t.Context(), &mk8sv1alpha1pb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: name},
		Spec: &mk8sv1alpha1pb.ClusterSpec{ControlPlane: &mk8sv1alpha1pb.ControlPlaneSpec{
			Endpoints: &mk8sv1alpha1pb.ControlPlaneEndpointsSpec{PublicEndpoint: &mk8sv1alpha1pb.PublicEndpointSpec{}},
		}},
	}))
}

func (a alphaAPI) createGroup(t *testing.T, cluster, name string) string {
	t.Helper()
	return waitAlphaOp(t)(a.groups.Create(t.Context(), &mk8sv1alpha1pb.CreateNodeGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: cluster, Name: name},
		Spec: &mk8sv1alpha1pb.NodeGroupSpec{
			Size: &mk8sv1alpha1pb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: 1},
			Template: &mk8sv1alpha1pb.NodeTemplate{
				Resources: &mk8sv1alpha1pb.ResourcesSpec{Platform: "cpu-d3", Size: &mk8sv1alpha1pb.ResourcesSpec_Preset{Preset: "2vcpu-8gb"}},
			},
		},
	}))
}

// waitAlphaOp is waitOp for the v1alpha1 operation type.
func waitAlphaOp(t *testing.T) func(*alphaops.Operation, error) string {
	t.Helper()
	return func(op *alphaops.Operation, err error) string {
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

// TestV1alpha1ClusterAndNodeGroupLifecycle runs the lifecycle config with
// {{.V}} = v1alpha1, i.e. resource "nebius_mk8s_v1alpha1_cluster",
// resource "nebius_mk8s_v1alpha1_node_group", data "nebius_mk8s_v1alpha1_cluster"
// and data "nebius_mk8s_v1alpha1_node_group".
func TestV1alpha1ClusterAndNodeGroupLifecycle(t *testing.T) {
	inst := startMK8s(t)
	sdk := inst.SDK(t)
	runLifecycle(t, inst, "v1alpha1", alphaAPI{mk8sv1alpha1svc.NewClusterService(sdk), mk8sv1alpha1svc.NewNodeGroupService(sdk)})
}
