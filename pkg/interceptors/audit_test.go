package interceptors

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/genproto/googleapis/rpc/code"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

func TestParseMethod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		full string
		want method
	}{
		{"/nebius.iam.v1.GroupService/Create", method{area: "iam", serviceName: "IAM", action: "CREATE", resourceType: "group"}},
		{"/nebius.vpc.v1.NetworkService/Delete", method{area: "vpc", serviceName: "VIRTUAL_NETWORKS", action: "DELETE", resourceType: "network"}},
		{"/nebius.mysterybox.v1.SecretService/Create", method{area: "mysterybox", serviceName: "MBOX", action: "CREATE", resourceType: "secret"}},
		{"/nebius.msp.postgresql.v1alpha1.ClusterService/Update", method{area: "msp.postgresql", serviceName: "MSP_POSTGRESQL", action: "UPDATE", resourceType: "cluster"}},
		{"/nebius.audit.v2.AuditEventExportService/Start", method{area: "audit", serviceName: "AUDIT", action: "START", resourceType: "auditeventexport"}},
		{"/nebius.iam.v1.GroupMembershipService/ListMemberOf", method{area: "iam", serviceName: "IAM", action: "LIST_MEMBER_OF", resourceType: "groupmembership"}},
	}
	for _, tt := range tests {
		t.Run(tt.full, func(t *testing.T) {
			t.Parallel()
			if got := parseMethod(tt.full); got != tt.want {
				t.Errorf("parseMethod(%q) = %+v, want %+v", tt.full, got, tt.want)
			}
		})
	}
}

func TestUpperSnake(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"":             "",
		"Create":       "CREATE",
		"ListMemberOf": "LIST_MEMBER_OF",
		"GetByName":    "GET_BY_NAME",
		"get":          "GET",
	}
	for in, want := range tests {
		if got := upperSnake(in); got != want {
			t.Errorf("upperSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsRead(t *testing.T) {
	t.Parallel()
	rules := authz.NewRules()
	rules.CRUD("nebius.iam.v1.GroupService", "iam", "groups")
	rules.Add("/x.Svc/Cancel", authz.OnID(authz.Perm("x", "ops", "cancel")))
	tests := []struct {
		name   string
		rules  *authz.Rules
		method string
		want   bool
	}{
		{"rule get", rules, "/nebius.iam.v1.GroupService/Get", true},
		{"rule list", rules, "/nebius.iam.v1.GroupService/List", true},
		{"rule create", rules, "/nebius.iam.v1.GroupService/Create", false},
		{"rule cancel", rules, "/x.Svc/Cancel", false},
		{"no rule get-prefix", rules, "/other.Svc/GetThing", true},
		{"no rule list-prefix", rules, "/other.Svc/ListThings", true},
		{"no rule mutation", rules, "/other.Svc/DeleteThing", false},
		{"nil-ish empty rules", authz.NewRules(), "/other.Svc/GetThing", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isRead(tt.rules, tt.method); got != tt.want {
				t.Errorf("isRead(%q) = %v, want %v", tt.method, got, tt.want)
			}
		})
	}
}

// fakeHierarchy is a child → parent map that can be made to forget everything.
type fakeHierarchy struct {
	mu      sync.Mutex
	parents map[string]string
}

func (f *fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parents[id], nil
}

func (f *fakeHierarchy) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parents = map[string]string{}
}

const (
	rawToken = "nmock-dev-admin"
	groupSvc = "nebius.iam.v1.GroupService"
)

type auditEnv struct {
	log  *audit.Log
	hier *fakeHierarchy
	icpt grpc.UnaryServerInterceptor
}

func newAuditEnv(t *testing.T, reads bool) *auditEnv {
	t.Helper()
	l, err := audit.NewLog(100)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := audit.NewRecorder(l, clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	rules := authz.NewRules()
	rules.CRUD(groupSvc, "iam", "groups")
	h := &fakeHierarchy{parents: map[string]string{
		"project-x": "tenant-t",
		"group-1":   "project-x",
	}}
	return &auditEnv{log: l, hier: h, icpt: Audit(AuditConfig{
		Recorder: rec, Hierarchy: h, Rules: rules, Reads: reads, DefaultTenant: "tenant-default",
	})}
}

func (e *auditEnv) events(t *testing.T) []*auditv2pb.AuditEvent {
	t.Helper()
	var out []*auditv2pb.AuditEvent
	for en := range e.log.Since(0) {
		ev, err := en.Event()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func reqContext(t *testing.T, principal string) (context.Context, *reqctx.Info) {
	t.Helper()
	info := &reqctx.Info{RequestID: "req-1", Principal: reqctx.Principal{ID: principal}}
	ctx := reqctx.WithInfo(t.Context(), info)
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		"authorization", "Bearer "+rawToken,
		"user-agent", "terraform/1.2",
		"x-idempotency-key", "idem-42",
	))
	ctx = peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 5555}})
	return ctx, info
}

