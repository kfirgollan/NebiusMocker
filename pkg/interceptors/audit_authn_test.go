package interceptors

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
)

// fakeAuthenticator accepts exactly the tokens in valid.
type fakeAuthenticator struct{ valid map[string]string }

func (f fakeAuthenticator) Authenticate(token string) (string, error) {
	if p, ok := f.valid[token]; ok {
		return p, nil
	}
	return "", errors.New("unknown token")
}

type authnEnv struct {
	log   *audit.Log
	chain grpc.UnaryServerInterceptor
}

func newAuthnEnv(t *testing.T, mode config.AuthnMode, tokens TokenAuthenticator) *authnEnv {
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
	cfg := AuditConfig{
		Recorder: rec, Rules: rules, DefaultTenant: "tenant-default",
		Hierarchy: &fakeHierarchy{parents: map[string]string{"project-x": "tenant-t", "group-1": "project-x"}},
	}
	outer, inner := AuditAuthnFailures(cfg), Authenticate(mode, tokens)
	return &authnEnv{log: l, chain: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		return outer(ctx, req, info, func(ctx context.Context, req any) (any, error) {
			return inner(ctx, req, info, next)
		})
	}}
}

func (e *authnEnv) events(t *testing.T) []*auditv2pb.AuditEvent {
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

func (e *authnEnv) call(ctx context.Context, method string) (bool, error) {
	reached := false
	_, err := e.chain(ctx, &iampb.GetGroupRequest{Id: "group-1"}, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) {
			reached = true
			return &iampb.Group{}, nil
		})
	return reached, err
}

func authnContext(t *testing.T, authorization string) context.Context {
	t.Helper()
	md := metadata.Pairs("user-agent", "terraform/1.2")
	if authorization != "" {
		md.Set("authorization", authorization)
	}
	ctx := metadata.NewIncomingContext(t.Context(), md)
	return peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 5555}})
}

func TestAuditAuthnFailures(t *testing.T) {
	t.Parallel()
	const get = "/" + groupSvc + "/Get"
	tests := []struct {
		name          string
		mode          config.AuthnMode
		authorization string
		method        string
		wantEvents    int
		wantReached   bool
		wantToken     bool
	}{
		{name: "rejected token", mode: config.AuthnOptional, authorization: "Bearer bogus.token.value", method: get, wantEvents: 1, wantToken: true},
		{name: "rejected malformed header", mode: config.AuthnOptional, authorization: "Basic abc", method: get, wantEvents: 1, wantToken: true},
		{name: "valid token", mode: config.AuthnRequired, authorization: "Bearer good", method: get, wantReached: true},
		{name: "missing token required", mode: config.AuthnRequired, method: get, wantEvents: 1},
		{name: "missing token optional is anonymous", mode: config.AuthnOptional, method: get, wantReached: true},
		{name: "exempt method with bad token", mode: config.AuthnRequired, authorization: "Bearer bogus", method: "/nebius.iam.v1.TokenExchangeService/Exchange", wantReached: true},
		{name: "exempt method without token", mode: config.AuthnRequired, method: "/nebius.iam.v1.TokenExchangeService/Exchange", wantReached: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newAuthnEnv(t, tt.mode, fakeAuthenticator{valid: map[string]string{"good": "useraccount-alice"}})
			reached, err := env.call(authnContext(t, tt.authorization), tt.method)
			if reached != tt.wantReached {
				t.Errorf("handler reached = %v, want %v", reached, tt.wantReached)
			}
			if tt.wantReached && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if !tt.wantReached && status.Code(err) != codes.Unauthenticated {
				t.Fatalf("err = %v, want Unauthenticated", err)
			}
			evs := env.events(t)
			if len(evs) != tt.wantEvents {
				t.Fatalf("events = %d, want %d: %v", len(evs), tt.wantEvents, evs)
			}
			if tt.wantEvents == 0 {
				return
			}
			ev := evs[0]
			if ev.GetStatus() != auditv2pb.Status_ERROR || ev.GetResponse().GetStatusCode() != code.Code_UNAUTHENTICATED {
				t.Errorf("status/code = %v/%v, want ERROR/UNAUTHENTICATED", ev.GetStatus(), ev.GetResponse().GetStatusCode())
			}
			a := ev.GetAuthentication()
			if a.GetAuthenticated() {
				t.Error("authenticated = true, want false")
			}
			if has := a.GetTokenCredential().GetMaskedToken() != ""; has != tt.wantToken {
				t.Errorf("has masked token = %v, want %v (%v)", has, tt.wantToken, a)
			}
			if ev.GetRequest().GetParameters() != nil {
				t.Errorf("request.parameters = %v, want none for an unauthenticated caller", ev.GetRequest().GetParameters())
			}
			if ev.GetType() != "ai.nebius.iam.group.get" || ev.GetAction() != "GET" || ev.GetService().GetName() != "IAM" {
				t.Errorf("type/action/service = %q/%q/%q", ev.GetType(), ev.GetAction(), ev.GetService().GetName())
			}
			if ev.GetResource().GetMetadata().GetId() != "group-1" || len(ev.GetResource().GetHierarchy()) != 2 {
				t.Errorf("resource = %v, want group-1 under tenant-t/project-x", ev.GetResource())
			}
			if ev.GetRequest().GetUserAgent() != "terraform/1.2" || ev.GetRequest().GetClientIp() != "10.1.2.3" {
				t.Errorf("request = %v", ev.GetRequest())
			}
		})
	}
}

