package contract

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/kms"
)

var filterGroupNames = []string{"alpha-1", "alpha-2", "beta-1", "beta-2", "gamma"}

// startFilterFixture creates five groups, one CREATE event each.
func startFilterFixture(t *testing.T) (env *auditEnv, ids map[string]string) {
	t.Helper()
	env = startAudit(t)
	ids = map[string]string{}
	for _, name := range filterGroupNames {
		ids[name] = createGroup(t, env.groups, env.inst.TenantID, name, nil).GetMetadata().GetId()
	}
	return env, ids
}

func eventNames(evs []*auditv2pb.AuditEvent) []string {
	names := make([]string, 0, len(evs))
	for _, ev := range evs {
		names = append(names, ev.GetResource().GetMetadata().GetName())
	}
	slices.Sort(names)
	return names
}

func without(all []string, drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(all), func(s string) bool { return slices.Contains(drop, s) })
}

func TestAuditFilters(t *testing.T) {
	t.Parallel()
	env, ids := startFilterFixture(t)
	all := slices.Clone(filterGroupNames)
	none := []string{}

	// A masked token, as the filter documentation says, is taken from an event.
	masked := env.list(t, "")[0].GetAuthentication().GetTokenCredential().GetMaskedToken()
	if masked == "" {
		t.Fatal("events carry no masked_token")
	}
	tenant := env.inst.TenantID

	tests := []struct {
		name, filter string
		want         []string
	}{
		{"empty filter", "", all},
		{"equals", "resource.metadata.name='alpha-1'", []string{"alpha-1"}},
		{"not equals", "resource.metadata.name!='alpha-1'", without(all, "alpha-1")},
		{"contains", "resource.metadata.name:'alpha'", []string{"alpha-1", "alpha-2"}},
		{"regex", "regex(resource.metadata.name, '^beta-[0-9]$')", []string{"beta-1", "beta-2"}},
		{"regex no match", "regex(resource.metadata.name, '^zzz')", none},
		{"name alias", "resource.name='gamma'", []string{"gamma"}},
		{"action", "action='CREATE'", all},
		{"action not equals", "action!='CREATE'", none},
		{"action other", "action='DELETE'", none},
		{"type", "type='ai.nebius.iam.group.create'", all},
		{"type contains", "type:'iam.group'", all},
		{"status done", "status='DONE'", all},
		{"status error", "status='ERROR'", none},
		{"status started", "status='STARTED'", none},
		{"service name", "service.name='IAM'", all},
		{"service name other", "service.name='COMPUTE'", none},
		{"service name not equals", "service.name!='IAM'", none},
		{"subject name", "authentication.subject.name='" + config.DefaultDevPrincipal + "'", all},
		{"subject name contains", "authentication.subject.name:'devadmin'", all},
		{"tenant user id", "authentication.subject.tenant_user_id='" + config.DefaultDevPrincipal + "'", all},
		{"other tenant user id", "authentication.subject.tenant_user_id='tenantuseraccount-e00other'", none},
		{"service account id", "authentication.subject.service_account_id='" + config.DefaultDevPrincipal + "'", none},
		{"masked token", "authentication.token_credential.masked_token='" + masked + "'", all},
		{"masked token not equals", "authentication.token_credential.masked_token!='" + masked + "'", none},
		{"static key id", "authentication.static_key_credential.id='statickey-e00none'", none},
		{"static key id contains", "authentication.static_key_credential.id:'statickey'", none},
		{"resource id", "resource.metadata.id='" + ids["beta-2"] + "'", []string{"beta-2"}},
		{"resource type", "resource.metadata.type='group'", all},
		{"resource type other", "resource.metadata.type='disk'", none},
		{"hierarchy id", "resource.hierarchy.id='" + tenant + "'", all},
		{"hierarchy id contains", "resource.hierarchy.id:'tenant-'", all},
		{"hierarchy name", "resource.hierarchy.name='default'", all},
		{"hierarchy name not equals", "resource.hierarchy.name!='default'", none},
		{"and", "service.name='IAM' AND resource.metadata.name:'beta' AND action='CREATE'", []string{"beta-1", "beta-2"}},
		{"and with regex", "regex(resource.metadata.name, '^[a-z]+-[0-9]$') AND resource.metadata.name!='alpha-1'", []string{"alpha-2", "beta-1", "beta-2"}},
		{"and disjoint", "resource.metadata.name='alpha-1' AND resource.metadata.name='alpha-2'", none},
		{"lowercase and", "action='CREATE' and resource.metadata.name='gamma'", []string{"gamma"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := eventNames(env.list(t, tt.filter))
			if !slices.Equal(got, tt.want) {
				t.Errorf("filter %q matched %v, want %v", tt.filter, got, tt.want)
			}
		})
	}
}

