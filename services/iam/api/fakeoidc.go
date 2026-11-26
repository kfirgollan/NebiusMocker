package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// FakeOIDCPrefix is where the built-in fake OIDC issuers are served on the
// edge port: <scheme>://<host>/_mocker/oidc/<name> is the issuer URL of <name>.
const FakeOIDCPrefix = "/_mocker/oidc/"

// HTTPRegistrar registers plain HTTP handlers (satisfied by the edge server).
type HTTPRegistrar interface {
	HandleHTTP(pattern string, h http.Handler)
}

// FakeOIDCHandler serves the fake OIDC issuers: discovery, JWKS and a token
// endpoint that mints a JWT for any subject.
type FakeOIDCHandler struct {
	issuers *domain.FakeOIDCIssuers
	log     *slog.Logger
}

// NewFakeOIDCHandler returns a handler backed by issuers.
func NewFakeOIDCHandler(issuers *domain.FakeOIDCIssuers, log *slog.Logger) *FakeOIDCHandler {
	return &FakeOIDCHandler{issuers: issuers, log: log}
}

// Register installs the issuer routes on reg.
func (h *FakeOIDCHandler) Register(reg HTTPRegistrar) {
	reg.HandleHTTP("GET "+FakeOIDCPrefix+"{name}/.well-known/openid-configuration", http.HandlerFunc(h.discovery))
	reg.HandleHTTP("GET "+FakeOIDCPrefix+"{name}/jwks", http.HandlerFunc(h.jwks))
	reg.HandleHTTP("POST "+FakeOIDCPrefix+"{name}/token", http.HandlerFunc(h.token))
}

// issuerURL is the issuer of the request's {name}, as the client reached it.
func issuerURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + FakeOIDCPrefix + r.PathValue("name")
}

type fakeOIDCDiscovery struct {
	Issuer             string   `json:"issuer"`
	JWKSURI            string   `json:"jwks_uri"`
	TokenEndpoint      string   `json:"token_endpoint"`
	ResponseTypes      []string `json:"response_types_supported"`
	SubjectTypes       []string `json:"subject_types_supported"`
	IDTokenSigningAlgs []string `json:"id_token_signing_alg_values_supported"`
}

func (h *FakeOIDCHandler) discovery(w http.ResponseWriter, r *http.Request) {
	if _, err := h.issuers.JWKS(r.PathValue("name")); err != nil {
		h.fail(w, r, err)
		return
	}
	iss := issuerURL(r)
	writeJSON(w, http.StatusOK, fakeOIDCDiscovery{
		Issuer: iss, JWKSURI: iss + "/jwks", TokenEndpoint: iss + "/token",
		ResponseTypes: []string{"id_token"}, SubjectTypes: []string{"public"},
		IDTokenSigningAlgs: []string{"RS256"},
	})
}

func (h *FakeOIDCHandler) jwks(w http.ResponseWriter, r *http.Request) {
	body, err := h.issuers.JWKS(r.PathValue("name"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// fakeOIDCTokenParams are the token endpoint's form parameters.
var fakeOIDCTokenParams = map[string]bool{"sub": true, "aud": true, "ttl": true}

// token mints a JWT: form sub (required), aud and ttl (a Go duration,
// default 1h). The body is the bare JWT, so `curl -o token.jwt` writes a
// file the Nebius CLI and SDKs read as federated subject credentials.
func (h *FakeOIDCHandler) token(w http.ResponseWriter, r *http.Request) {
	cmd, err := fakeOIDCTokenForm(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	jwt, err := h.issuers.Mint(r.PathValue("name"), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/jwt")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// The body is a JWT the issuer just signed (base64url segments), served as
	// application/jwt: nothing from the request is reflected into it.
	_, _ = io.WriteString(w, jwt) //nolint:gosec // G705: see above; a write error means the client went away
}

func fakeOIDCTokenForm(w http.ResponseWriter, r *http.Request) (domain.FakeOIDCMintCmd, error) {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/x-www-form-urlencoded" {
		return domain.FakeOIDCMintCmd{}, errs.BadRequest(errs.Violation{
			Field: "Content-Type", Message: "must be application/x-www-form-urlencoded",
		})
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return domain.FakeOIDCMintCmd{}, errs.BadRequest(errs.Violation{Message: "malformed form body: " + err.Error()})
	}
	if len(r.URL.Query()) > 0 {
		return domain.FakeOIDCMintCmd{}, errs.BadRequest(errs.Violation{Message: "parameters must be sent in the form body, not the URL"})
	}
	for name, vs := range r.PostForm {
		if !fakeOIDCTokenParams[name] {
			return domain.FakeOIDCMintCmd{}, errs.BadRequest(errs.Violation{Field: name, Message: "unknown parameter: only sub, aud and ttl are accepted"})
		}
		if len(vs) > 1 {
			return domain.FakeOIDCMintCmd{}, errs.BadRequest(errs.Violation{Field: name, Message: "must not be repeated"})
		}
	}
	cmd := domain.FakeOIDCMintCmd{Issuer: issuerURL(r), Subject: r.PostForm.Get("sub"), Audience: r.PostForm.Get("aud")}
	if v := r.PostForm.Get("ttl"); v != "" {
		ttl, err := time.ParseDuration(v)
		if err != nil || ttl <= 0 {
			return domain.FakeOIDCMintCmd{}, errs.BadRequest(errs.Violation{Field: "ttl", Message: "must be a positive Go duration such as 15m"})
		}
		cmd.TTL = ttl
	}
	return cmd, nil
}

func (h *FakeOIDCHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if e, ok := errors.AsType[*errs.Error](err); ok && e.Kind == errs.KindBadRequest {
		desc := e.Message
		if len(e.Violations) > 0 {
			v := e.Violations[0]
			desc = v.Message
			if v.Field != "" {
				desc = v.Field + ": " + desc
			}
		}
		writeOAuthError(w, http.StatusBadRequest, domain.OAuthInvalidRequest, desc)
		return
	}
	h.log.ErrorContext(r.Context(), "fake OIDC issuer", "error", err)
	writeOAuthError(w, http.StatusInternalServerError, "server_error", "internal error")
}
