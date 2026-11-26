package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	tunnelsvc "github.com/nebius/gosdk/services/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/tunnel"
)

const (
	tunnelAgentToken = "tunnel-agent-token"
	tunnelAgentID    = "serviceaccount-e00mock0000000tunagent"
)

func startTunnel(t *testing.T) *testkit.Instance {
	t.Helper()
	return testkit.StartT(t, []mocker.Module{iam.New(), tunnel.New()},
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{tunnelAgentToken: tunnelAgentID}),
	)
}

func tunnelAs(t *testing.T, inst *testkit.Instance, token string) tunnelsvc.TunnelService {
	t.Helper()
	return tunnelsvc.NewTunnelService(inst.SDKAs(t, token))
}

func tunnelCreate(t *testing.T, svc tunnelsvc.TunnelService, parent, title string, labels map[string]string) string {
	t.Helper()
	op, err := svc.Create(t.Context(), &tunnelpb.CreateTunnelRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Labels: labels},
		Spec:     &tunnelpb.TunnelSpec{Title: title, Description: "desc of " + title},
	})
	return mustOp(t, op, err)
}

func tunnelGet(t *testing.T, svc tunnelsvc.TunnelService, id string) *tunnelpb.Tunnel {
	t.Helper()
	got, err := svc.Get(t.Context(), &tunnelpb.GetTunnelRequest{Id: id})
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return got
}

func TestTunnelCRUD(t *testing.T) {
	t.Parallel()
	inst := startTunnel(t)
	svc := tunnelAs(t, inst, config.DefaultDevToken)
	ctx := t.Context()

	id := tunnelCreate(t, svc, inst.ProjectID, "office", map[string]string{"env": "dev"})
	got := tunnelGet(t, svc, id)
	md := got.GetMetadata()
	if md.GetId() != id || md.GetParentId() != inst.ProjectID || md.GetResourceVersion() != 1 || md.GetLabels()["env"] != "dev" {
		t.Errorf("metadata = %v", md)
	}
	if md.GetCreatedAt() == nil || md.GetUpdatedAt() == nil {
		t.Errorf("timestamps not set: %v", md)
	}
	if got.GetSpec().GetTitle() != "office" || got.GetSpec().GetDescription() != "desc of office" {
		t.Errorf("spec = %v", got.GetSpec())
	}
	st := got.GetStatus()
	if st.GetState() != tunnelpb.TunnelStatus_CREATED || st.GetConnectionState() != tunnelpb.ConnectionState_CONNECTION_STATE_DISCONNECTED || len(st.GetServices()) != 0 {
		t.Errorf("status = %v", st)
	}

	// Tunnels are unnamed: several with the same (empty) name coexist.
	id2 := tunnelCreate(t, svc, inst.ProjectID, "second", nil)
	id3 := tunnelCreate(t, svc, inst.ProjectID, "third", nil)

	all, err := svc.List(ctx, &tunnelpb.ListTunnelRequest{ParentId: inst.ProjectID})
	if err != nil || len(all.GetItems()) != 3 {
		t.Fatalf("List = %d items, %v; want 3", len(all.GetItems()), err)
	}
	seen := map[string]bool{}
	token := ""
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatal("pagination did not terminate")
		}
		page, err := svc.List(ctx, &tunnelpb.ListTunnelRequest{ParentId: inst.ProjectID, PageSize: 1, PageToken: token})
		if err != nil || len(page.GetItems()) != 1 {
			t.Fatalf("List page = %d items, %v; want 1", len(page.GetItems()), err)
		}
		seen[page.GetItems()[0].GetMetadata().GetId()] = true
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if !seen[id] || !seen[id2] || !seen[id3] || len(seen) != 3 {
		t.Errorf("paginated ids = %v, want %s %s %s", seen, id, id2, id3)
	}

	// Update in place.
	cur := tunnelGet(t, svc, id)
	cur.Metadata.Labels = map[string]string{"env": "prod", "team": "net"}
	cur.Spec.Title = "office-2"
	cur.Spec.Description = "changed"
	op, err := svc.Update(ctx, &tunnelpb.UpdateTunnelRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()})
	mustOp(t, op, err)
	upd := tunnelGet(t, svc, id)
	if upd.GetMetadata().GetResourceVersion() != 2 || upd.GetSpec().GetTitle() != "office-2" ||
		upd.GetSpec().GetDescription() != "changed" || upd.GetMetadata().GetLabels()["team"] != "net" {
		t.Errorf("after update = %v", upd)
	}

	// A no-op update keeps the resource_version.
	op, err = svc.Update(ctx, &tunnelpb.UpdateTunnelRequest{Metadata: upd.GetMetadata(), Spec: upd.GetSpec()})
	mustOp(t, op, err)
	if v := tunnelGet(t, svc, id).GetMetadata().GetResourceVersion(); v != 2 {
		t.Errorf("no-op update resource_version = %d, want 2", v)
	}

	// X-ResetMask clears the description.
	cur = tunnelGet(t, svc, id)
	cur.Spec.Description = ""
	op, err = svc.Update(resetCtx(ctx, t, "spec.description"), &tunnelpb.UpdateTunnelRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()})
	mustOp(t, op, err)
	cleared := tunnelGet(t, svc, id)
	if cleared.GetSpec().GetDescription() != "" || cleared.GetSpec().GetTitle() != "office-2" || cleared.GetMetadata().GetResourceVersion() != 3 {
		t.Errorf("after reset = %v", cleared)
	}

	// Delete, then Get is NotFound.
	op, err = svc.Delete(ctx, &tunnelpb.DeleteTunnelRequest{Id: id})
	mustOp(t, op, err)
	_, err = svc.Get(ctx, &tunnelpb.GetTunnelRequest{Id: id})
	assertCode(t, err, codes.NotFound)
	if _, err = svc.Get(ctx, &tunnelpb.GetTunnelRequest{Id: id2}); err != nil {
		t.Errorf("other tunnel after delete: %v", err)
	}
}