func (e *auditEnv) call(ctx context.Context, method string, req any, next grpc.UnaryHandler) (any, error) {
	return e.icpt(ctx, req, &grpc.UnaryServerInfo{FullMethod: method}, next)
}

func returning(resp any, err error) grpc.UnaryHandler {
	return func(context.Context, any) (any, error) { return resp, err }
}

func runningOp() *commonpb.Operation {
	return &commonpb.Operation{Id: "op-1", ResourceId: "group-1"}
}

func finishedOp(c code.Code, msg string) *commonpb.Operation {
	return &commonpb.Operation{
		Id: "op-2", ResourceId: "group-1", FinishedAt: timestamppb.Now(),
		Status: &rpcstatus.Status{Code: int32(c), Message: msg},
	}
}

func createGroupReq() *iampb.CreateGroupRequest {
	return &iampb.CreateGroupRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "project-x", Name: "devs"}}
}

func assertNoRawToken(t *testing.T, evs []*auditv2pb.AuditEvent) {
	t.Helper()
	for _, ev := range evs {
		b, err := protojson.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), rawToken) {
			t.Errorf("raw bearer token stored in event: %s", b)
		}
	}
}

func TestAuditCreateStarted(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, false)
	ctx, _ := reqContext(t, "useraccount-alice")

	op := runningOp()
	resp, err := env.call(ctx, "/"+groupSvc+"/Create", createGroupReq(), returning(op, nil))
	if err != nil || resp != any(op) {
		t.Fatalf("interceptor returned (%v, %v), want (op, nil)", resp, err)
	}

	evs := env.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.GetStatus() != auditv2pb.Status_STARTED {
		t.Errorf("status = %v, want STARTED", ev.GetStatus())
	}
	md := ev.GetResource().GetMetadata()
	if md.GetId() != "group-1" || md.GetType() != "group" || md.GetName() != "devs" {
		t.Errorf("resource metadata = %v, want group-1/group/devs", md)
	}
	var hier []string
	for _, h := range ev.GetResource().GetHierarchy() {
		hier = append(hier, h.GetId())
	}
	if strings.Join(hier, ",") != "tenant-t,project-x" {
		t.Errorf("hierarchy = %v, want [tenant-t project-x]", hier)
	}
	if ev.GetAction() != "CREATE" || ev.GetService().GetName() != "IAM" || ev.GetType() != "ai.nebius.iam.group.create" {
		t.Errorf("action/service/type = %q/%q/%q", ev.GetAction(), ev.GetService().GetName(), ev.GetType())
	}
	authn := ev.GetAuthentication()
	if !authn.GetAuthenticated() || authn.GetSubject().GetTenantUserId() != "useraccount-alice" {
		t.Errorf("authentication = %v, want authenticated tenant user alice", authn)
	}
	if tok := authn.GetTokenCredential().GetMaskedToken(); tok == "" || tok == rawToken {
		t.Errorf("masked token = %q, want a masked non-empty token", tok)
	}
	req := ev.GetRequest()
	if got := req.GetParameters().AsMap()["metadata"].(map[string]any)["name"]; got != "devs" {
		t.Errorf("request params metadata.name = %v, want devs", got)
	}
	if req.GetUserAgent() != "terraform/1.2" || req.GetIdempotencyId() != "idem-42" ||
		req.GetClientIp() != "10.1.2.3" || req.GetRequestId() != "req-1" {
		t.Errorf("request = %v", req)
	}
	assertNoRawToken(t, evs)
}

func TestAuditDeleteUsesPreCallHierarchy(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, false)
	ctx, _ := reqContext(t, "useraccount-alice")
	next := func(context.Context, any) (any, error) {
		env.hier.forget() // the resource is gone once the handler ran
		return finishedOp(code.Code_OK, ""), nil
	}
	if _, err := env.call(ctx, "/"+groupSvc+"/Delete", &iampb.DeleteGroupRequest{Id: "group-1"}, next); err != nil {
		t.Fatal(err)
	}
	evs := env.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.GetStatus() != auditv2pb.Status_DONE {
		t.Errorf("status = %v, want DONE", ev.GetStatus())
	}
	var hier []string
	for _, h := range ev.GetResource().GetHierarchy() {
		hier = append(hier, h.GetId())
	}
	if strings.Join(hier, ",") != "tenant-t,project-x" {
		t.Errorf("hierarchy = %v, want [tenant-t project-x]", hier)
	}
	if ev.GetAction() != "DELETE" {
		t.Errorf("action = %q, want DELETE", ev.GetAction())
	}
}