// TestAuditDataPlaneRejected checks that DATA_PLANE is refused by name, not
// answered with an empty list.
func TestAuditDataPlaneRejected(t *testing.T) {
	t.Parallel()
	env, _ := startFilterFixture(t)
	req := auditListReq(env.inst.TenantID, "")
	req.EventType = auditv2pb.EventType_DATA_PLANE
	_, err := env.events.List(t.Context(), req)
	assertCode(t, err, codes.InvalidArgument)
	if !strings.Contains(err.Error(), "DATA_PLANE") {
		t.Errorf("err = %v, want it to name DATA_PLANE", err)
	}
}

func TestAuditFilterEventType(t *testing.T) {
	t.Parallel()
	env, _ := startFilterFixture(t)
	for _, tt := range []struct {
		eventType auditv2pb.EventType
		want      int
	}{
		{auditv2pb.EventType_EVENT_TYPE_UNSPECIFIED, len(filterGroupNames)},
		{auditv2pb.EventType_CONTROL_PLANE, len(filterGroupNames)},
	} {
		t.Run(tt.eventType.String(), func(t *testing.T) {
			t.Parallel()
			req := auditListReq(env.inst.TenantID, "")
			req.EventType = tt.eventType
			resp, err := env.events.List(t.Context(), req)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if got := len(resp.GetItems()); got != tt.want {
				t.Errorf("event_type %v returned %d events, want %d", tt.eventType, got, tt.want)
			}
		})
	}
}

// TestAuditFilterStatusError checks that a failed call is an ERROR event
// carrying the error code.
func TestAuditFilterStatusError(t *testing.T) {
	t.Parallel()
	env := startAudit(t)
	createGroup(t, env.groups, env.inst.TenantID, "dup", nil)
	_, err := env.groups.Create(t.Context(), createReq(env.inst.TenantID, "dup"))
	assertCode(t, err, codes.AlreadyExists)

	evs := env.list(t, "status='ERROR'")
	if len(evs) != 1 {
		t.Fatalf("ERROR events = %d, want 1: %v", len(evs), evs)
	}
	if got := evs[0].GetResponse().GetStatusCode().String(); got != "ALREADY_EXISTS" {
		t.Errorf("response.status_code = %s, want ALREADY_EXISTS", got)
	}
	if got := evs[0].GetAction(); got != "CREATE" {
		t.Errorf("action = %q, want CREATE", got)
	}
	if got := len(env.list(t, "status='DONE'")); got != 1 {
		t.Errorf("DONE events = %d, want 1", got)
	}
}