func TestTunnelErrors(t *testing.T) {
	t.Parallel()
	inst := startTunnel(t)
	svc := tunnelAs(t, inst, config.DefaultDevToken)
	ctx := t.Context()
	id := tunnelCreate(t, svc, inst.ProjectID, "existing", nil)

	_, err := svc.Get(ctx, &tunnelpb.GetTunnelRequest{Id: "tunnel-e00test000000000404"})
	assertCode(t, err, codes.NotFound)

	_, err = svc.Create(ctx, &tunnelpb.CreateTunnelRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID},
		Spec:     &tunnelpb.TunnelSpec{Title: strings.Repeat("a", 257)},
	})
	assertCode(t, err, codes.InvalidArgument)

	_, err = svc.Create(ctx, &tunnelpb.CreateTunnelRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00test000000000404"},
		Spec:     &tunnelpb.TunnelSpec{Title: "x"},
	})
	assertCode(t, err, codes.NotFound)

	_, err = svc.List(ctx, &tunnelpb.ListTunnelRequest{ParentId: inst.ProjectID, Filter: "title = 'x'"})
	assertCode(t, err, codes.InvalidArgument)

	cur := tunnelGet(t, svc, id)
	op, err := svc.Update(ctx, &tunnelpb.UpdateTunnelRequest{Metadata: cur.GetMetadata(), Spec: &tunnelpb.TunnelSpec{Title: "v2"}})
	mustOp(t, op, err)

	// cur now carries a stale resource_version: errs.Conflict maps to Aborted.
	_, err = svc.Update(ctx, &tunnelpb.UpdateTunnelRequest{Metadata: cur.GetMetadata(), Spec: &tunnelpb.TunnelSpec{Title: "v3"}})
	assertCode(t, err, codes.Aborted)

	moved := tunnelGet(t, svc, id)
	moved.Metadata.ParentId = "project-e00test000000000002"
	_, err = svc.Update(ctx, &tunnelpb.UpdateTunnelRequest{Metadata: moved.GetMetadata(), Spec: moved.GetSpec()})
	assertCode(t, err, codes.InvalidArgument)
}

