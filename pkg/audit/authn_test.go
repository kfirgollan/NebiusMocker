package audit

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/genproto/googleapis/rpc/code"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
)

func TestAuthnFailureEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		f        AuthnFailure
		wantType string
		wantRes  string
		wantKind string
	}{
		{
			name:     "type from id prefix",
			f:        AuthnFailure{Area: "iam", Action: "CREATE", ResourceID: "publickey-abc", ResourceType: "group"},
			wantType: "ai.nebius.iam.publickey.create", wantRes: "publickey-abc", wantKind: "publickey",
		},
		{
			name:     "type from resource type without id",
			f:        AuthnFailure{Area: "vpc", Action: "LIST", ResourceType: "network"},
			wantType: "ai.nebius.vpc.network.list", wantKind: "network",
		},
		{
			name:     "id without dash falls back to type",
			f:        AuthnFailure{Area: "iam", Action: "GET_BY_NAME", ResourceID: "plain", ResourceType: "group"},
			wantType: "ai.nebius.iam.group.get_by_name", wantRes: "plain", wantKind: "group",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := AuthnFailureEvent(tt.f)
			if ev.GetType() != tt.wantType {
				t.Errorf("type = %q, want %q", ev.GetType(), tt.wantType)
			}
			md := ev.GetResource().GetMetadata()
			if md.GetId() != tt.wantRes || md.GetType() != tt.wantKind {
				t.Errorf("resource = %v, want id %q type %q", md, tt.wantRes, tt.wantKind)
			}
			if ev.GetStatus() != auditv2pb.Status_ERROR {
				t.Errorf("status = %v, want ERROR", ev.GetStatus())
			}
			if ev.GetAuthentication().GetAuthenticated() {
				t.Error("authenticated = true, want false")
			}
		})
	}
}

func TestAuthnFailureEventFields(t *testing.T) {
	t.Parallel()
	ev := AuthnFailureEvent(AuthnFailure{
		Source: "nebius.iam.v1.GroupService/Create", Area: "iam", ServiceName: "IAM", Action: "CREATE",
		ResourceID: "group-1", Hierarchy: []string{"tenant-t", "project-x"},
		Token: "aGVhZGVy.cGF5bG9hZA.c2lnbmF0dXJl", Subject: "serviceaccount-1",
		Parameters: map[string]string{"kid": "publickey-1", "empty": ""},
		ClientIP:   "10.0.0.1", UserAgent: "ua", RequestID: "req-1",
		Code: code.Code_UNAUTHENTICATED, Message: "invalid bearer token",
		Payload: map[string]string{"error": "invalid_grant"},
	})
	if ev.GetSource() != "nebius.iam.v1.GroupService/Create" || ev.GetAction() != "CREATE" || ev.GetService().GetName() != "IAM" {
		t.Errorf("source/action/service = %q/%q/%q", ev.GetSource(), ev.GetAction(), ev.GetService().GetName())
	}
	var hier []string
	for _, h := range ev.GetResource().GetHierarchy() {
		hier = append(hier, h.GetType()+":"+h.GetId())
	}
	if diff := cmp.Diff([]string{"tenant:tenant-t", "project:project-x"}, hier); diff != "" {
		t.Errorf("hierarchy (-want +got):\n%s", diff)
	}
	a := ev.GetAuthentication()
	if a.GetAuthenticationType() != auditv2pb.AuthenticationType_ACCESS_TOKEN {
		t.Errorf("authentication type = %v, want ACCESS_TOKEN", a.GetAuthenticationType())
	}
	if got := a.GetTokenCredential().GetMaskedToken(); got != "aGVhZGVy.cGF5bG9hZA" {
		t.Errorf("masked token = %q, want header.payload", got)
	}
	if a.GetSubject().GetServiceAccountId() != "serviceaccount-1" {
		t.Errorf("subject = %v, want service account 1", a.GetSubject())
	}
	req := ev.GetRequest()
	if req.GetClientIp() != "10.0.0.1" || req.GetUserAgent() != "ua" || req.GetRequestId() != "req-1" || req.GetTraceId() != "req-1" {
		t.Errorf("request = %v", req)
	}
	if diff := cmp.Diff(map[string]any{"kid": "publickey-1"}, req.GetParameters().AsMap()); diff != "" {
		t.Errorf("parameters (-want +got):\n%s", diff)
	}
	resp := ev.GetResponse()
	if resp.GetStatusCode() != code.Code_UNAUTHENTICATED || resp.GetErrorMessage() != "invalid bearer token" {
		t.Errorf("response = %v", resp)
	}
	if diff := cmp.Diff(map[string]any{"error": "invalid_grant"}, resp.GetPayload().AsMap()); diff != "" {
		t.Errorf("payload (-want +got):\n%s", diff)
	}
}

