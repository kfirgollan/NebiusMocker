package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/auth"
	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
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

// authnUserAgent is the User-Agent the HTTP exchange requests send.
const authnUserAgent = "authn-contract-test/1.0"

// authnEnv is IAM + audit with authentication required and authz enforced:
// the setup of a tenant that watches for bad credentials.
type authnEnv struct {
	*saAuthEnv
	events auditsvc.AuditEventService
}

func startAuthnAudit(t *testing.T, opts ...testkit.Option) *authnEnv {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce)}, opts...)
	env := saAuthStart(t, []mocker.Module{iam.New(), audit.New()}, opts...)
	return &authnEnv{saAuthEnv: env, events: auditsvc.NewAuditEventService(env.admin)}
}

// failures lists the events under the tenant matching filter, as the admin.
func (e *authnEnv) failures(t *testing.T, filter string) []*auditv2pb.AuditEvent {
	t.Helper()
	return listEvents(t, e.events, e.inst.TenantID, filter)
}

// unsigned is a JWT without its signature: what masked_token holds.
func unsigned(jws string) string {
	i := strings.LastIndex(jws, ".")
	return jws[:i]
}

// signatureOf is the signature part of a JWS (or of an nmock.v1 token).
func signatureOf(jws string) string {
	return jws[strings.LastIndex(jws, ".")+1:]
}

// assertFailedAuthn checks the fields every failed-authentication event has.
func assertFailedAuthn(t *testing.T, ev *auditv2pb.AuditEvent, wantCode code.Code) {
	t.Helper()
	if ev.GetAuthentication().GetAuthenticated() {
		t.Error("authentication.authenticated = true, want false")
	}
	if ev.GetStatus() != auditv2pb.Status_ERROR {
		t.Errorf("status = %v, want ERROR", ev.GetStatus())
	}
	if got := ev.GetResponse().GetStatusCode(); got != wantCode {
		t.Errorf("response.status_code = %v, want %v (message %q)", got, wantCode, ev.GetResponse().GetErrorMessage())
	}
	if ev.GetResponse().GetErrorMessage() == "" {
		t.Error("response.error_message is empty, want the reason")
	}
	if got := ev.GetRequest().GetClientIp(); got != "127.0.0.1" {
		t.Errorf("request.client_ip = %q, want 127.0.0.1", got)
	}
	if ev.GetRequest().GetUserAgent() == "" {
		t.Error("request.user_agent is empty")
	}
	if ev.GetId() == "" || ev.GetTime() == nil {
		t.Errorf("event not stamped: id %q time %v", ev.GetId(), ev.GetTime())
	}
}

// assertNotInEvent fails if any of secrets appears in the event's JSON.
func assertNotInEvent(t *testing.T, ev *auditv2pb.AuditEvent, secrets ...string) {
	t.Helper()
	js, err := protojson.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	for _, s := range secrets {
		if s != "" && bytes.Contains(js, []byte(s)) {
			t.Errorf("event contains %q:\n%s", s, js)
		}
	}
}

func hierarchyIDs(ev *auditv2pb.AuditEvent) []string {
	var ids []string
	for _, h := range ev.GetResource().GetHierarchy() {
		ids = append(ids, h.GetId())
	}
	return ids
}

// postExchange posts form to the HTTP exchange endpoint with authnUserAgent.
func postExchange(t *testing.T, addr string, form url.Values) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/oauth2/token/exchange",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", authnUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST token exchange: %v", err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("HTTP exchange response has no X-Request-Id")
	}
	return resp.StatusCode
}