func TestAuditAuthnFailuresMasksToken(t *testing.T) {
	t.Parallel()
	const secretTail = "c2VjcmV0c2lnbmF0dXJl"
	env := newAuthnEnv(t, config.AuthnRequired, fakeAuthenticator{})
	_, err := env.call(authnContext(t, "Bearer aGVhZGVy.cGF5bG9hZA."+secretTail), "/"+groupSvc+"/Get")
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
	evs := env.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if got := evs[0].GetAuthentication().GetTokenCredential().GetMaskedToken(); got != "aGVhZGVy.cGF5bG9hZA" {
		t.Errorf("masked token = %q, want header.payload", got)
	}
	b, err := protojson.Marshal(evs[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secretTail) {
		t.Errorf("signature stored in event: %s", b)
	}
}

func TestAuditAuthnFailuresNamesIssuedToken(t *testing.T) {
	t.Parallel()
	secret := make([]byte, auth.MinSecretBytes)
	iss, err := auth.NewIssuer(secret, time.Hour, clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := iss.Mint(auth.Grant{Principal: "serviceaccount-1", Tenant: "tenant-t", KeyID: "publickey-1"})
	if err != nil {
		t.Fatal(err)
	}
	lastDot := strings.LastIndex(tok.AccessToken, ".")
	forged := tok.AccessToken[:lastDot+1] + "AAAA"

	env := newAuthnEnv(t, config.AuthnRequired, fakeAuthenticator{})
	if _, err := env.call(authnContext(t, "Bearer "+forged), "/"+groupSvc+"/Get"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
	evs := env.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	a := evs[0].GetAuthentication()
	if a.GetSubject().GetServiceAccountId() != "serviceaccount-1" {
		t.Errorf("subject = %v, want serviceaccount-1", a.GetSubject())
	}
	if got, want := a.GetTokenCredential().GetMaskedToken(), tok.AccessToken[:lastDot]; got != want {
		t.Errorf("masked token = %q, want %q (no MAC)", got, want)
	}
	params := evs[0].GetRequest().GetParameters().AsMap()
	if params["token_id"] != tok.Claims.TokenID || params["token_key_id"] != "publickey-1" {
		t.Errorf("parameters = %v, want the token and key ids", params)
	}
}