func TestFailedAuthentication(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		token, subj string
		wantToken   bool
		wantSA      string
		wantUser    string
	}{
		{name: "nothing presented"},
		{name: "token only", token: "nmock-dev-admin", wantToken: true},
		{name: "service account", token: "a.b.c", subj: "serviceaccount-9", wantToken: true, wantSA: "serviceaccount-9"},
		{name: "tenant user", subj: "useraccount-alice", wantUser: "useraccount-alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := FailedAuthentication(tt.token, tt.subj)
			if a.GetAuthenticated() {
				t.Error("authenticated = true")
			}
			if has := a.GetTokenCredential() != nil; has != tt.wantToken {
				t.Errorf("has credential = %v, want %v", has, tt.wantToken)
			}
			if a.GetSubject().GetServiceAccountId() != tt.wantSA || a.GetSubject().GetTenantUserId() != tt.wantUser {
				t.Errorf("subject = %v, want sa %q user %q", a.GetSubject(), tt.wantSA, tt.wantUser)
			}
			if tt.token == "" && a.GetAuthenticationType() != auditv2pb.AuthenticationType_AUTHENTICATION_TYPE_UNSPECIFIED {
				t.Errorf("authentication type = %v without a token", a.GetAuthenticationType())
			}
		})
	}
}

func TestAuthnFailureEventClipping(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return strings.Repeat("x", n) }
	params := map[string]string{"a_long": long(1000)}
	for i := range 30 {
		params["k"+strconv.Itoa(1000+i)] = "v"
	}
	ev := AuthnFailureEvent(AuthnFailure{
		Area: "iam", Action: "GET",
		ResourceID: "group-" + long(1000), UserAgent: long(1000), ClientIP: long(1000),
		Message: long(5000), Subject: "useraccount-" + long(1000),
		Token:      "h." + long(5000) + ".sig",
		Parameters: params, Payload: map[string]string{"p": long(1000)},
	})
	if n := len(ev.GetResource().GetMetadata().GetId()); n != maxUntrusted {
		t.Errorf("resource id length = %d, want %d", n, maxUntrusted)
	}
	if n := len(ev.GetRequest().GetUserAgent()); n != maxUntrusted {
		t.Errorf("user agent length = %d, want %d", n, maxUntrusted)
	}
	if n := len(ev.GetRequest().GetClientIp()); n != maxUntrusted {
		t.Errorf("client ip length = %d, want %d", n, maxUntrusted)
	}
	if n := len(ev.GetResponse().GetErrorMessage()); n != maxMessage {
		t.Errorf("message length = %d, want %d", n, maxMessage)
	}
	if n := len(ev.GetAuthentication().GetSubject().GetName()); n != maxUntrusted {
		t.Errorf("subject length = %d, want %d", n, maxUntrusted)
	}
	if n := len(ev.GetAuthentication().GetTokenCredential().GetMaskedToken()); n != maxMaskedToken {
		t.Errorf("masked token length = %d, want %d", n, maxMaskedToken)
	}
	fields := ev.GetRequest().GetParameters().GetFields()
	if len(fields) > maxParameters {
		t.Errorf("parameters = %d keys, want <= %d", len(fields), maxParameters)
	}
	for k, v := range fields {
		if len(v.GetStringValue()) > maxUntrusted {
			t.Errorf("parameter %q value length = %d, want <= %d", k, len(v.GetStringValue()), maxUntrusted)
		}
	}
	if n := len(fields["a_long"].GetStringValue()); n != maxUntrusted {
		t.Errorf("long parameter length = %d, want %d", n, maxUntrusted)
	}
	if n := len(ev.GetResponse().GetPayload().GetFields()["p"].GetStringValue()); n != maxUntrusted {
		t.Errorf("payload value length = %d, want %d", n, maxUntrusted)
	}
}

func TestClipRuneBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short", "abc", 5, "abc"},
		{"exact", "abc", 3, "abc"},
		{"ascii cut", "abcdef", 3, "abc"},
		{"multibyte cut backs off", "aé", 2, "a"},
		{"multibyte kept", "aé", 3, "aé"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clip(tt.in, tt.n); got != tt.want {
				t.Errorf("clip(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestSubjectOf(t *testing.T) {
	t.Parallel()
	if s := SubjectOf("serviceaccount-1"); s.GetServiceAccountId() != "serviceaccount-1" || s.GetName() != "serviceaccount-1" {
		t.Errorf("SubjectOf(sa) = %v", s)
	}
	if s := SubjectOf("useraccount-1"); s.GetTenantUserId() != "useraccount-1" || s.GetServiceAccountId() != "" {
		t.Errorf("SubjectOf(user) = %v", s)
	}
}

// mapHierarchy is a child -> parent map.
type mapHierarchy map[string]string

func (m mapHierarchy) Parent(_ context.Context, id string) (string, error) { return m[id], nil }

func TestAncestry(t *testing.T) {
	t.Parallel()
	h := mapHierarchy{"group-1": "project-x", "project-x": "tenant-t"}
	tests := []struct {
		id   string
		want []string
	}{
		{"group-1", []string{"tenant-t", "project-x"}},
		{"project-x", []string{"tenant-t"}},
		{"tenant-t", nil},
		{"unknown-1", nil},
	}
	for _, tt := range tests {
		if diff := cmp.Diff(tt.want, Ancestry(t.Context(), h, tt.id)); diff != "" {
			t.Errorf("Ancestry(%q) (-want +got):\n%s", tt.id, diff)
		}
	}
	cycle := mapHierarchy{"a-1": "b-1", "b-1": "a-1"}
	if got := Ancestry(t.Context(), cycle, "a-1"); len(got) != maxAncestry {
		t.Errorf("Ancestry over a cycle = %d links, want the cap %d", len(got), maxAncestry)
	}
}

func TestNewAuthnRecorderValidation(t *testing.T) {
	t.Parallel()
	rec, _, _ := newTestRecorder(t)
	if _, err := NewAuthnRecorder(nil, mapHierarchy{}, "tenant-d"); err == nil {
		t.Error("nil recorder: error = nil")
	}
	if _, err := NewAuthnRecorder(rec, nil, "tenant-d"); err == nil {
		t.Error("nil hierarchy: error = nil")
	}
}

func TestRecordAuthnFailureHierarchy(t *testing.T) {
	t.Parallel()
	h := mapHierarchy{
		"publickey-1":      "serviceaccount-1",
		"serviceaccount-1": "project-x",
		"project-x":        "tenant-t",
	}
	tests := []struct {
		name   string
		tenant string
		f      AuthnFailure
		want   []string
	}{
		{
			name: "resource ancestry",
			f:    AuthnFailure{ResourceID: "publickey-1", Subject: "serviceaccount-1"},
			want: []string{"tenant-t", "project-x", "serviceaccount-1"},
		},
		{
			name: "subject ancestry and subject",
			f:    AuthnFailure{ResourceID: "publickey-canary", Subject: "serviceaccount-1"},
			want: []string{"tenant-t", "project-x", "serviceaccount-1"},
		},
		{
			name: "subject only",
			f:    AuthnFailure{Subject: "serviceaccount-1"},
			want: []string{"tenant-t", "project-x", "serviceaccount-1"},
		},
		{
			name:   "default tenant",
			tenant: "tenant-default",
			f:      AuthnFailure{ResourceID: "publickey-canary", Subject: "serviceaccount-ghost"},
			want:   []string{"tenant-default"},
		},
		{
			name:   "same id for resource and subject is not retried",
			tenant: "tenant-default",
			f:      AuthnFailure{ResourceID: "serviceaccount-ghost", Subject: "serviceaccount-ghost"},
			want:   []string{"tenant-default"},
		},
		{
			name: "no default tenant",
			f:    AuthnFailure{ResourceID: "publickey-canary"},
			want: nil,
		},
		{
			name:   "explicit hierarchy kept",
			tenant: "tenant-default",
			f:      AuthnFailure{ResourceID: "publickey-1", Hierarchy: []string{"tenant-z"}},
			want:   []string{"tenant-z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, l, _ := newTestRecorder(t)
			ar, err := NewAuthnRecorder(rec, h, tt.tenant)
			if err != nil {
				t.Fatal(err)
			}
			tt.f.Area, tt.f.Action, tt.f.Token = "iam", "CREATE", "nmock-dev-admin"
			ar.RecordAuthnFailure(t.Context(), tt.f)
			evs := events(t, l)
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			var got []string
			for _, m := range evs[0].GetResource().GetHierarchy() {
				got = append(got, m.GetId())
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("hierarchy (-want +got):\n%s", diff)
			}
			if evs[0].GetAuthentication().GetAuthenticated() || evs[0].GetStatus() != auditv2pb.Status_ERROR {
				t.Errorf("event = %v, want unauthenticated ERROR", evs[0])
			}
		})
	}
}