func TestTunnelRoles(t *testing.T) {
	t.Parallel()
	inst := startTunnel(t)
	admin := tunnelAs(t, inst, config.DefaultDevToken)
	fixtures := map[string]string{} // one per role: the roles run in parallel
	for _, n := range []string{"admin", "editor", "viewer"} {
		fixtures[n] = tunnelCreate(t, admin, inst.ProjectID, "fixture-"+n, nil)
	}

	roles := []struct {
		name, token string
		write       bool
	}{
		{"admin", config.DefaultDevToken, true},
		{"editor", config.DefaultEditorToken, true},
		{"viewer", config.DefaultViewerToken, false},
	}
	for _, role := range roles {
		t.Run(role.name, func(t *testing.T) {
			t.Parallel()
			svc := tunnelAs(t, inst, role.token)
			ctx := t.Context()
			fixture := fixtures[role.name]
			if _, err := svc.Get(ctx, &tunnelpb.GetTunnelRequest{Id: fixture}); err != nil {
				t.Errorf("Get: %v", err)
			}
			if _, err := svc.List(ctx, &tunnelpb.ListTunnelRequest{ParentId: inst.ProjectID}); err != nil {
				t.Errorf("List: %v", err)
			}
			cur := tunnelGet(t, svc, fixture)
			_, createErr := svc.Create(ctx, &tunnelpb.CreateTunnelRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID},
				Spec:     &tunnelpb.TunnelSpec{Title: "by-" + role.name},
			})
			_, updateErr := svc.Update(ctx, &tunnelpb.UpdateTunnelRequest{
				Metadata: cur.GetMetadata(), Spec: &tunnelpb.TunnelSpec{Title: "edited-by-" + role.name},
			})
			if !role.write {
				assertCode(t, createErr, codes.PermissionDenied)
				assertCode(t, updateErr, codes.PermissionDenied)
				_, err := svc.Delete(ctx, &tunnelpb.DeleteTunnelRequest{Id: fixture})
				assertCode(t, err, codes.PermissionDenied)
				return
			}
			if createErr != nil || updateErr != nil {
				t.Fatalf("Create = %v, Update = %v; want success", createErr, updateErr)
			}
			own := tunnelCreate(t, svc, inst.ProjectID, "own-"+role.name, nil)
			op, err := svc.Delete(ctx, &tunnelpb.DeleteTunnelRequest{Id: own})
			mustOp(t, op, err)
		})
	}
}

// TestTunnelAgentRole follows docs.nebius.com/tunnels/connect: a service
// account in a group holding applicationtunnel.agent on one tunnel can read
// that tunnel and nothing else.
func TestTunnelAgentRole(t *testing.T) {
	t.Parallel()
	inst := startTunnel(t)
	adminSDK := inst.SDKAs(t, config.DefaultDevToken)
	admin := tunnelsvc.NewTunnelService(adminSDK)
	ctx := t.Context()
	target := tunnelCreate(t, admin, inst.ProjectID, "target", nil)
	other := tunnelCreate(t, admin, inst.ProjectID, "other", nil)

	agent := tunnelAs(t, inst, tunnelAgentToken)
	_, err := agent.Get(ctx, &tunnelpb.GetTunnelRequest{Id: target})
	tunnelDenied(t, err)

	groupOp, err := iamsvc.NewGroupService(adminSDK).Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: "tunnel-agents"},
		Spec:     &iampb.GroupSpec{},
	})
	gid := mustOp(t, groupOp, err)
	memOp, err := iamsvc.NewGroupMembershipService(adminSDK).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.GroupMembershipSpec{MemberId: tunnelAgentID},
	})
	mustOp(t, memOp, err)
	permitOp, err := iamsvc.NewAccessPermitService(adminSDK).Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.AccessPermitSpec{ResourceId: target, Role: authz.RoleTunnelAgent},
	})
	mustOp(t, permitOp, err)

	if _, err := agent.Get(ctx, &tunnelpb.GetTunnelRequest{Id: target}); err != nil {
		t.Fatalf("agent Get of its tunnel: %v", err)
	}
	_, err = agent.Get(ctx, &tunnelpb.GetTunnelRequest{Id: other})
	tunnelDenied(t, err)
	cur := tunnelGet(t, admin, target)
	_, err = agent.Update(ctx, &tunnelpb.UpdateTunnelRequest{Metadata: cur.GetMetadata(), Spec: &tunnelpb.TunnelSpec{Title: "hijack"}})
	assertCode(t, err, codes.PermissionDenied)
	_, err = agent.Delete(ctx, &tunnelpb.DeleteTunnelRequest{Id: target})
	assertCode(t, err, codes.PermissionDenied)
	if got := tunnelGet(t, admin, target); got.GetSpec().GetTitle() != "target" {
		t.Errorf("tunnel changed by the agent: %v", got.GetSpec())
	}
}

// tunnelDenied accepts the two codes enforce uses to refuse a read: callers
// who cannot see a resource may be told NotFound instead of PermissionDenied.
func tunnelDenied(t *testing.T, err error) {
	t.Helper()
	if c := status.Code(err); c != codes.PermissionDenied && c != codes.NotFound {
		t.Fatalf("code = %v, want PermissionDenied or NotFound (err: %v)", c, err)
	}
}
