package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/ai/api"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

// fakeCalls is an api.CallAuthorizer that records its arguments.
type fakeCalls struct {
	call domain.Call
	err  error

	target, authorization string
}

func (f *fakeCalls) AuthorizeCall(_ context.Context, target, authorization string) (domain.Call, error) {
	f.target, f.authorization = target, authorization
	return f.call, f.err
}

func newDataPlane() *api.DataPlane { return api.NewDataPlane(slog.New(slog.DiscardHandler)) }

// started starts a data plane on a loopback port and stops it at the
// end of the test, then returns it.
func started(t *testing.T, calls api.CallAuthorizer, publicURL string) *api.DataPlane {
	t.Helper()
	dp := newDataPlane()
	if err := dp.Start(t.Context(), calls, "127.0.0.1:0", publicURL); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := dp.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return dp
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
	dp := newDataPlane()
	if err := dp.Start(t.Context(), calls, "127.0.0.1:0", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	base := dp.BaseURL()
	if base == "" || base == "http://127.0.0.1:0" {
		t.Fatalf("BaseURL = %q, want the bound address", base)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/aiendpoint-1/x/y", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
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
	if err := dp.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
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