func TestAuditTimeRange(t *testing.T) {
	t.Parallel()
	env, _ := startFilterFixture(t)
	evs := env.list(t, "")
	first, last := evs[0], evs[len(evs)-1]
	lastTime := last.GetTime().AsTime()
	in := func(start, end time.Time) []string {
		t.Helper()
		req := auditListReq(env.inst.TenantID, "")
		req.Start, req.End = timestamppb.New(start), timestamppb.New(end)
		resp, err := env.events.List(t.Context(), req)
		if err != nil {
			t.Fatalf("List [%v, %v): %v", start, end, err)
		}
		return eventIDs(resp.GetItems())
	}
	now := clock.Real{}.Now()

	if got := in(now.Add(-3*time.Hour), now.Add(-2*time.Hour)); len(got) != 0 {
		t.Errorf("window before the events returned %d events, want 0", len(got))
	}
	if got := in(now.Add(2*time.Hour), now.Add(3*time.Hour)); len(got) != 0 {
		t.Errorf("window after the events returned %d events, want 0", len(got))
	}
	// start is inclusive, end is exclusive.
	if got := in(first.GetTime().AsTime(), now.Add(time.Hour)); !slices.Contains(got, first.GetId()) {
		t.Errorf("start == first event time: %v lacks the first event %s", got, first.GetId())
	}
	if got := in(lastTime, now.Add(time.Hour)); !slices.Contains(got, last.GetId()) {
		t.Errorf("start == last event time: %v lacks the last event %s", got, last.GetId())
	}
	if got := in(lastTime.Add(time.Nanosecond), now.Add(time.Hour)); slices.Contains(got, last.GetId()) {
		t.Errorf("start just after the last event still returned it: %v", got)
	}
	if got := in(now.Add(-time.Hour), lastTime); slices.Contains(got, last.GetId()) {
		t.Errorf("end == last event time returned the last event: %v", got)
	}
	if got := in(now.Add(-time.Hour), lastTime.Add(time.Nanosecond)); !slices.Contains(got, last.GetId()) {
		t.Errorf("end just after the last event lacks it: %v", got)
	}
}

func TestAuditPaging(t *testing.T) {
	t.Parallel()
	env, _ := startFilterFixture(t)
	ctx := t.Context()
	want := eventIDs(env.list(t, ""))
	if len(want) != 5 {
		t.Fatalf("fixture events = %d, want 5", len(want))
	}

	var got []string
	var sizes []int
	var tokens []string
	token := ""
	for range 10 {
		req := auditListReq(env.inst.TenantID, "")
		req.PageSize, req.PageToken = 2, token
		resp, err := env.events.List(ctx, req)
		if err != nil {
			t.Fatalf("List page: %v", err)
		}
		sizes = append(sizes, len(resp.GetItems()))
		tokens = append(tokens, resp.GetNextPageToken())
		got = append(got, eventIDs(resp.GetItems())...)
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("page sizes = %v, want [2 2 1]", sizes)
	}
	if tokens[0] == "" || tokens[1] == "" || tokens[0] == tokens[1] || tokens[2] != "" {
		t.Errorf("next_page_tokens = %q, want two distinct non-empty tokens then empty", tokens)
	}
	if !slices.Equal(got, want) {
		t.Errorf("paged ids = %v, want %v", got, want)
	}

	// gosdk's Filter iterator follows the tokens itself.
	req := auditListReq(env.inst.TenantID, "")
	req.PageSize = 2
	var viaIter []string
	for ev, err := range env.events.Filter(ctx, req) {
		if err != nil {
			t.Fatalf("Filter iterator: %v", err)
		}
		viaIter = append(viaIter, ev.GetId())
	}
	if !slices.Equal(viaIter, want) {
		t.Errorf("Filter iterator ids = %v, want %v", viaIter, want)
	}

	// A filter applies before paging.
	req = auditListReq(env.inst.TenantID, "resource.metadata.name:'a'")
	req.PageSize = 1
	n := 0
	for _, err := range env.events.Filter(ctx, req) {
		if err != nil {
			t.Fatalf("Filter iterator: %v", err)
		}
		n++
	}
	if n != 5 { // alpha-1, alpha-2, beta-1, beta-2, gamma all contain an 'a'
		t.Errorf("filtered iteration yielded %d events, want 5", n)
	}
}

