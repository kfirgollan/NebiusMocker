package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/services/iam/api"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

type muxRegistrar struct{ mux *http.ServeMux }

func (m muxRegistrar) HandleHTTP(pattern string, h http.Handler) { m.mux.Handle(pattern, h) }

func newFakeOIDCServer(t *testing.T, clk clock.Clock) *httptest.Server {
	t.Helper()
	issuers, err := domain.NewFakeOIDCIssuers(clk)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.NewFakeOIDCHandler(issuers, slog.New(slog.DiscardHandler)).Register(muxRegistrar{mux})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeOIDCResp is the status and headers of a response whose body was read.
type fakeOIDCResp struct {
	StatusCode int
	Header     http.Header
}

func fakeOIDCDo(t *testing.T, req *http.Request) (resp fakeOIDCResp, body []byte) {
	t.Helper()
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	body, err = io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return fakeOIDCResp{StatusCode: r.StatusCode, Header: r.Header}, body
}

func fakeOIDCGet(t *testing.T, target string) (resp fakeOIDCResp, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	return fakeOIDCDo(t, req)
}

func fakeOIDCPost(t *testing.T, target string, form url.Values) (resp fakeOIDCResp, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return fakeOIDCDo(t, req)
}

func TestFakeOIDCIssuerFlow(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(apT0)
	srv := newFakeOIDCServer(t, clk)
	issuer := srv.URL + api.FakeOIDCPrefix + "ci"

	resp, body := fakeOIDCGet(t, issuer+"/.well-known/openid-configuration")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discovery: %d %s", resp.StatusCode, body)
	}
	var d map[string]any
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	if d["issuer"] != issuer || d["jwks_uri"] != issuer+"/jwks" {
		t.Errorf("discovery = %v, want issuer %s and jwks_uri %s/jwks", d, issuer, issuer)
	}
	for _, k := range []string{"response_types_supported", "subject_types_supported", "id_token_signing_alg_values_supported"} {
		if _, ok := d[k]; !ok {
			t.Errorf("discovery has no %s", k)
		}
	}

	resp, body = fakeOIDCPost(t, issuer+"/token", url.Values{"sub": {"runner"}, "aud": {"nebius"}, "ttl": {"15m"}})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/jwt" {
		t.Fatalf("token: %d %s (%s)", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	j, err := auth.ParseExternalJWT(string(body))
	if err != nil {
		t.Fatalf("token body is not a bare JWT: %v (%q)", err, body)
	}
	if j.Issuer != issuer || j.Subject != "runner" {
		t.Errorf("token = %+v, want iss %s sub runner", j, issuer)
	}

	_, jwks := fakeOIDCGet(t, d["jwks_uri"].(string))
	set, err := auth.ParseJWKSet(jwks)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.VerifyExternalJWT(j, set, clk.Now()); err != nil {
		t.Errorf("token does not verify with the published JWKS: %v", err)
	}
	if err := auth.VerifyExternalJWT(j, set, clk.Now().Add(16*time.Minute)); err == nil {
		t.Error("15m token still valid after 16m")
	}
}

func TestFakeOIDCTokenRefusals(t *testing.T) {
	t.Parallel()
	srv := newFakeOIDCServer(t, clock.NewManual(apT0))
	tokenURL := srv.URL + api.FakeOIDCPrefix + "ci/token"
	for name, form := range map[string]url.Values{
		"no sub":        {},
		"bad ttl":       {"sub": {"s"}, "ttl": {"forever"}},
		"zero ttl":      {"sub": {"s"}, "ttl": {"0s"}},
		"unknown param": {"sub": {"s"}, "scope": {"openid"}},
		"repeated sub":  {"sub": {"a", "b"}},
	} {
		resp, body := fakeOIDCPost(t, tokenURL, form)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid_request") {
			t.Errorf("%s: %d %s, want 400 invalid_request", name, resp.StatusCode, body)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tokenURL, strings.NewReader(`{"sub":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, body := fakeOIDCDo(t, req); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("JSON body: %d %s, want 400", resp.StatusCode, body)
	}
	if resp, body := fakeOIDCGet(t, srv.URL+api.FakeOIDCPrefix+"-bad/jwks"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad issuer name: %d %s, want 400", resp.StatusCode, body)
	}
}
