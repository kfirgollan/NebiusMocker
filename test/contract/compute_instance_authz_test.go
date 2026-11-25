package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func TestInstanceAuthz(t *testing.T) {
	t.Parallel()
	c := startInstances(t, testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{platformOutsiderToken: platformOutsiderID}))
	admin := c
	editor := instClientsFor(t, c.env, config.DefaultEditorToken)
	viewer := instClientsFor(t, c.env, config.DefaultViewerToken)
	outsider := instClientsFor(t, c.env, platformOutsiderToken)
	outsider.subnet = c.subnet

	fixture := admin.mustCreate(t, "fixture", c.cpuSpec("fixture"))
	id := fixture.GetMetadata().GetId()

	reads := func(t *testing.T, who string, p instClients, want codes.Code) {
		t.Helper()
		_, err := p.instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: id})
		if want == codes.OK {
			wantCode(t, who+" Get", err, want, "")
		} else if code := codeOf(err); code != codes.NotFound && code != want { // a denied by-id read may hide existence
			t.Errorf("%s Get: %v", who, err)
		}
		_, err = p.instances.List(t.Context(), &computepb.ListInstancesRequest{ParentId: c.project})
		wantCode(t, who+" List", err, want, "")
		_, err = p.instances.ListOperationsByParent(t.Context(), &computepb.ListOperationsByParentRequest{ParentId: c.project})
		wantCode(t, who+" ListOperationsByParent", err, want, "")
		_, err = p.instances.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: c.project, Name: "fixture"})
		if want == codes.OK {
			wantCode(t, who+" GetByName", err, codes.OK, "")
		} else if code := codeOf(err); code != codes.NotFound && code != want {
			t.Errorf("%s GetByName: %v", who, err)
		}
		res, err := p.instances.BatchGet(t.Context(), &computepb.BatchGetRequest{InstanceIds: []string{id}})
		if err != nil {
			t.Fatalf("%s BatchGet: %v", who, err)
		}
		e := res.GetItems()[id].GetError()
		switch {
		case want == codes.OK && res.GetItems()[id].GetInstance() == nil:
			t.Errorf("%s BatchGet entry = %v", who, res.GetItems()[id])
		case want != codes.OK && (e == nil || codes.Code(e.GetCode()) != want):
			t.Errorf("%s BatchGet entry = %v, want status %v", who, res.GetItems()[id], want)
		}
	}
	writes := func(t *testing.T, who string, p instClients, want codes.Code) {
		t.Helper()
		_, err := p.create(t.Context(), "w-"+who, p.cpuSpec("w-"+who))
		wantCode(t, who+" Create", err, want, "")
		err = p.update(t.Context(), t, fixture, func(s *computepb.InstanceSpec) { s.Hostname = "h-" + who })
		wantCode(t, who+" Update", err, want, "")
		wantCode(t, who+" Stop", p.stop(t.Context(), id), want, "")
		wantCode(t, who+" Start", p.start(t.Context(), id), want, "")
		wantCode(t, who+" Delete", p.del(t.Context(), id), want, "")
	}

	func() {
		reads(t, "viewer", viewer, codes.OK)
		writes(t, "viewer", viewer, codes.PermissionDenied)
	}()
	func() {
		reads(t, "outsider", outsider, codes.PermissionDenied)
		writes(t, "outsider", outsider, codes.PermissionDenied)
	}()
	if got := admin.get(t, id); got.GetStatus().GetState() != computepb.InstanceStatus_RUNNING || got.GetSpec().GetHostname() != "" {
		t.Fatalf("denied calls changed the instance: %v", got)
	}
	func() {
		reads(t, "editor", editor, codes.OK)
		own := editor.mustCreate(t, "by-editor", editor.cpuSpec("by-editor"))
		oid := own.GetMetadata().GetId()
		editor.mustUpdate(t, own, func(s *computepb.InstanceSpec) { s.Hostname = "ed" })
		if err := editor.stop(t.Context(), oid); err != nil {
			t.Errorf("editor Stop: %v", err)
		}
		if err := editor.start(t.Context(), oid); err != nil {
			t.Errorf("editor Start: %v", err)
		}
		editor.mustDelete(t, oid)
	}()
	func() {
		reads(t, "admin", admin, codes.OK)
		if err := admin.stop(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if err := admin.start(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		admin.mustDelete(t, id)
	}()
}

var _ = computesvc.NewInstanceService
