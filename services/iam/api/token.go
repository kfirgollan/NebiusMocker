package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// TokenExchangePath is the HTTP OAuth 2.0 token exchange endpoint (RFC 8693),
// served on the edge port like Nebius serves it on auth.<region>.nebius.com.
const TokenExchangePath = "/oauth2/token/exchange" //nolint:gosec // G101: a URL path, not a credential

// maxFormBytes bounds the HTTP exchange request body.
const maxFormBytes = 64 << 10

// TokenExchangeHandler implements nebius.iam.v1.TokenExchangeService (gRPC)
// and POST /oauth2/token/exchange (HTTP); both use the same domain path.
type TokenExchangeHandler struct {
	iampb.UnimplementedTokenExchangeServiceServer
	svc   *domain.TokenExchangeService
	audit AuthnFailureRecorder
	log   *slog.Logger
}

var _ iampb.TokenExchangeServiceServer = (*TokenExchangeHandler)(nil)

// AuthnFailureRecorder records refused exchanges as audit events (satisfied
// by *audit.AuthnRecorder).
type AuthnFailureRecorder interface {
	RecordAuthnFailure(ctx context.Context, f audit.AuthnFailure)
}

// NewTokenExchangeHandler returns a handler backed by svc that records every
// refused exchange through rec (nil records nothing).
func NewTokenExchangeHandler(svc *domain.TokenExchangeService, rec AuthnFailureRecorder, log *slog.Logger) *TokenExchangeHandler {
	return &TokenExchangeHandler{svc: svc, audit: rec, log: log}
}

// Audit sources of refused exchanges: the gRPC method, and the HTTP
// endpoint (named like data-plane sources, "<protocol>/<operation>").
const (
	exchangeSourceGRPC = "nebius.iam.v1.TokenExchangeService/Exchange"
	exchangeSourceHTTP = "oauth2/token/exchange"
)

// client is the transport view of the caller, for audit events.
type client struct {
	ip, userAgent, requestID string
}

func grpcClient(ctx context.Context) client {
	md, _ := metadata.FromIncomingContext(ctx)
	c := client{requestID: reqctx.RequestID(ctx)}
	if ua := md.Get("user-agent"); len(ua) > 0 {
		c.userAgent = ua[0]
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		c.ip = hostOnly(p.Addr.String())
	}
	return c
}

func httpClient(r *http.Request, requestID string) client {
	return client{ip: hostOnly(r.RemoteAddr), userAgent: r.UserAgent(), requestID: requestID}
}

// requestIDHeader carries the request ID of an HTTP exchange, as the gRPC
// edge returns x-request-id.
const requestIDHeader = "X-Request-Id"

// newRequestID returns a random request ID in the edge's format (16 hex digits).
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// hostOnly strips the port from "ip:port" (and brackets from IPv6).
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// recordFailure records the refused exchange req as a failed
// authentication: the presented key or account, the claimed service
// account, the masked token and the refusal. err is the domain error.
func (h *TokenExchangeHandler) recordFailure(ctx context.Context, source string, c client, req domain.ExchangeRequest, err error, payload map[string]string) {
	if h.audit == nil {
		return
	}
	p := domain.PresentedBy(req)
	st := serviceerror.StatusProto(ctx, err)
	params := map[string]string{
		"grant_type": req.GrantType, "requested_token_type": req.RequestedTokenType,
		"subject_token_type": req.SubjectTokenType, "actor_token_type": req.ActorTokenType,
		"service_account_id": p.ServiceAccountID,
		"kid":                p.KeyID, "alg": p.Algorithm, "iss": p.Issuer, "sub": p.Subject,
	}
	h.audit.RecordAuthnFailure(ctx, audit.AuthnFailure{
		Source: source, Area: "iam", ServiceName: "IAM", Action: "EXCHANGE",
		ResourceID: p.CredentialID, ResourceType: "tokenexchange",
		Token: p.Token, Subject: p.ServiceAccountID, Parameters: params,
		ClientIP: c.ip, UserAgent: c.userAgent, RequestID: c.requestID,
		Code: code.Code(st.GetCode()), Message: st.GetMessage(), Payload: payload,
	})
}