func TestAuditReads(t *testing.T) {
	t.Parallel()
	get := "/" + groupSvc + "/Get"
	denied := status.Error(codes.PermissionDenied, "nope")
	tests := []struct {
		name       string
		reads      bool
		deny       bool
		err        error
		wantEvents int
		wantStatus auditv2pb.Status
		wantCode   code.Code
	}{
		{name: "denied get", deny: true, err: denied, wantEvents: 1, wantStatus: auditv2pb.Status_ERROR, wantCode: code.Code_PERMISSION_DENIED},
		{name: "get without denial", err: nil, wantEvents: 0},
		{name: "handler error without denial", err: status.Error(codes.NotFound, "x"), wantEvents: 0},
		{name: "get with reads", reads: true, wantEvents: 1, wantStatus: auditv2pb.Status_DONE},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newAuditEnv(t, tt.reads)
			ctx, info := reqContext(t, "useraccount-alice")
			next := func(context.Context, any) (any, error) {
				info.Denied = tt.deny
				return &iampb.Group{}, tt.err
			}
			_, err := env.call(ctx, get, &iampb.GetGroupRequest{Id: "group-1"}, next)
			if err != tt.err { //nolint:errorlint // identity: the interceptor must return next's error as is
				t.Errorf("err = %v, want %v", err, tt.err)
			}
			evs := env.events(t)
			if len(evs) != tt.wantEvents {
				t.Fatalf("events = %d, want %d", len(evs), tt.wantEvents)
			}
			if tt.wantEvents == 0 {
				return
			}
			if evs[0].GetStatus() != tt.wantStatus || evs[0].GetResponse().GetStatusCode() != tt.wantCode {
				t.Errorf("status/code = %v/%v, want %v/%v", evs[0].GetStatus(), evs[0].GetResponse().GetStatusCode(), tt.wantStatus, tt.wantCode)
			}
			if evs[0].GetResource().GetMetadata().GetId() != "group-1" {
				t.Errorf("resource id = %q, want group-1", evs[0].GetResource().GetMetadata().GetId())
			}
			if len(evs[0].GetResource().GetHierarchy()) != 2 {
				t.Errorf("hierarchy = %v, want resolved after denial", evs[0].GetResource().GetHierarchy())
			}
		})
	}
}

func TestAuditExemptMethod(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, true)
	ctx, _ := reqContext(t, "useraccount-alice")
	called := false
	next := func(context.Context, any) (any, error) { called = true; return "ok", nil }
	resp, err := env.call(ctx, "/grpc.health.v1.Health/Check", struct{}{}, next)
	if err != nil || resp != "ok" || !called {
		t.Fatalf("got (%v, %v, called=%v)", resp, err, called)
	}
	if n := env.log.Len(); n != 0 {
		t.Errorf("events = %d, want 0", n)
	}
}

func TestAuditFinishedOperationWithError(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, false)
	ctx, _ := reqContext(t, "useraccount-alice")
	_, err := env.call(ctx, "/"+groupSvc+"/Create", createGroupReq(),
		returning(finishedOp(code.Code_INVALID_ARGUMENT, "bad group"), nil))
	if err != nil {
		t.Fatal(err)
	}
	evs := env.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if evs[0].GetStatus() != auditv2pb.Status_ERROR {
		t.Errorf("status = %v, want ERROR", evs[0].GetStatus())
	}
	if r := evs[0].GetResponse(); r.GetStatusCode() != code.Code_INVALID_ARGUMENT || r.GetErrorMessage() != "bad group" {
		t.Errorf("response = %v, want INVALID_ARGUMENT/bad group", r)
	}
}

func TestAuditMutationHandlerError(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, false)
	ctx, _ := reqContext(t, "useraccount-alice")
	want := status.Error(codes.AlreadyExists, "dup")
	resp, err := env.call(ctx, "/"+groupSvc+"/Create", createGroupReq(), returning(nil, want))
	if err != want || resp != nil { //nolint:errorlint // identity check
		t.Errorf("got (%v, %v), want (nil, %v)", resp, err, want)
	}
	evs := env.events(t)
	if len(evs) != 1 || evs[0].GetStatus() != auditv2pb.Status_ERROR ||
		evs[0].GetResponse().GetStatusCode() != code.Code_ALREADY_EXISTS {
		t.Errorf("events = %v, want one ERROR/ALREADY_EXISTS", evs)
	}
}

