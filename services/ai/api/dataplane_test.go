package api_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	"github.com/kfirgollan/NebiusMocker/services/ai/api"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

// fakeCalls is an api.CallAuthorizer that records its arguments.
type fakeCalls struct {
	call domain.Call
	err  error

	target, authorization string

	tcpCall domain.Call
	tcpErr  error
	tcpID   string
	tcpPort int32
}

func (f *fakeCalls) AuthorizeTCP(_ context.Context, endpointID string, port int32) (domain.Call, error) {
	f.tcpID, f.tcpPort = endpointID, port
	return f.tcpCall, f.tcpErr
}

func (f *fakeCalls) AuthorizeCall(_ context.Context, target, authorization string) (domain.Call, error) {
	f.target, f.authorization = target, authorization
	return f.call, f.err
}

func newDataPlane() *api.DataPlane { return api.NewDataPlane(slog.New(slog.DiscardHandler)) }

const tcpDomain = "ai.localhost"

// testTLS is a local CA and a data-plane config signed by it.
func testTLS(t *testing.T, publicURL string) (api.DataPlaneConfig, *x509.CertPool) {
	t.Helper()
	ca, err := localca.LoadOrCreate(t.TempDir(), clock.Real{})
	if err != nil {
		t.Fatalf("localca: %v", err)
	}
	cert, err := ca.Issue("127.0.0.1", "localhost", "*."+tcpDomain)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return api.DataPlaneConfig{
		Listen: "127.0.0.1:0", PublicURL: publicURL, TCPListen: "127.0.0.1:0", TCPDomain: tcpDomain, Cert: cert,
	}, ca.Pool()
}

// started starts a data plane on a loopback port and stops it at the
// end of the test, then returns it.
func started(t *testing.T, calls api.CallAuthorizer, publicURL string) *api.DataPlane {
	t.Helper()
	dp, _ := startedTLS(t, calls, publicURL)
	return dp
}

// startedTLS is started, also returning the pool that trusts its certificate.
func startedTLS(t *testing.T, calls api.CallAuthorizer, publicURL string) (*api.DataPlane, *x509.CertPool) {
	t.Helper()
	cfg, pool := testTLS(t, publicURL)
	dp := newDataPlane()
	if err := dp.Start(t.Context(), calls, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := dp.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return dp, pool
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body %q is not JSON: %v", rec.Body.String(), err)
	}
	return body
}

func TestServeHTTPSuccess(t *testing.T) {
	t.Parallel()
	calls := &fakeCalls{call: domain.Call{EndpointID: "aiendpoint-1", Port: 8080}}
	dp := started(t, calls, "")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/aiendpoint-1:8080/v1/chat?x=1", http.NoBody)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	dp.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	want := map[string]any{
		"message": "endpoint was called", "endpoint_id": "aiendpoint-1", "port": float64(8080),
		"method": "POST", "path": "/v1/chat", "query": "x=1",
	}
	if d := cmp.Diff(want, decode(t, rec)); d != "" {
		t.Errorf("body mismatch (-want +got):\n%s", d)
	}
	if calls.target != "aiendpoint-1:8080" || calls.authorization != "Bearer tok" {
		t.Errorf("AuthorizeCall(%q, %q), want (aiendpoint-1:8080, Bearer tok)", calls.target, calls.authorization)
	}
}

func TestServeHTTPRootOfEndpoint(t *testing.T) {
	t.Parallel()
	dp := started(t, &fakeCalls{call: domain.Call{EndpointID: "aiendpoint-1", Port: 80}}, "")
	rec := httptest.NewRecorder()
	dp.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/aiendpoint-1", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decode(t, rec)["path"]; got != "/" {
		t.Errorf("path = %v, want /", got)
	}
}

func TestServeHTTPErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantWWWAuth bool
	}{
		{name: "not found", err: errs.NotFound("aiendpoint-1"), wantStatus: http.StatusNotFound},
		{name: "unauthenticated", err: errs.Unauthenticated("missing bearer token"), wantStatus: http.StatusUnauthorized, wantWWWAuth: true},
		{name: "bad state", err: errs.BadState("aiendpoint-1", "not running"), wantStatus: http.StatusServiceUnavailable},
		{name: "internal", err: errs.Internal("boom"), wantStatus: http.StatusInternalServerError},
		{name: "plain error", err: errors.New("boom"), wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dp := started(t, &fakeCalls{err: tt.err}, "")
			rec := httptest.NewRecorder()
			dp.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/aiendpoint-1/x", http.NoBody))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("WWW-Authenticate") != ""; got != tt.wantWWWAuth {
				t.Errorf("WWW-Authenticate present = %v, want %v", got, tt.wantWWWAuth)
			}
			body := decode(t, rec)
			if body["error"] == "" || body["error"] == nil {
				t.Errorf("error body = %v, want a message", body)
			}
			if body["endpoint_id"] != "aiendpoint-1" {
				t.Errorf("endpoint_id = %v, want aiendpoint-1", body["endpoint_id"])
			}
		})
	}
}

func TestServeHTTPEmptyPath(t *testing.T) {
	t.Parallel()
	calls := &fakeCalls{}
	dp := started(t, calls, "")
	rec := httptest.NewRecorder()
	dp.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if calls.target != "" {
		t.Errorf("AuthorizeCall was called with %q for an empty path", calls.target)
	}
}

