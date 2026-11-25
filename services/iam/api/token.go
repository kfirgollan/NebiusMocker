package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
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
	svc *domain.TokenExchangeService
	log *slog.Logger
}

var _ iampb.TokenExchangeServiceServer = (*TokenExchangeHandler)(nil)

// NewTokenExchangeHandler returns a handler backed by svc.
func NewTokenExchangeHandler(svc *domain.TokenExchangeService, log *slog.Logger) *TokenExchangeHandler {
	return &TokenExchangeHandler{svc: svc, log: log}
}

// Exchange exchanges a service account JWT (or an impersonation request) for
// an access token.
func (h *TokenExchangeHandler) Exchange(ctx context.Context, req *iampb.ExchangeTokenRequest) (*iampb.CreateTokenResponse, error) {
	res, err := h.svc.Exchange(ctx, domain.ExchangeRequest{
		GrantType: req.GetGrantType(), RequestedTokenType: req.GetRequestedTokenType(),
		SubjectToken: req.GetSubjectToken(), SubjectTokenType: req.GetSubjectTokenType(),
		Scopes: req.GetScopes(), Audience: req.GetAudience(),
		ActorToken: req.GetActorToken(), ActorTokenType: req.GetActorTokenType(), Resource: req.GetResource(),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.CreateTokenResponse{
		AccessToken: res.AccessToken, IssuedTokenType: res.IssuedTokenType, TokenType: res.TokenType,
		ExpiresIn: int64(res.ExpiresIn.Seconds()),
	}, nil
}

// tokenResponse is the RFC 8693 §2.2.1 success body.
type tokenResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
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
	req, ferr := exchangeForm(w, r)
	if ferr != nil {
		writeOAuthError(w, http.StatusBadRequest, domain.OAuthInvalidRequest, ferr.Error())
		return
	}
	res, err := h.svc.Exchange(ctx, req)
	if xe, ok := errors.AsType[*domain.ExchangeError](err); ok {
		writeOAuthError(w, http.StatusBadRequest, xe.Code, xe.Description())
		return
	}
	if err != nil {
		h.log.ErrorContext(ctx, "token exchange", "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "internal error")
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

func writeOAuthError(w http.ResponseWriter, status int, code domain.OAuthErrorCode, desc string) {
	writeJSON(w, status, oauthError{Error: string(code), ErrorDescription: desc})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the status is sent; a write error means the client went away
}