// TestAuditAuthnFailedBearerTokens: every request the edge refuses for its
// bearer token is an event, whatever the method (reads included).
func TestAuditAuthnFailedBearerTokens(t *testing.T) {
	t.Parallel()
	env := startAuthnAudit(t)

	t.Run("unknown static token", func(t *testing.T) {
		t.Parallel()
		const bogus = "nmock-dev-adm1n-typo"
		_, err := saAuthProfileID(t, env.inst.SDKAs(t, bogus))
		assertCode(t, err, codes.Unauthenticated)

		evs := env.failures(t, "authentication.token_credential.masked_token:'***' AND type = 'ai.nebius.iam.profile.get'")
		var ev *auditv2pb.AuditEvent
		for _, e := range evs {
			if strings.HasPrefix(e.GetAuthentication().GetTokenCredential().GetMaskedToken(), "nmock-") {
				ev = e
			}
		}
		if ev == nil {
			t.Fatalf("no event for the unknown token among %d", len(evs))
		}
		assertFailedAuthn(t, ev, code.Code_UNAUTHENTICATED)
		assertNotInEvent(t, ev, bogus)
		if ev.GetSource() != "nebius.iam.v1.ProfileService/Get" || ev.GetAction() != "GET" || ev.GetService().GetName() != "IAM" {
			t.Errorf("source/action/service = %q/%q/%q", ev.GetSource(), ev.GetAction(), ev.GetService().GetName())
		}
		if got := ev.GetAuthentication().GetAuthenticationType(); got != auditv2pb.AuthenticationType_ACCESS_TOKEN {
			t.Errorf("authentication_type = %v, want ACCESS_TOKEN", got)
		}
		if ev.GetRequest().GetParameters() != nil {
			t.Errorf("request.parameters = %v, want none for an unauthenticated call", ev.GetRequest().GetParameters())
		}
		if !slices.Contains(hierarchyIDs(ev), env.inst.TenantID) {
			t.Errorf("hierarchy %v lacks the tenant", hierarchyIDs(ev))
		}
	})

	t.Run("missing token", func(t *testing.T) {
		t.Parallel()
		_, err := iamsvc.NewGroupService(env.inst.SDKAs(t, "")).List(t.Context(), &iampb.ListGroupsRequest{ParentId: env.inst.TenantID})
		assertCode(t, err, codes.Unauthenticated)
		evs := env.failures(t, "type = 'ai.nebius.iam.group.list' AND status = 'ERROR'")
		if len(evs) == 0 {
			t.Fatal("no event for the call without a token")
		}
		ev := evs[len(evs)-1]
		assertFailedAuthn(t, ev, code.Code_UNAUTHENTICATED)
		if ev.GetAuthentication().GetCredential() != nil {
			t.Errorf("credential = %v, want none", ev.GetAuthentication().GetCredential())
		}
		if ev.GetResource().GetMetadata().GetId() != "" || !slices.Contains(hierarchyIDs(ev), env.inst.TenantID) {
			t.Errorf("resource = %v, want the tenant as parent", ev.GetResource())
		}
	})

	t.Run("revoked token of a deactivated key", func(t *testing.T) {
		t.Parallel()
		saID := env.newSA(t, "revoked-token")
		key := env.newKey(t, saID, nil)
		token := saAuthMustExchange(t, env.inst, saAuthForm(key.assertion(t, clock.Real{}.Now().Add(5*time.Minute))))
		env.deactivate(t, key.id)
		saAuthWantTokenRejected(t, env.inst, token)

		evs := env.failures(t, fmt.Sprintf("authentication.subject.service_account_id = '%s' AND type = 'ai.nebius.iam.profile.get'", saID))
		if len(evs) != 1 {
			t.Fatalf("got %d events for the revoked token, want 1", len(evs))
		}
		ev := evs[0]
		assertFailedAuthn(t, ev, code.Code_UNAUTHENTICATED)
		if got := ev.GetAuthentication().GetTokenCredential().GetMaskedToken(); got != unsigned(token) {
			t.Errorf("masked_token = %q, want the token without its MAC %q", got, unsigned(token))
		}
		if !strings.Contains(ev.GetResponse().GetErrorMessage(), "revoked") {
			t.Errorf("error_message = %q, want it to say revoked", ev.GetResponse().GetErrorMessage())
		}
		params := ev.GetRequest().GetParameters().GetFields()
		if got := params["token_key_id"].GetStringValue(); got != key.id {
			t.Errorf("parameters.token_key_id = %q, want %s", got, key.id)
		}
		assertNotInEvent(t, ev, token, signatureOf(token))

		// The filter language finds it by the masked token too.
		if evs := env.failures(t, fmt.Sprintf("authentication.token_credential.masked_token = '%s'", unsigned(token))); len(evs) == 0 {
			t.Error("masked_token filter found nothing")
		}
	})
}

