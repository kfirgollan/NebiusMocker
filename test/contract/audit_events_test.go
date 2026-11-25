package contract

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	auditsvc "github.com/nebius/gosdk/services/nebius/audit/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

const auditRegion = "eu-north1"

// auditEnv is an instance with the IAM and audit modules, driven as the dev admin.
type auditEnv struct {
	inst    *testkit.Instance
	groups  iamsvc.GroupService
	events  auditsvc.AuditEventService
	exports auditsvc.AuditEventExportService
	// bucketID is the Object Storage bucket exports go to, once addExportBucket ran.
	bucketID string
}

func startAudit(t *testing.T, opts ...testkit.Option) *auditEnv {
	t.Helper()
	return startAuditWith(t, nil, opts...)
}

// startAuditWith also starts the extra modules.
func startAuditWith(t *testing.T, extra []mocker.Module, opts ...testkit.Option) *auditEnv {
	t.Helper()
	modules := append([]mocker.Module{iam.New(), audit.New()}, extra...)
	inst := testkit.StartT(t, modules, opts...)
	sdk := inst.SDK(t)
	return &auditEnv{
		inst:    inst,
		groups:  iamsvc.NewGroupService(sdk),
		events:  auditsvc.NewAuditEventService(sdk),
		exports: auditsvc.NewAuditEventExportService(sdk),
	}
}

// auditWindow is [now-1h, now+1h), wide enough to hold every event of a test.
func auditWindow() (start, end *timestamppb.Timestamp) {
	now := clock.Real{}.Now()
	return timestamppb.New(now.Add(-time.Hour)), timestamppb.New(now.Add(time.Hour))
}

func auditListReq(parentID, filter string) *auditv2pb.ListAuditEventRequest {
	start, end := auditWindow()
	return &auditv2pb.ListAuditEventRequest{
		ParentId: parentID, Filter: filter, Start: start, End: end,
		Region: auditRegion, PageSize: 500,
	}
}

// listEvents returns every event under parentID matching filter, oldest first.
func listEvents(t *testing.T, svc auditsvc.AuditEventService, parentID, filter string) []*auditv2pb.AuditEvent {
	t.Helper()
	resp, err := svc.List(t.Context(), auditListReq(parentID, filter))
	if err != nil {
		t.Fatalf("List(parent=%s, filter=%q): %v", parentID, filter, err)
	}
	if resp.GetNextPageToken() != "" {
		t.Fatalf("List(filter=%q): unexpected next_page_token with page_size 500", filter)
	}
	assertNoRawTokens(t, resp.GetItems())
	return resp.GetItems()
}

func (e *auditEnv) list(t *testing.T, filter string) []*auditv2pb.AuditEvent {
	t.Helper()
	return listEvents(t, e.events, e.inst.TenantID, filter)
}

// waitEvents polls until the filter yields want events: operation completion
// events of delayed operations are recorded asynchronously.
func waitEvents(t *testing.T, svc auditsvc.AuditEventService, parentID, filter string, want int) []*auditv2pb.AuditEvent {
	t.Helper()
	var got []*auditv2pb.AuditEvent
	for range 100 {
		got = listEvents(t, svc, parentID, filter)
		if len(got) == want {
			return got
		}
		<-clock.Real{}.After(50 * time.Millisecond)
	}
	t.Fatalf("List(filter=%q) returned %d events, want %d", filter, len(got), want)
	return nil
}

func eventIDs(evs []*auditv2pb.AuditEvent) []string {
	ids := make([]string, 0, len(evs))
	for _, ev := range evs {
		ids = append(ids, ev.GetId())
	}
	return ids
}