func TestAuditListInvalidArguments(t *testing.T) {
	t.Parallel()
	env := startAudit(t)
	tenant := env.inst.TenantID
	now := clock.Real{}.Now()

	mod := func(fn func(*auditv2pb.ListAuditEventRequest)) *auditv2pb.ListAuditEventRequest {
		req := auditListReq(tenant, "")
		fn(req)
		return req
	}
	tests := []struct {
		name string
		req  *auditv2pb.ListAuditEventRequest
	}{
		{"unsupported operator", auditListReq(tenant, "action ~ 'x'")},
		{"unknown field", auditListReq(tenant, "bogus.field='x'")},
		{"unterminated string", auditListReq(tenant, "action='CREATE")},
		{"bad regex", auditListReq(tenant, "regex(action, '(')")},
		{"missing value", auditListReq(tenant, "action=")},
		{"trailing and", auditListReq(tenant, "action='CREATE' AND")},
		{"bad page token", mod(func(r *auditv2pb.ListAuditEventRequest) { r.PageToken = "not-a-token!" })},
		{"well-formed but foreign token", mod(func(r *auditv2pb.ListAuditEventRequest) { r.PageToken = "AAAA" })},
		{"end equals start", mod(func(r *auditv2pb.ListAuditEventRequest) {
			r.Start, r.End = timestamppb.New(now), timestamppb.New(now)
		})},
		{"end before start", mod(func(r *auditv2pb.ListAuditEventRequest) {
			r.Start, r.End = timestamppb.New(now), timestamppb.New(now.Add(-time.Hour))
		})},
		{"page size too large", mod(func(r *auditv2pb.ListAuditEventRequest) { r.PageSize = 501 })},
		{"negative page size", mod(func(r *auditv2pb.ListAuditEventRequest) { r.PageSize = -1 })},
		{"missing region", mod(func(r *auditv2pb.ListAuditEventRequest) { r.Region = "" })},
		{"missing start", mod(func(r *auditv2pb.ListAuditEventRequest) { r.Start = nil })},
		{"missing parent", mod(func(r *auditv2pb.ListAuditEventRequest) { r.ParentId = "" })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := env.events.List(t.Context(), tt.req)
			assertCode(t, err, codes.InvalidArgument)
		})
	}
}

// TestAuditParentScope checks that parent_id selects the events under that
// resource: the project sees what happens in it, the tenant sees everything.
func TestAuditParentScope(t *testing.T) {
	t.Parallel()
	env := startAuditWith(t, []mocker.Module{kms.New()})
	ctx := t.Context()
	kmsc := newKMSClients(env.inst.SDK(t))
	key := kmsSymKind(kmsc).mustCreate(t, env.inst.ProjectID, "scoped-key", nil)
	group := createGroup(t, env.groups, env.inst.TenantID, "tenant-level", nil)

	project := listEvents(t, env.events, env.inst.ProjectID, "")
	if len(project) != 1 || project[0].GetResource().GetMetadata().GetId() != key.id {
		t.Fatalf("project events = %v, want only the key creation %s", eventIDs(project), key.id)
	}
	if got := project[0].GetService().GetName(); got != "KMS" {
		t.Errorf("project event service.name = %q, want KMS", got)
	}
	h := project[0].GetResource().GetHierarchy()
	if len(h) != 2 || h[0].GetId() != env.inst.TenantID || h[1].GetId() != env.inst.ProjectID {
		t.Errorf("hierarchy = %v, want [tenant, project]", h)
	}

	tenantEvs := env.list(t, "")
	var ids []string
	for _, ev := range tenantEvs {
		ids = append(ids, ev.GetResource().GetMetadata().GetId())
	}
	if !slices.Contains(ids, key.id) || !slices.Contains(ids, group.GetMetadata().GetId()) {
		t.Errorf("tenant events cover %v, want both %s and %s", ids, key.id, group.GetMetadata().GetId())
	}

	_, err := env.events.List(ctx, auditListReq("tenant-e00missing00000000", ""))
	assertCode(t, err, codes.NotFound)
	_, err = env.events.List(ctx, auditListReq("project-e00missing00000000", ""))
	assertCode(t, err, codes.NotFound)
}