// TestAuditAuthnExpiredToken: an expired issued token (on the mocker clock).
func TestAuditAuthnExpiredToken(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	clk := clock.NewManual(t0)
	env := startAuthnAudit(t, testkit.WithClock(clk))
	saID := env.newSA(t, "expiring")
	key := env.newKey(t, saID, timestamppb.New(t0.Add(24*time.Hour)))
	token := saAuthMustExchange(t, env.inst, saAuthForm(key.assertion(t, clk.Now().Add(5*time.Minute))))
	clk.Advance(saAuthTokenTTL + time.Minute)
	saAuthWantTokenRejected(t, env.inst, token)

	// The key itself expires too: the next exchange is refused as well.
	clk.Advance(24 * time.Hour)
	_, err := saAuthExchange(t, env.inst, saAuthForm(key.assertion(t, clk.Now().Add(5*time.Minute))))
	assertCode(t, err, codes.Unauthenticated)

	req := &auditv2pb.ListAuditEventRequest{
		ParentId: env.inst.TenantID, Region: auditRegion, PageSize: 500,
		Filter: fmt.Sprintf("authentication.subject.service_account_id = '%s' AND status = 'ERROR'", saID),
		Start:  timestamppb.New(t0), End: timestamppb.New(clk.Now().Add(time.Hour)),
	}
	resp, err := env.events.List(t.Context(), req)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sawToken, sawKey bool
	for _, ev := range resp.GetItems() {
		assertFailedAuthn(t, ev, code.Code_UNAUTHENTICATED)
		msg := ev.GetResponse().GetErrorMessage()
		switch ev.GetSource() {
		case "nebius.iam.v1.ProfileService/Get":
			sawToken = strings.Contains(msg, "expired")
		case "nebius.iam.v1.TokenExchangeService/Exchange":
			sawKey = ev.GetResource().GetMetadata().GetId() == key.id && strings.Contains(msg, "EXPIRED")
		}
	}
	if !sawToken || !sawKey {
		t.Errorf("expired token event %v, expired key event %v, in %d events", sawToken, sawKey, len(resp.GetItems()))
	}
}