func TestAuditAnonymousPrincipal(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, false)
	ctx, _ := reqContext(t, "")
	if _, err := env.call(ctx, "/"+groupSvc+"/Create", createGroupReq(), returning(runningOp(), nil)); err != nil {
		t.Fatal(err)
	}
	evs := env.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	authn := evs[0].GetAuthentication()
	if authn.GetAuthenticated() || authn.GetSubject() != nil || authn.GetTokenCredential() != nil {
		t.Errorf("authentication = %v, want unauthenticated without subject or token", authn)
	}
	assertNoRawToken(t, evs)
}

func TestAuditServiceAccountPrincipal(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, false)
	ctx, _ := reqContext(t, "serviceaccount-ci")
	if _, err := env.call(ctx, "/"+groupSvc+"/Create", createGroupReq(), returning(runningOp(), nil)); err != nil {
		t.Fatal(err)
	}
	sub := env.events(t)[0].GetAuthentication().GetSubject()
	if sub.GetServiceAccountId() != "serviceaccount-ci" || sub.GetTenantUserId() != "" {
		t.Errorf("subject = %v, want service account", sub)
	}
}

func TestAuditHierarchyFallsBackToDefaultTenant(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, false)
	ctx, _ := reqContext(t, "useraccount-alice")
	// group-zzz is unknown to the hierarchy: no ancestors can be resolved.
	resp := finishedOp(code.Code_OK, "")
	resp.ResourceId = "group-zzz"
	if _, err := env.call(ctx, "/"+groupSvc+"/Delete", &iampb.DeleteGroupRequest{Id: "group-zzz"}, returning(resp, nil)); err != nil {
		t.Fatal(err)
	}
	evs := env.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	h := evs[0].GetResource().GetHierarchy()
	if len(h) != 1 || h[0].GetId() != "tenant-default" {
		t.Errorf("hierarchy = %v, want [tenant-default]", h)
	}
}

func TestAuditReturnsNextResultUnchanged(t *testing.T) {
	t.Parallel()
	env := newAuditEnv(t, true)
	ctx, _ := reqContext(t, "useraccount-alice")
	wantErr := status.Error(codes.Unavailable, "later")
	wantResp := &iampb.Group{}
	resp, err := env.call(ctx, "/"+groupSvc+"/Update", &iampb.UpdateGroupRequest{Metadata: &commonpb.ResourceMetadata{Id: "group-1"}}, returning(wantResp, wantErr))
	if resp != any(wantResp) || err != wantErr { //nolint:errorlint // identity check
		t.Errorf("got (%v, %v), want (%v, %v)", resp, err, wantResp, wantErr)
	}
}

// fakeStates answers each read of an ID with the next queued state; a nil
// entry (or an exhausted queue) means "not readable".
type fakeStates struct {
	mu    sync.Mutex
	queue map[string][]*structpb.Struct
	calls []string // "method id" per read
}

func (f *fakeStates) ResourceState(_ context.Context, method, id string) (*structpb.Struct, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+id)
	q := f.queue[id]
	if len(q) == 0 {
		return nil, false
	}
	f.queue[id] = q[1:]
	return q[0], q[0] != nil
}

