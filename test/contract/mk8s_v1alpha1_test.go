//nolint:staticcheck // SA1019: nebius.mk8s.v1alpha1 is deprecated upstream; serving it is this file's purpose
package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	mk8sv1alpha1pb "github.com/nebius/gosdk/proto/nebius/mk8s/v1alpha1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	mk8ssvc "github.com/nebius/gosdk/services/nebius/mk8s/v1"
	mk8salpha "github.com/nebius/gosdk/services/nebius/mk8s/v1alpha1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// TestMK8sV1Alpha1ServesTheV1Resources drives the deprecated v1alpha1
// ClusterService and NodeGroupService with the official gosdk clients, under
// enforced authorization, and checks the v1 services see the same resources.
func TestMK8sV1Alpha1ServesTheV1Resources(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(20*time.Millisecond),
		testkit.WithAuthz(authz.ModeEnforce))
	sdk := inst.SDK(t)
	ctx := t.Context()
	project := inst.ProjectID
	alphaClusters, alphaGroups := mk8salpha.NewClusterService(sdk), mk8salpha.NewNodeGroupService(sdk)
	v1Clusters, v1Groups := mk8ssvc.NewClusterService(sdk), mk8ssvc.NewNodeGroupService(sdk)
	wait := alphaWait(t)

	sn, err := vpcsvc.NewSubnetService(sdk).GetByName(ctx, &vpcpb.GetSubnetByNameRequest{ParentId: project, Name: "default-subnet"})
	if err != nil {
		t.Fatalf("default subnet: %v", err)
	}
	subnet := sn.GetMetadata().GetId()

	vers, err := alphaClusters.ListControlPlaneVersions(ctx, &mk8sv1alpha1pb.ListClusterControlPlaneVersionsRequest{})
	if err != nil || len(vers.GetItems()) == 0 || vers.GetItems()[0].GetVersion() == "" {
		t.Fatalf("ListControlPlaneVersions = %v, %v; want a catalog", vers, err)
	}

	clusterID := wait(alphaClusters.Create(ctx, &mk8sv1alpha1pb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: "legacy", Labels: map[string]string{"gen": "alpha"}},
		Spec: &mk8sv1alpha1pb.ClusterSpec{ControlPlane: &mk8sv1alpha1pb.ControlPlaneSpec{
			SubnetId:  subnet,
			Endpoints: &mk8sv1alpha1pb.ControlPlaneEndpointsSpec{PublicEndpoint: &mk8sv1alpha1pb.PublicEndpointSpec{}},
		}},
	}))

	// The same resource through both versions.
	c, err := alphaClusters.Get(ctx, &mk8sv1alpha1pb.GetClusterRequest{Id: clusterID})
	if err != nil {
		t.Fatalf("alpha get cluster: %v", err)
	}
	if c.GetStatus().GetState() != mk8sv1alpha1pb.ClusterStatus_RUNNING || c.GetSpec().GetControlPlane().GetSubnetId() != subnet ||
		c.GetStatus().GetControlPlane().GetEndpoints().GetPublicEndpoint() == "" || c.GetMetadata().GetLabels()["gen"] != "alpha" {
		t.Fatalf("alpha cluster = %v, want RUNNING on %s with a public endpoint", c, subnet)
	}
	v1c, err := v1Clusters.Get(ctx, &mk8spb.GetClusterRequest{Id: clusterID})
	if err != nil || v1c.GetMetadata().GetName() != "legacy" || v1c.GetStatus().GetControlPlane().GetVersion() != c.GetStatus().GetControlPlane().GetVersion() {
		t.Fatalf("v1 get of the alpha cluster = %v, %v", v1c, err)
	}
	if byName, err := alphaClusters.GetByName(ctx, &mk8sv1alpha1pb.GetClusterByNameRequest{ParentId: project, Name: "legacy"}); err != nil || byName.GetMetadata().GetId() != clusterID {
		t.Errorf("alpha GetByName = %v, %v", byName, err)
	}
	if l, err := alphaClusters.List(ctx, &mk8sv1alpha1pb.ListClustersRequest{ParentId: project}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("alpha List = %v, %v; want the one cluster", l, err)
	}

	wait(alphaClusters.Update(ctx, &mk8sv1alpha1pb.UpdateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{Id: clusterID, ParentId: project, Name: "legacy", Labels: map[string]string{"gen": "alpha", "x": "y"}},
		Spec:     c.GetSpec(),
	}))
	if v1c, err = v1Clusters.Get(ctx, &mk8spb.GetClusterRequest{Id: clusterID}); err != nil || v1c.GetMetadata().GetLabels()["x"] != "y" {
		t.Fatalf("v1 cluster after alpha update = %v, %v; want label x=y", v1c, err)
	}

	// A CPU node group: v1alpha1 requires a drivers preset only for GPU settings.
	groupID := wait(alphaGroups.Create(ctx, &mk8sv1alpha1pb.CreateNodeGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: clusterID, Name: "cpu"},
		Spec: &mk8sv1alpha1pb.NodeGroupSpec{
			Size: &mk8sv1alpha1pb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: 1},
			Template: &mk8sv1alpha1pb.NodeTemplate{
				Resources:         &mk8sv1alpha1pb.ResourcesSpec{Platform: "cpu-d3", Size: &mk8sv1alpha1pb.ResourcesSpec_Preset{Preset: "2vcpu-8gb"}},
				NetworkInterfaces: []*mk8sv1alpha1pb.NetworkInterfaceTemplate{{PublicIpAddress: &mk8sv1alpha1pb.PublicIPAddress{}}},
			},
		},
	}))
	g, err := alphaGroups.Get(ctx, &mk8sv1alpha1pb.GetNodeGroupRequest{Id: groupID})
	if err != nil {
		t.Fatalf("alpha get node group: %v", err)
	}
	if st := g.GetStatus(); st.GetState() != mk8sv1alpha1pb.NodeGroupStatus_RUNNING || st.GetNodeCount() != 1 || st.GetReadyNodeCount() != 1 {
		t.Fatalf("alpha node group status = %v, want RUNNING with 1 ready node", st)
	}
	v1g, err := v1Groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: groupID})
	if err != nil || v1g.GetStatus().GetVersion() != g.GetStatus().GetVersion() || v1g.GetSpec().GetFixedNodeCount() != 1 {
		t.Fatalf("v1 get of the alpha node group = %v, %v", v1g, err)
	}
	if byName, err := alphaGroups.GetByName(ctx, &mk8sv1alpha1pb.GetNodeGroupByNameRequest{ParentId: clusterID, Name: "cpu"}); err != nil || byName.GetMetadata().GetId() != groupID {
		t.Errorf("alpha node group GetByName = %v, %v", byName, err)
	}
	if l, err := alphaGroups.List(ctx, &mk8sv1alpha1pb.ListNodeGroupsRequest{ParentId: clusterID}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("alpha node group List = %v, %v; want the one group", l, err)
	}

	spec := g.GetSpec()
	spec.Size = &mk8sv1alpha1pb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: 2}
	wait(alphaGroups.Update(ctx, &mk8sv1alpha1pb.UpdateNodeGroupRequest{Metadata: g.GetMetadata(), Spec: spec}))
	if v1g, err = v1Groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: groupID}); err != nil || v1g.GetStatus().GetNodeCount() != 2 {
		t.Fatalf("v1 node group after alpha scale to 2 = %v, %v", v1g, err)
	}
	wait(alphaGroups.Upgrade(ctx, &mk8sv1alpha1pb.UpgradeNodeGroupRequest{
		Id: groupID, UpgradeType: &mk8sv1alpha1pb.UpgradeNodeGroupRequest_LatestInfraVersion{LatestInfraVersion: &emptypb.Empty{}},
	}))

	wait(alphaGroups.Delete(ctx, &mk8sv1alpha1pb.DeleteNodeGroupRequest{Id: groupID}))
	if _, err := v1Groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: groupID}); status.Code(err) != codes.NotFound {
		t.Errorf("v1 get node group after alpha delete: %v, want NotFound", err)
	}
	wait(alphaClusters.Delete(ctx, &mk8sv1alpha1pb.DeleteClusterRequest{Id: clusterID}))
	if _, err := v1Clusters.Get(ctx, &mk8spb.GetClusterRequest{Id: clusterID}); status.Code(err) != codes.NotFound {
		t.Errorf("v1 get cluster after alpha delete: %v, want NotFound", err)
	}

	// Enforced authorization: a viewer reads but cannot create.
	viewer := mk8salpha.NewClusterService(inst.SDKAs(t, "nmock-dev-viewer"))
	if _, err := viewer.List(ctx, &mk8sv1alpha1pb.ListClustersRequest{ParentId: project}); err != nil {
		t.Errorf("viewer alpha List: %v", err)
	}
	_, err = viewer.Create(ctx, &mk8sv1alpha1pb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: "denied"},
		Spec:     &mk8sv1alpha1pb.ClusterSpec{ControlPlane: &mk8sv1alpha1pb.ControlPlaneSpec{SubnetId: subnet}},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("viewer alpha Create: %v, want PermissionDenied", err)
	}
}