// TestAuditAuthnFailedExchanges: every refused exchange, over gRPC and over
// HTTP, is an event naming the presented key (or the claimed account), the
// service account, the reason, the client IP and the user agent, and never
// the JWT's signature.
func TestAuditAuthnFailedExchanges(t *testing.T) {
	t.Parallel()
	env := startAuthnAudit(t)
	now := clock.Real{}.Now()
	exp := now.Add(5 * time.Minute)

	type exchange struct {
		form     url.Values
		token    string // the presented JWT or actor token
		resource string // expected resource.metadata.id
		kid      string // expected parameters.kid
		subject  string // expected claimed service account
		grpcCode code.Code
		oauth    string
		reason   string // substring of error_message
	}
	tests := []struct {
		name  string
		setup func(t *testing.T) exchange
	}{
		{"unknown key", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "unknown-key")
			const kid = "publickey-e00neverexisted00"
			tok := saAuthSign(t, jwt.SigningMethodRS256, saAuthGenerate(t), kid, saAuthClaims(saID, exp))
			return exchange{
				form: saAuthForm(tok), token: tok, resource: kid, kid: kid, subject: saID,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "does not exist",
			}
		}},
		{"deactivated key", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "deactivated-key")
			key := env.newKey(t, saID, nil)
			env.deactivate(t, key.id)
			tok := key.assertion(t, exp)
			return exchange{
				form: saAuthForm(tok), token: tok, resource: key.id, kid: key.id, subject: saID,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "INACTIVE",
			}
		}},
		{"deleted key", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "deleted-key")
			key := env.newKey(t, saID, nil)
			env.deleteKey(t, key.id)
			tok := key.assertion(t, exp)
			return exchange{
				form: saAuthForm(tok), token: tok, resource: key.id, kid: key.id, subject: saID,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "does not exist",
			}
		}},
		{"bad signature", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "bad-signature")
			key := env.newKey(t, saID, nil)
			tok := saAuthSign(t, jwt.SigningMethodRS256, saAuthGenerate(t), key.id, saAuthClaims(saID, exp))
			return exchange{
				form: saAuthForm(tok), token: tok, resource: key.id, kid: key.id, subject: saID,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "verification error",
			}
		}},
		{"wrong iss and sub", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "wrong-sub-owner")
			other := env.newSA(t, "wrong-sub-claimed")
			key := env.newKey(t, saID, nil)
			tok := saAuthSign(t, jwt.SigningMethodRS256, key.priv, key.id, saAuthClaims(other, exp))
			return exchange{
				form: saAuthForm(tok), token: tok, resource: key.id, kid: key.id, subject: other,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "iss and sub must be",
			}
		}},
		{"expired JWT", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "expired-jwt")
			key := env.newKey(t, saID, nil)
			tok := key.assertion(t, now.Add(-time.Hour))
			return exchange{
				form: saAuthForm(tok), token: tok, resource: key.id, kid: key.id, subject: saID,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "expired",
			}
		}},
		{"alg none", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "alg-none")
			key := env.newKey(t, saID, nil)
			tok := saAuthSign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, key.id, saAuthClaims(saID, exp))
			return exchange{
				form: saAuthForm(tok), token: tok, resource: key.id, kid: key.id, subject: saID,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "RS256",
			}
		}},
		{"federated: untrusted issuer", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "federated-untrusted")
			tok := saAuthSign(t, jwt.SigningMethodRS256, saAuthGenerate(t), "external-kid-1", jwt.MapClaims{
				"iss": "https://untrusted-issuer.example", "sub": "repo:evil/ci", "exp": exp.Unix(),
			})
			form := url.Values{
				"grant_type": {saAuthGrantType}, "requested_token_type": {saAuthAccessType},
				"subject_token": {saID}, "subject_token_type": {saAuthSubjectType},
				"actor_token": {tok}, "actor_token_type": {saAuthJWTType},
			}
			return exchange{
				form: form, token: tok, resource: saID, kid: "external-kid-1", subject: saID,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "untrusted-issuer.example",
			}
		}},
		{"impersonation: bad actor token", func(t *testing.T) exchange {
			t.Helper()
			saID := env.newSA(t, "impersonated")
			const actor = "stolen-or-made-up-token"
			form := url.Values{
				"grant_type": {saAuthGrantType}, "requested_token_type": {saAuthAccessType},
				"subject_token": {saID}, "subject_token_type": {saAuthSubjectType},
				"actor_token": {actor}, "actor_token_type": {saAuthAccessType},
			}
			return exchange{
				form: form, token: actor, resource: saID, subject: saID,
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "actor_token",
			}
		}},
		{"malformed JWT", func(t *testing.T) exchange {
			t.Helper()
			const tok = "not-a-jwt"
			return exchange{
				form: saAuthForm(tok), token: tok, resource: "",
				grpcCode: code.Code_UNAUTHENTICATED, oauth: "invalid_grant", reason: "subject_token",
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			x := tt.setup(t)
			_, err := saAuthExchange(t, env.inst, x.form)
			assertCode(t, err, codes.Code(x.grpcCode))
			if got := postExchange(t, env.inst.Addr, x.form); got != http.StatusBadRequest {
				t.Fatalf("HTTP status = %d, want 400", got)
			}

			filter := fmt.Sprintf("resource.metadata.id = '%s' AND action = 'EXCHANGE'", x.resource)
			if x.resource == "" {
				filter = "action = 'EXCHANGE' AND resource.metadata.type = 'tokenexchange'"
			}
			bySource := map[string]*auditv2pb.AuditEvent{}
			for _, ev := range env.failures(t, filter) {
				if x.resource != "" || ev.GetAuthentication().GetTokenCredential().GetMaskedToken() != "" {
					bySource[ev.GetSource()] = ev
				}
			}
			grpcEv, httpEv := bySource["nebius.iam.v1.TokenExchangeService/Exchange"], bySource["oauth2/token/exchange"]
			if grpcEv == nil || httpEv == nil {
				t.Fatalf("events by source = %v, want one gRPC and one HTTP", bySource)
			}
			for _, ev := range []*auditv2pb.AuditEvent{grpcEv, httpEv} {
				assertFailedAuthn(t, ev, x.grpcCode)
				if ev.GetService().GetName() != "IAM" || ev.GetAction() != "EXCHANGE" {
					t.Errorf("service/action = %q/%q, want IAM/EXCHANGE", ev.GetService().GetName(), ev.GetAction())
				}
				wantType := "ai.nebius.iam.tokenexchange.exchange"
				if kind, _, ok := strings.Cut(x.resource, "-"); ok {
					wantType = "ai.nebius.iam." + kind + ".exchange"
				}
				if ev.GetType() != wantType {
					t.Errorf("type = %q, want %q", ev.GetType(), wantType)
				}
				if !strings.Contains(ev.GetResponse().GetErrorMessage(), x.reason) {
					t.Errorf("error_message = %q, want it to contain %q", ev.GetResponse().GetErrorMessage(), x.reason)
				}
				if got := ev.GetAuthentication().GetSubject().GetServiceAccountId(); got != x.subject {
					t.Errorf("subject.service_account_id = %q, want %q", got, x.subject)
				}
				if got := ev.GetRequest().GetParameters().GetFields()["kid"].GetStringValue(); got != x.kid {
					t.Errorf("parameters.kid = %q, want %q", got, x.kid)
				}
				masked := ev.GetAuthentication().GetTokenCredential().GetMaskedToken()
				if strings.Count(x.token, ".") == 2 && masked != unsigned(x.token) {
					t.Errorf("masked_token = %q, want the JWT without signature", masked)
				}
				if x.resource != "" && !slices.Contains(hierarchyIDs(ev), env.inst.TenantID) {
					t.Errorf("hierarchy %v lacks the tenant", hierarchyIDs(ev))
				}
				secrets := []string{x.token}
				if strings.Count(x.token, ".") == 2 && signatureOf(x.token) != "" {
					secrets = append(secrets, signatureOf(x.token))
				}
				assertNotInEvent(t, ev, secrets...)
			}
			for _, ev := range []*auditv2pb.AuditEvent{grpcEv, httpEv} {
				if ev.GetRequest().GetRequestId() == "" {
					t.Errorf("%s event has no request_id", ev.GetSource())
				}
			}
			if got := httpEv.GetRequest().GetUserAgent(); got != authnUserAgent {
				t.Errorf("HTTP user_agent = %q, want %q", got, authnUserAgent)
			}
			if got := httpEv.GetResponse().GetPayload().GetFields()["error"].GetStringValue(); got != x.oauth {
				t.Errorf("HTTP payload.error = %q, want %q", got, x.oauth)
			}
		})
	}
}