// Exchange exchanges a service account JWT (or an impersonation request) for
// an access token.
func (h *TokenExchangeHandler) Exchange(ctx context.Context, req *iampb.ExchangeTokenRequest) (*iampb.CreateTokenResponse, error) {
	res, err := h.svc.Exchange(ctx, exchangeRequest(req))
	if err != nil {
		h.recordFailure(ctx, exchangeSourceGRPC, grpcClient(ctx), exchangeRequest(req), err, nil)
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.CreateTokenResponse{
		AccessToken: res.AccessToken, IssuedTokenType: res.IssuedTokenType, TokenType: res.TokenType,
		ExpiresIn: int64(res.ExpiresIn.Seconds()),
	}, nil
}

func exchangeRequest(req *iampb.ExchangeTokenRequest) domain.ExchangeRequest {
	return domain.ExchangeRequest{
		GrantType: req.GetGrantType(), RequestedTokenType: req.GetRequestedTokenType(),
		SubjectToken: req.GetSubjectToken(), SubjectTokenType: req.GetSubjectTokenType(),
		Scopes: req.GetScopes(), Audience: req.GetAudience(),
		ActorToken: req.GetActorToken(), ActorTokenType: req.GetActorTokenType(), Resource: req.GetResource(),
	}
}

// tokenResponse is the RFC 8693 §2.2.1 success body.
type tokenResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
}

// oauthServerError is the RFC 6749 §5.2 code of an internal failure.
const oauthServerError domain.OAuthErrorCode = "server_error"

// oauthPayload is the response detail of a refused HTTP exchange.
func oauthPayload(httpStatus int, c domain.OAuthErrorCode) map[string]string {
	return map[string]string{"http_status": strconv.Itoa(httpStatus), "error": string(c)}
}

// oauthError is the RFC 6749 §5.2 error body.
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// ServeHTTP handles POST /oauth2/token/exchange with an
// application/x-www-form-urlencoded body.
func (h *TokenExchangeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rid := newRequestID()
	w.Header().Set(requestIDHeader, rid)
	c := httpClient(r, rid)
	req, ferr := exchangeForm(w, r)
	if ferr != nil {
		h.recordFailure(ctx, exchangeSourceHTTP, c, req, ferr, oauthPayload(http.StatusBadRequest, domain.OAuthInvalidRequest))
		writeOAuthError(w, http.StatusBadRequest, domain.OAuthInvalidRequest, ferr.Error())
		return
	}
	res, err := h.svc.Exchange(ctx, req)
	if xe, ok := errors.AsType[*domain.ExchangeError](err); ok {
		h.recordFailure(ctx, exchangeSourceHTTP, c, req, err, oauthPayload(http.StatusBadRequest, xe.Code))
		writeOAuthError(w, http.StatusBadRequest, xe.Code, xe.Description())
		return
	}
	if err != nil {
		h.log.ErrorContext(ctx, "token exchange", "error", err)
		h.recordFailure(ctx, exchangeSourceHTTP, c, req, err, oauthPayload(http.StatusInternalServerError, oauthServerError))
		writeOAuthError(w, http.StatusInternalServerError, oauthServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: res.AccessToken, IssuedTokenType: res.IssuedTokenType, TokenType: res.TokenType,
		ExpiresIn: int64(res.ExpiresIn.Seconds()),
	})
}

// singleValued are the form parameters that must not repeat (RFC 6749 §3.2);
// resource may (RFC 8693 §2.1). Unrecognized parameters are ignored, as RFC
// 6749 §3.2 requires.
var singleValued = []string{
	"grant_type", "requested_token_type", "subject_token", "subject_token_type",
	"scope", "audience", "actor_token", "actor_token_type",
}

func exchangeForm(w http.ResponseWriter, r *http.Request) (domain.ExchangeRequest, error) {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/x-www-form-urlencoded" {
		return domain.ExchangeRequest{}, errs.BadRequest(errs.Violation{
			Field: "Content-Type", Message: "must be application/x-www-form-urlencoded",
		})
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return domain.ExchangeRequest{}, errs.BadRequest(errs.Violation{Message: "malformed form body: " + err.Error()})
	}
	form := r.PostForm
	for _, name := range singleValued {
		if len(form[name]) > 1 {
			return domain.ExchangeRequest{}, errs.BadRequest(errs.Violation{Field: name, Message: "must not be repeated"})
		}
	}
	if len(r.URL.Query()) > 0 {
		return domain.ExchangeRequest{}, errs.BadRequest(errs.Violation{Message: "parameters must be sent in the form body, not the URL"})
	}
	return domain.ExchangeRequest{
		GrantType: form.Get("grant_type"), RequestedTokenType: form.Get("requested_token_type"),
		SubjectToken: form.Get("subject_token"), SubjectTokenType: form.Get("subject_token_type"),
		Scopes: strings.Fields(form.Get("scope")), Audience: form.Get("audience"),
		ActorToken: form.Get("actor_token"), ActorTokenType: form.Get("actor_token_type"),
		Resource: nonEmpty(form, "resource"),
	}, nil
}

func nonEmpty(form url.Values, name string) []string {
	var out []string
	for _, v := range form[name] {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func writeOAuthError(w http.ResponseWriter, status int, oc domain.OAuthErrorCode, desc string) {
	writeJSON(w, status, oauthError{Error: string(oc), ErrorDescription: desc})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the status is sent; a write error means the client went away
}
