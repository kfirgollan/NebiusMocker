package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	mlflowsvc "github.com/nebius/gosdk/services/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func startMLflowEnforcing(t *testing.T) *mlflowEnv {
	t.Helper()
	return startMLflow(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
}

func (e *mlflowEnv) as(t *testing.T, token string) mlflowsvc.ClusterService {
	t.Helper()
	return mlflowsvc.NewClusterService(e.inst.SDKAs(t, token))
}

func TestMLflowViewerReadsButCannotWrite(t *testing.T) {
	t.Parallel()
	e := startMLflowEnforcing(t)
	ctx := t.Context()
	id := e.create(t, "guarded", e.spec()).GetMetadata().GetId()
	viewer := e.as(t, config.DefaultViewerToken)

	if _, err := viewer.Get(ctx, &mlflowpb.GetClusterRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := viewer.GetByName(ctx, &mlflowpb.GetClusterByNameRequest{ParentId: e.project, Name: "guarded"}); err != nil {
		t.Errorf("viewer GetByName: %v", err)
	}
	if l, err := viewer.List(ctx, &mlflowpb.ListClustersRequest{ParentId: e.project}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("viewer List = %v, %v", l, err)
	}

	_, err := viewer.Create(ctx, e.createReq("nope", e.spec()))
	wantFailure(t, "viewer Create", err, codes.PermissionDenied)
	_, err = viewer.Stop(ctx, &mlflowpb.StopClusterRequest{Id: id})
	wantFailure(t, "viewer Stop", err, codes.PermissionDenied)
	_, err = viewer.Start(ctx, &mlflowpb.StartClusterRequest{Id: id})
	wantFailure(t, "viewer Start", err, codes.PermissionDenied)
	_, err = viewer.Delete(ctx, &mlflowpb.DeleteClusterRequest{Id: id})
	wantFailure(t, "viewer Delete", err, codes.PermissionDenied)

	// Nothing changed.
	if got := mlflowPhase(e.get(t, id)); got != "PHASE_RUNNING/STATE_FINISHED" {
		t.Errorf("cluster after denied calls: %s", got)
	}
	if l, err := e.clusters.List(ctx, &mlflowpb.ListClustersRequest{ParentId: e.project}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("clusters after denied calls = %v, %v", l, err)
	}
}

func TestMLflowEditorManagesClustersAndUnauthenticatedIsRefused(t *testing.T) {
	t.Parallel()
	e := startMLflowEnforcing(t)
	ctx := t.Context()
	editor := e.as(t, config.DefaultEditorToken)
	wait := alphaWait(t)

	id := wait(editor.Create(ctx, e.createReq("by-editor", e.spec())))
	wait(editor.Stop(ctx, &mlflowpb.StopClusterRequest{Id: id}))
	wait(editor.Start(ctx, &mlflowpb.StartClusterRequest{Id: id}))
	wait(editor.Delete(ctx, &mlflowpb.DeleteClusterRequest{Id: id}))
	if _, err := editor.Get(ctx, &mlflowpb.GetClusterRequest{Id: id}); err == nil {
		t.Error("the editor-deleted cluster is still there")
	}

	for name, token := range map[string]string{"bogus token": "not-a-token", "no token": ""} {
		c := e.as(t, token)
		_, err := c.Get(ctx, &mlflowpb.GetClusterRequest{Id: id})
		wantFailure(t, name+" Get", err, codes.Unauthenticated)
		_, err = c.Create(ctx, e.createReq("x", e.spec()))
		wantFailure(t, name+" Create", err, codes.Unauthenticated)
		_, err = c.Stop(ctx, &mlflowpb.StopClusterRequest{Id: id})
		wantFailure(t, name+" Stop", err, codes.Unauthenticated)
	}
}

func TestMLflowAdminAndTargetValidation(t *testing.T) {
	t.Parallel()
	e := startMLflowEnforcing(t)
	ctx := t.Context()
	admin := e.as(t, config.DefaultDevToken)

	id := alphaWait(t)(admin.Create(ctx, e.createReq("by-admin", e.spec())))
	if _, err := admin.Get(ctx, &mlflowpb.GetClusterRequest{Id: id}); err != nil {
		t.Errorf("admin Get: %v", err)
	}
	// The permission target is checked before the request is handled.
	_, err := e.as(t, config.DefaultViewerToken).Create(ctx, &mlflowpb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.other, Name: "elsewhere"}, Spec: e.spec(),
	})
	wantFailure(t, "viewer Create in another project", err, codes.PermissionDenied)
}