// TestAuditAuthnCanaryKey is the canary flow through the Go SDK: a planted
// key that was never activated for use (deactivated right away) is used from
// a gosdk credentials file; the security team finds the event by key ID.
func TestAuditAuthnCanaryKey(t *testing.T) {
	t.Parallel()
	env := startAuthnAudit(t)
	saID := env.newSA(t, "canary-holder")
	canary := env.newKey(t, saID, nil)
	env.deactivate(t, canary.id)

	creds := canary.credentialsFile(t)
	sdk := saAuthSDK(t, env.inst, auth.NewServiceAccountCredentialsFileParser(nil, creds))
	_, err := saAuthProfileID(t, sdk)
	if err == nil {
		t.Fatal("ProfileService.Get with the canary key succeeded, want an authentication error")
	}

	evs := env.failures(t, fmt.Sprintf("resource.metadata.id = '%s' AND action = 'EXCHANGE'", canary.id))
	if len(evs) == 0 {
		t.Fatal("no event names the canary key")
	}
	first := evs[0]
	assertFailedAuthn(t, first, code.Code_UNAUTHENTICATED)
	if first.GetSource() != "nebius.iam.v1.TokenExchangeService/Exchange" || first.GetResource().GetMetadata().GetType() != "publickey" {
		t.Errorf("source %q, resource type %q", first.GetSource(), first.GetResource().GetMetadata().GetType())
	}
	if got := first.GetAuthentication().GetSubject().GetServiceAccountId(); got != saID {
		t.Errorf("subject.service_account_id = %q, want %s", got, saID)
	}
	// The project scope finds it as well (the key's hierarchy).
	if evs := listEvents(t, env.events, env.inst.ProjectID, fmt.Sprintf("resource.metadata.id = '%s' AND action = 'EXCHANGE'", canary.id)); len(evs) == 0 {
		t.Error("List scoped to the key's project found nothing")
	}
	assertNotInEvent(t, first, string(saAuthPrivatePEM(t, canary.priv)))
}

// TestAuditAuthnUntrustedInputIsClipped: an unauthenticated caller cannot
// grow an event without bound, and a flood stays within the log capacity.
func TestAuditAuthnUntrustedInputIsClipped(t *testing.T) {
	t.Parallel()
	const capacity = 25
	env := startAuthnAudit(t, testkit.WithConfig(func(c *config.Config) { c.Audit.MaxEvents = capacity }))
	saID := env.newSA(t, "flooded")
	hugeKid := "publickey-" + strings.Repeat("k", 10_000)
	tok := saAuthSign(t, jwt.SigningMethodRS256, saAuthGenerate(t), hugeKid, saAuthClaims(saID, clock.Real{}.Now().Add(time.Minute)))
	for range 3 * capacity {
		_, err := saAuthExchange(t, env.inst, saAuthForm(tok))
		assertCode(t, err, codes.Unauthenticated)
	}
	evs := env.failures(t, "action = 'EXCHANGE'")
	if len(evs) == 0 || len(evs) > capacity {
		t.Fatalf("got %d events, want 1..%d (the log capacity)", len(evs), capacity)
	}
	ev := evs[len(evs)-1]
	js, err := json.Marshal(ev.GetRequest().GetParameters().AsMap())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ev.GetResource().GetMetadata().GetId()); n > 256 {
		t.Errorf("resource id is %d bytes, want it clipped", n)
	}
	if n := len(ev.GetAuthentication().GetTokenCredential().GetMaskedToken()); n > 2048 {
		t.Errorf("masked token is %d bytes, want it clipped", n)
	}
	if n := len(ev.GetResponse().GetErrorMessage()); n > 1024 {
		t.Errorf("error message is %d bytes, want it clipped", n)
	}
	if len(js) > 4096 {
		t.Errorf("parameters are %d bytes, want them clipped", len(js))
	}
}
