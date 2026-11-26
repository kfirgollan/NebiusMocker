package contract

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	mk8ssvc "github.com/nebius/gosdk/services/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func startMK8sEnforcing(t *testing.T) *mk8sEnv {
	t.Helper()
	return startMK8s(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
}

// as returns the mk8s and compute clients authenticated with token.
func (e *mk8sEnv) as(t *testing.T, token string) (mk8ssvc.ClusterService, mk8ssvc.NodeGroupService, computesvc.InstanceService) {
	t.Helper()
	sdk := e.inst.SDKAs(t, token)
	return mk8ssvc.NewClusterService(sdk), mk8ssvc.NewNodeGroupService(sdk), computesvc.NewInstanceService(sdk)
}

func TestMK8sViewerReadsButCannotWrite(t *testing.T) {
	t.Parallel()
	e := startMK8sEnforcing(t)
	ctx := t.Context()
	cid := e.newCluster(t, "guarded")
	gid := e.newGroup(t, cid, "pool", fixedGroup(1))
	clusters, groups, instances := e.as(t, config.DefaultViewerToken)

	if _, err := clusters.Get(ctx, &mk8spb.GetClusterRequest{Id: cid}); err != nil {
		t.Errorf("viewer cluster Get: %v", err)
	}
	if _, err := clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.project, Name: "guarded"}); err != nil {
		t.Errorf("viewer cluster GetByName: %v", err)
	}
	if l, err := clusters.List(ctx, &mk8spb.ListClustersRequest{ParentId: e.project}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("viewer cluster List = %v, %v", l, err)
	}
	if _, err := clusters.ListControlPlaneVersions(ctx, &mk8spb.ListClusterControlPlaneVersionsRequest{}); err != nil {
		t.Errorf("viewer ListControlPlaneVersions: %v", err)
	}
	if _, err := groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: gid}); err != nil {
		t.Errorf("viewer node group Get: %v", err)
	}
	if _, err := groups.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: cid, Name: "pool"}); err != nil {
		t.Errorf("viewer node group GetByName: %v", err)
	}
	if l, err := groups.List(ctx, &mk8spb.ListNodeGroupsRequest{ParentId: cid}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("viewer node group List = %v, %v", l, err)
	}
	if _, err := groups.GetCompatibilityMatrix(ctx, &mk8spb.GetNodeGroupCompatibilityMatrixRequest{ClusterKubernetesVersion: "1.36"}); err != nil {
		t.Errorf("viewer GetCompatibilityMatrix: %v", err)
	}
	if _, err := groups.PreflightCheck(ctx, &mk8spb.PreflightCheckNodeGroupRequest{
		Context:  &commonpb.PreflightCheckContext{Action: commonpb.PreflightCheckContext_CREATE},
		Metadata: &commonpb.ResourceMetadata{ParentId: cid, Name: "x"}, Spec: fixedGroup(1),
	}); err != nil {
		t.Errorf("viewer PreflightCheck: %v", err)
	}
	nodes := e.nodes(t, gid)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if _, err := instances.Get(ctx, &computepb.GetInstanceRequest{Id: nodes[0].GetMetadata().GetId()}); err != nil {
		t.Errorf("viewer node Get: %v", err)
	}
	if l, err := instances.List(ctx, &computepb.ListInstancesRequest{ParentId: e.project}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("viewer instance List = %v, %v", l, err)
	}

	// Writes are denied.
	_, err := clusters.Create(ctx, e.clusterReq("nope", nil))
	wantFailure(t, "viewer cluster Create", err, codes.PermissionDenied)
	cur := e.getCluster(t, cid)
	_, err = clusters.Update(ctx, &mk8spb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()})
	wantFailure(t, "viewer cluster Update", err, codes.PermissionDenied)
	_, err = groups.Create(ctx, e.groupReq(cid, "nope", fixedGroup(1)))
	wantFailure(t, "viewer node group Create", err, codes.PermissionDenied)
	g := e.getGroup(t, gid)
	_, err = groups.Update(ctx, &mk8spb.UpdateNodeGroupRequest{Metadata: g.GetMetadata(), Spec: g.GetSpec()})
	wantFailure(t, "viewer node group Update", err, codes.PermissionDenied)
	_, err = groups.Upgrade(ctx, &mk8spb.UpgradeNodeGroupRequest{Id: gid, UpgradeType: &mk8spb.UpgradeNodeGroupRequest_LatestInfraVersion{LatestInfraVersion: &emptypb.Empty{}}})
	wantFailure(t, "viewer node group Upgrade", err, codes.PermissionDenied)
	_, err = groups.Delete(ctx, &mk8spb.DeleteNodeGroupRequest{Id: gid})
	wantFailure(t, "viewer node group Delete", err, codes.PermissionDenied)
	_, err = clusters.Delete(ctx, &mk8spb.DeleteClusterRequest{Id: cid})
	wantFailure(t, "viewer cluster Delete", err, codes.PermissionDenied)

	// Nothing changed.
	if _, err := e.groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: gid}); err != nil {
		t.Errorf("node group lost after denied calls: %v", err)
	}
}

func TestMK8sEditorCreatesAndUnauthenticatedIsRefused(t *testing.T) {
	t.Parallel()
	e := startMK8sEnforcing(t)
	ctx := t.Context()
	clusters, groups, _ := e.as(t, config.DefaultEditorToken)

	cid := opWait(t)(clusters.Create(ctx, e.clusterReq("by-editor", nil)))
	gid := opWait(t)(groups.Create(ctx, e.groupReq(cid, "pool", fixedGroup(1))))
	opWait(t)(groups.Delete(ctx, &mk8spb.DeleteNodeGroupRequest{Id: gid}))
	opWait(t)(clusters.Delete(ctx, &mk8spb.DeleteClusterRequest{Id: cid}))

	for name, token := range map[string]string{"bogus token": "not-a-token", "no token": ""} {
		c, g, _ := e.as(t, token)
		_, err := c.Get(ctx, &mk8spb.GetClusterRequest{Id: cid})
		wantFailure(t, name+" cluster Get", err, codes.Unauthenticated)
		_, err = c.Create(ctx, e.clusterReq("x", nil))
		wantFailure(t, name+" cluster Create", err, codes.Unauthenticated)
		_, err = g.List(ctx, &mk8spb.ListNodeGroupsRequest{ParentId: cid})
		wantFailure(t, name+" node group List", err, codes.Unauthenticated)
	}
}