func TestServeHTTPBeforeStart(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	newDataPlane().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/aiendpoint-1", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestStartServesRealRequests(t *testing.T) {
	t.Parallel()
	calls := &fakeCalls{call: domain.Call{EndpointID: "aiendpoint-1", Port: 80}}
	dp, pool := startedTLS(t, calls, "")
	base := dp.BaseURL()
	if !strings.HasPrefix(base, "https://127.0.0.1:") || base == "https://127.0.0.1:0" {
		t.Fatalf("BaseURL = %q, want https on the bound address", base)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	t.Cleanup(client.CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/aiendpoint-1/x/y", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["path"] != "/x/y" || body["endpoint_id"] != "aiendpoint-1" || body["message"] != "endpoint was called" {
		t.Errorf("body = %v", body)
	}

	// A client that does not trust the CA is refused.
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/aiendpoint-1", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Error("GET without the CA succeeded, want a certificate error")
	}
}

// tcpHostPort extracts the loopback host:port of a tls:// address; the
// server name is set on the dial, so no DNS is needed.
func tcpHostPort(t *testing.T, addr string) string {
	t.Helper()
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "tls" {
		t.Fatalf("TCPURL = %q, want a tls:// address (%v)", addr, err)
	}
	return net.JoinHostPort("127.0.0.1", u.Port())
}

// dialTCP connects to the TCP stub listener behind addr with the given SNI and
// returns the one line it sends.
func dialTCP(t *testing.T, pool *x509.CertPool, hostPort, serverName string) string {
	t.Helper()
	d := tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(t.Context(), "tcp", hostPort)
	if err != nil {
		t.Fatalf("dial %s as %s: %v", hostPort, serverName, err)
	}
	defer conn.Close()
	line, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(line)
}

func TestTCPStub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		call       domain.Call
		err        error
		serverName string
		want       string
		wantID     string
		wantPort   int32
	}{
		{
			name: "running", call: domain.Call{EndpointID: "aiendpoint-1", Port: 22}, serverName: "aiendpoint-1-p22.ai.localhost",
			want: "endpoint was called: aiendpoint-1 port 22\n", wantID: "aiendpoint-1", wantPort: 22,
		},
		{
			name: "unknown endpoint", err: errs.NotFound("aiendpoint-9"), serverName: "aiendpoint-9-p22.ai.localhost",
			want: "error 404: resource not found\n", wantID: "aiendpoint-9", wantPort: 22,
		},
		{
			name: "stopped", err: errs.BadState("aiendpoint-1", "endpoint is not running"), serverName: "aiendpoint-1-p22.ai.localhost",
			want: "error 503: endpoint is not running\n", wantID: "aiendpoint-1", wantPort: 22,
		},
		{
			name: "server name without endpoint", serverName: "nope.ai.localhost",
			want: "error 404: no endpoint in TLS server name; connect to <endpoint-id>-p<port>.ai.localhost\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls := &fakeCalls{tcpCall: tt.call, tcpErr: tt.err}
			dp, pool := startedTLS(t, calls, "")
			addr := tcpHostPort(t, dp.TCPURL("aiendpoint-1", 22))
			if got := dialTCP(t, pool, addr, tt.serverName); got != tt.want {
				t.Errorf("line = %q, want %q", got, tt.want)
			}
			if calls.tcpID != tt.wantID || calls.tcpPort != tt.wantPort {
				t.Errorf("AuthorizeTCP(%q, %d), want (%q, %d)", calls.tcpID, calls.tcpPort, tt.wantID, tt.wantPort)
			}
		})
	}
}

func TestTCPURL(t *testing.T) {
	t.Parallel()
	dp := started(t, &fakeCalls{}, "")
	u, err := url.Parse(dp.TCPURL("aiendpoint-1", 5432))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "tls" || u.Hostname() != "aiendpoint-1-p5432.ai.localhost" || u.Port() == "" || u.Port() == "0" {
		t.Errorf("TCPURL = %q, want tls://aiendpoint-1-p5432.ai.localhost:<port>", u)
	}
}

func TestParseTCPHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host     string
		wantID   string
		wantPort int32
		wantOK   bool
	}{
		{"aiendpoint-abc-p22.ai.localhost", "aiendpoint-abc", 22, true},
		{"AIENDPOINT-abc-p5432.AI.Localhost", "AIENDPOINT-abc", 5432, true},
		{"a-p1.ai.localhost", "a", 1, true},
		{"aiendpoint-abc-p22.other.test", "", 0, false},
		{"ai.localhost", "", 0, false},
		{".ai.localhost", "", 0, false},
		{"nope.ai.localhost", "", 0, false},
		{"-p22.ai.localhost", "", 0, false},
		{"aiendpoint-abc-p.ai.localhost", "", 0, false},
		{"aiendpoint-abc-p0.ai.localhost", "", 0, false},
		{"aiendpoint-abc-p-5.ai.localhost", "", 0, false},
		{"aiendpoint-abc-pxx.ai.localhost", "", 0, false},
		{"aiendpoint-abc-p99999999999.ai.localhost", "", 0, false},
		{"", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			id, port, ok := api.ParseTCPHost(tt.host, "ai.localhost")
			if id != tt.wantID || port != tt.wantPort || ok != tt.wantOK {
				t.Errorf("parseTCPHost(%q) = (%q, %d, %v), want (%q, %d, %v)", tt.host, id, port, ok, tt.wantID, tt.wantPort, tt.wantOK)
			}
		})
	}
}

func TestStartPublicURL(t *testing.T) {
	t.Parallel()
	dp := started(t, &fakeCalls{}, "https://ai.example.test/")
	if got, want := dp.BaseURL(), "https://ai.example.test"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

func TestStopBeforeStart(t *testing.T) {
	t.Parallel()
	if err := newDataPlane().Stop(t.Context()); err != nil {
		t.Errorf("Stop before Start = %v, want nil", err)
	}
}