// TestAuditEventsFromGroupLifecycle checks the events of a real create, update
// and delete made through the IAM SDK.
func TestAuditEventsFromGroupLifecycle(t *testing.T) {
	t.Parallel()
	env := startAudit(t, testkit.WithAuthz(authz.ModeEnforce))
	ctx := t.Context()
	tenant := env.inst.TenantID

	g := createGroup(t, env.groups, tenant, "audited", nil)
	id := g.GetMetadata().GetId()
	op, err := env.groups.Update(ctx, &iampb.UpdateGroupRequest{Metadata: &commonpb.ResourceMetadata{
		Id: id, ParentId: tenant, Name: "audited", Labels: map[string]string{"env": "test"},
	}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Update wait: %v", err)
	}
	op, err = env.groups.Delete(ctx, &iampb.DeleteGroupRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Delete wait: %v", err)
	}

	evs := env.list(t, "resource.metadata.id='"+id+"'")
	actions := []string{"CREATE", "UPDATE", "DELETE"}
	methods := map[string]string{"CREATE": "Create", "UPDATE": "Update", "DELETE": "Delete"}
	if len(evs) != len(actions) {
		t.Fatalf("got %d events, want %d: %v", len(evs), len(actions), evs)
	}
	start, end := auditWindow()
	seen := map[string]bool{}
	for _, ev := range evs {
		if seen[ev.GetId()] || ev.GetId() == "" {
			t.Errorf("event id %q is empty or duplicated", ev.GetId())
		}
		seen[ev.GetId()] = true
	}
	for i, ev := range evs {
		action := actions[i]
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			if got := ev.GetAction(); got != action {
				t.Errorf("action = %q, want %q", got, action)
			}
			if got := ev.GetStatus(); got != auditv2pb.Status_DONE {
				t.Errorf("status = %v, want DONE", got)
			}
			if got := ev.GetService().GetName(); got != "IAM" {
				t.Errorf("service.name = %q, want IAM", got)
			}
			if got, want := ev.GetType(), "ai.nebius.iam.group."+strings.ToLower(action); got != want {
				t.Errorf("type = %q, want %q", got, want)
			}
			if got, want := ev.GetSource(), "nebius.iam.v1.GroupService/"+methods[action]; got != want {
				t.Errorf("source = %q, want %q", got, want)
			}
			authn := ev.GetAuthentication()
			if got := authn.GetSubject().GetTenantUserId(); got != config.DefaultDevPrincipal {
				t.Errorf("tenant_user_id = %q, want %q", got, config.DefaultDevPrincipal)
			}
			if !authn.GetAuthenticated() {
				t.Error("authenticated = false, want true")
			}
			if tok := authn.GetTokenCredential().GetMaskedToken(); tok == "" || tok == config.DefaultDevToken {
				t.Errorf("masked_token = %q, want a masked, non-empty token", tok)
			}
			md := ev.GetResource().GetMetadata()
			if md.GetId() != id || md.GetName() != "audited" || md.GetType() != "group" {
				t.Errorf("resource.metadata = %v, want id=%s name=audited type=group", md, id)
			}
			h := ev.GetResource().GetHierarchy()
			if len(h) == 0 || h[0].GetId() != tenant || h[0].GetName() != "default" {
				t.Errorf("hierarchy = %v, want tenant %s named default first", h, tenant)
			}
			if ts := ev.GetTime().AsTime(); ts.Before(start.AsTime()) || !ts.Before(end.AsTime()) {
				t.Errorf("time = %v, outside the test window", ts)
			}
			if got := ev.GetResponse().GetStatusCode(); got != code.Code_OK {
				t.Errorf("response.status_code = %v, want OK", got)
			}
			params, err := protojson.Marshal(ev.GetRequest().GetParameters())
			if err != nil {
				t.Fatalf("marshal parameters: %v", err)
			}
			if action != "DELETE" && !strings.Contains(string(params), `"audited"`) {
				t.Errorf("request.parameters = %s, want the group name", params)
			}
			if action == "DELETE" && !strings.Contains(string(params), id) {
				t.Errorf("request.parameters = %s, want the group id", params)
			}
		})
	}
}

// TestAuditEventsForDelayedOperation checks that an operation which is still
// running when the RPC returns yields STARTED, then DONE.
func TestAuditEventsForDelayedOperation(t *testing.T) {
	t.Parallel()
	env := startAudit(t, testkit.WithOperationDelay(200*time.Millisecond))
	g := createGroup(t, env.groups, env.inst.TenantID, "slow", nil)
	filter := "resource.metadata.id='" + g.GetMetadata().GetId() + "'"

	evs := waitEvents(t, env.events, env.inst.TenantID, filter, 2)
	got := []auditv2pb.Status{evs[0].GetStatus(), evs[1].GetStatus()}
	if want := []auditv2pb.Status{auditv2pb.Status_STARTED, auditv2pb.Status_DONE}; !slices.Equal(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
	for _, ev := range evs {
		if ev.GetAction() != "CREATE" {
			t.Errorf("action = %q, want CREATE", ev.GetAction())
		}
	}
	if evs[0].GetId() == evs[1].GetId() {
		t.Errorf("STARTED and DONE share event id %q", evs[0].GetId())
	}
}

func TestAuditReadsOffByDefault(t *testing.T) {
	t.Parallel()
	env := startAudit(t)
	g := createGroup(t, env.groups, env.inst.TenantID, "readme", nil) // Create + Get
	if _, err := env.groups.List(t.Context(), &iampb.ListGroupsRequest{ParentId: env.inst.TenantID}); err != nil {
		t.Fatalf("List groups: %v", err)
	}
	evs := env.list(t, "resource.metadata.id='"+g.GetMetadata().GetId()+"'")
	if len(evs) != 1 || evs[0].GetAction() != "CREATE" {
		t.Errorf("events = %v, want just CREATE", evs)
	}
	for _, action := range []string{"GET", "LIST"} {
		if got := env.list(t, "action='"+action+"'"); len(got) != 0 {
			t.Errorf("%s events = %d, want 0 without WithAuditReads", action, len(got))
		}
	}
}

func TestAuditReadsRecordedWhenEnabled(t *testing.T) {
	t.Parallel()
	env := startAudit(t, testkit.WithAuditReads())
	g := createGroup(t, env.groups, env.inst.TenantID, "readme", nil) // Create + Get
	if _, err := env.groups.List(t.Context(), &iampb.ListGroupsRequest{ParentId: env.inst.TenantID}); err != nil {
		t.Fatalf("List groups: %v", err)
	}
	if got := env.list(t, "action='GET' AND resource.metadata.id='"+g.GetMetadata().GetId()+"'"); len(got) != 1 {
		t.Errorf("GET events = %d, want 1", len(got))
	}
	if got := env.list(t, "action='LIST' AND service.name='IAM'"); len(got) != 1 {
		t.Errorf("LIST events = %d, want 1", len(got))
	}
}