func stateOf(t *testing.T, name string) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(map[string]any{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuditResourceState(t *testing.T) {
	t.Parallel()
	const (
		update = "/" + groupSvc + "/Update"
		create = "/" + groupSvc + "/Create"
		del    = "/" + groupSvc + "/Delete"
		get    = "/" + groupSvc + "/Get"
	)
	updateReq := &iampb.UpdateGroupRequest{Metadata: &commonpb.ResourceMetadata{Id: "group-1"}}
	deleteReq := &iampb.DeleteGroupRequest{Id: "group-1"}
	tests := []struct {
		name         string
		method       string
		req          any
		reads        []*structpb.Struct // queued states of group-1: before, then after
		resp         any
		err          error
		deny         bool
		wantPrevious string // "" = absent
		wantCurrent  string
		wantReads    int
	}{
		{
			name: "update with plain response", method: update, req: updateReq, resp: &iampb.Group{},
			reads: []*structpb.Struct{stateOf(t, "before"), stateOf(t, "after")}, wantPrevious: "before", wantCurrent: "after", wantReads: 2,
		},
		{
			name: "update with finished operation", method: update, req: updateReq, resp: finishedOp(code.Code_OK, ""),
			reads: []*structpb.Struct{stateOf(t, "before"), stateOf(t, "after")}, wantPrevious: "before", wantCurrent: "after", wantReads: 2,
		},
		{
			name: "create has no previous", method: create, req: createGroupReq(), resp: finishedOp(code.Code_OK, ""),
			reads: []*structpb.Struct{stateOf(t, "after")}, wantCurrent: "after", wantReads: 1,
		},
		{
			name: "delete has no current", method: del, req: deleteReq, resp: finishedOp(code.Code_OK, ""),
			reads: []*structpb.Struct{stateOf(t, "before"), nil}, wantPrevious: "before", wantReads: 2,
		},
		{
			name: "running operation has no current", method: update, req: updateReq, resp: runningOp(),
			reads: []*structpb.Struct{stateOf(t, "before"), stateOf(t, "after")}, wantPrevious: "before", wantReads: 1,
		},
		{
			name: "failed operation has no current", method: update, req: updateReq, resp: finishedOp(code.Code_INTERNAL, "boom"),
			reads: []*structpb.Struct{stateOf(t, "before"), stateOf(t, "after")}, wantPrevious: "before", wantReads: 1,
		},
		{
			name: "handler error has no state", method: update, req: updateReq, err: status.Error(codes.Internal, "boom"),
			reads: []*structpb.Struct{stateOf(t, "before"), stateOf(t, "after")}, wantReads: 1,
		},
		{
			name: "denied call has no state", method: update, req: updateReq, resp: &iampb.Group{}, deny: true,
			reads: []*structpb.Struct{stateOf(t, "before"), stateOf(t, "after")}, wantReads: 1,
		},
		{
			name: "read method never reads state", method: get, req: &iampb.GetGroupRequest{Id: "group-1"}, resp: &iampb.Group{},
			reads: []*structpb.Struct{stateOf(t, "before")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := &fakeStates{queue: map[string][]*structpb.Struct{"group-1": tt.reads}}
			l, err := audit.NewLog(10)
			if err != nil {
				t.Fatal(err)
			}
			rec, err := audit.NewRecorder(l, clock.NewManual(time.Unix(0, 0).UTC()), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			rules := authz.NewRules()
			rules.CRUD(groupSvc, "iam", "groups")
			icpt := Audit(AuditConfig{Recorder: rec, Hierarchy: &fakeHierarchy{}, Rules: rules, Reads: true, States: fs})
			ctx, info := reqContext(t, "useraccount-alice")
			next := func(context.Context, any) (any, error) {
				info.Denied = tt.deny
				return tt.resp, tt.err
			}
			if _, err := icpt(ctx, tt.req, &grpc.UnaryServerInfo{FullMethod: tt.method}, next); err != tt.err { //nolint:errorlint // identity check
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			var evs []*auditv2pb.AuditEvent
			for e := range l.Since(0) {
				ev, err := e.Event()
				if err != nil {
					t.Fatal(err)
				}
				evs = append(evs, ev)
			}
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			var want *auditv2pb.ResourceState
			if tt.wantPrevious != "" || tt.wantCurrent != "" {
				want = &auditv2pb.ResourceState{}
				if tt.wantPrevious != "" {
					want.Previous = stateOf(t, tt.wantPrevious)
				}
				if tt.wantCurrent != "" {
					want.Current = stateOf(t, tt.wantCurrent)
				}
			}
			if diff := cmp.Diff(want, evs[0].GetResource().GetState(), protocmp.Transform()); diff != "" {
				t.Errorf("resource.state mismatch (-want +got):\n%s", diff)
			}
			if len(fs.calls) != tt.wantReads {
				t.Errorf("state reads = %v, want %d", fs.calls, tt.wantReads)
			}
		})
	}
}

func TestAuditRunningOperationCompletes(t *testing.T) {
	t.Parallel()
	l, err := audit.NewLog(10)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := audit.NewRecorder(l, clock.NewManual(time.Unix(0, 0).UTC()), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	rules := authz.NewRules()
	rules.CRUD(groupSvc, "iam", "groups")
	icpt := Audit(AuditConfig{Recorder: rec, Hierarchy: &fakeHierarchy{}, Rules: rules})
	ctx, _ := reqContext(t, "useraccount-alice")
	_, err = icpt(ctx, createGroupReq(), &grpc.UnaryServerInfo{FullMethod: "/" + groupSvc + "/Create"}, returning(runningOp(), nil))
	if err != nil {
		t.Fatal(err)
	}
	rec.OperationFinished(ctx, "op-1", code.Code_OK, "")
	var statuses []auditv2pb.Status
	for e := range l.Since(0) {
		ev, err := e.Event()
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, ev.GetStatus())
	}
	if len(statuses) != 2 || statuses[0] != auditv2pb.Status_STARTED || statuses[1] != auditv2pb.Status_DONE {
		t.Errorf("statuses = %v, want [STARTED DONE]", statuses)
	}
}
