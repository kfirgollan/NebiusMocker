package docker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

// fakeAccess accepts token "good"; it denies push on namespace "ro".
type fakeAccess struct {
	mu   sync.Mutex
	seen []domain.AccessRequest
}

func (f *fakeAccess) Authenticate(token string) (string, error) {
	if token != "good" {
		return "", errs.Unauthenticated("bad token")
	}
	return "tenantuseraccount-x", nil
}

func (f *fakeAccess) Authorize(_ context.Context, req domain.AccessRequest) error {
	f.mu.Lock()
	f.seen = append(f.seen, req)
	f.mu.Unlock()
	if _, err := f.Authenticate(req.Token); err != nil {
		return err
	}
	switch {
	case req.Namespace == "missing":
		return errs.NotFound("registry-missing")
	case req.Namespace == "ro" && req.Action != domain.ActionPull:
		return errs.PermissionDenied("registry-ro")
	}
	return nil
}

type upstreamHit struct {
	method, path, host, user, pass string
}

func newFront(t *testing.T) (*httptest.Server, *fakeAccess, *[]upstreamHit) {
	t.Helper()
	var mu sync.Mutex
	var hits []upstreamHit
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		mu.Lock()
		hits = append(hits, upstreamHit{r.Method, r.URL.Path, r.Host, user, pass})
		mu.Unlock()
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "http://harbor.internal:8080/v2/ns/app/blobs/uploads/123?_state=x")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = w.Write([]byte("manifest"))
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	access := &fakeAccess{}
	s, err := New(access, Upstream{URL: u, Username: "admin", Password: "pw"}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	front := httptest.NewServer(s.Handler())
	t.Cleanup(front.Close)
	return front, access, &hits
}

// result is what the tests inspect of a response (the body is already closed).
type result struct {
	code   int
	header http.Header
}

func do(t *testing.T, method, target, token string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.SetBasicAuth("iam", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close() // best-effort: test client
	return result{code: resp.StatusCode, header: resp.Header}
}

func TestPing(t *testing.T) {
	t.Parallel()
	front, _, _ := newFront(t)
	resp := do(t, http.MethodGet, front.URL+"/v2/", "")
	if resp.code != http.StatusUnauthorized || resp.header.Get("WWW-Authenticate") != challenge {
		t.Fatalf("anonymous ping = %d %q, want 401 with Basic challenge", resp.code, resp.header.Get("WWW-Authenticate"))
	}
	if resp.header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Error("missing Docker-Distribution-API-Version header")
	}
	if resp := do(t, http.MethodGet, front.URL+"/v2/", "bad"); resp.code != http.StatusUnauthorized {
		t.Errorf("bad token ping = %d, want 401", resp.code)
	}
	if resp := do(t, http.MethodGet, front.URL+"/v2/", "good"); resp.code != http.StatusOK {
		t.Errorf("good token ping = %d, want 200", resp.code)
	}
}

func TestProxy(t *testing.T) {
	t.Parallel()
	front, access, hits := newFront(t)
	resp := do(t, http.MethodGet, front.URL+"/v2/ns/team/app/manifests/v1", "good")
	if resp.code != http.StatusOK {
		t.Fatalf("pull = %d, want 200", resp.code)
	}
	h := (*hits)[0]
	frontHost := front.Listener.Addr().String()
	if h.path != "/v2/ns/team/app/manifests/v1" || h.user != "admin" || h.pass != "pw" || h.host != frontHost {
		t.Errorf("upstream saw %+v; want admin credentials and Host %s", h, frontHost)
	}
	if got := access.seen[0]; got.Namespace != "ns" || got.Action != domain.ActionPull || got.Token != "good" {
		t.Errorf("authorized %+v", got)
	}

	resp = do(t, http.MethodPost, front.URL+"/v2/ns/app/blobs/uploads/?mount=sha256:1&from=other/base", "good")
	if resp.code != http.StatusAccepted {
		t.Fatalf("upload start = %d", resp.code)
	}
	if loc := resp.header.Get("Location"); loc != "/v2/ns/app/blobs/uploads/123?_state=x" {
		t.Errorf("Location = %q, want it relative to the front", loc)
	}
	want := []domain.AccessRequest{
		{Token: "good", Namespace: "ns", Action: domain.ActionPush},
		{Token: "good", Namespace: "other", Action: domain.ActionPull},
	}
	if !slices.Equal(access.seen[1:], want) {
		t.Errorf("cross-mount checks = %+v, want %+v", access.seen[1:], want)
	}
}

func TestDenials(t *testing.T) {
	t.Parallel()
	front, _, hits := newFront(t)
	tests := []struct {
		method, path, token string
		want                int
	}{
		{http.MethodGet, "/v2/ns/app/manifests/v1", "", http.StatusUnauthorized},
		{http.MethodPut, "/v2/ro/app/manifests/v1", "good", http.StatusForbidden},
		{http.MethodDelete, "/v2/ro/app/manifests/sha256:1", "good", http.StatusForbidden},
		{http.MethodGet, "/v2/missing/app/tags/list", "good", http.StatusNotFound},
		{http.MethodGet, "/v2/_catalog", "good", http.StatusNotFound},
		{http.MethodGet, "/other", "good", http.StatusNotFound},
	}
	for _, tt := range tests {
		resp := do(t, tt.method, front.URL+tt.path, tt.token)
		if resp.code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, resp.code, tt.want)
		}
	}
	if len(*hits) != 0 {
		t.Errorf("denied requests reached the upstream: %+v", *hits)
	}
	resp := do(t, http.MethodGet, front.URL+"/v2/ro/app/manifests/v1", "good")
	if resp.code != http.StatusOK {
		t.Errorf("pull from read-only registry = %d, want 200", resp.code)
	}
}

func TestStartTLS(t *testing.T) {
	t.Parallel()
	certSrc := httptest.NewUnstartedServer(http.NotFoundHandler()) // only for its test certificate
	certSrc.StartTLS()
	t.Cleanup(certSrc.Close)
	u, _ := url.Parse("http://127.0.0.1:1")
	s, err := New(&fakeAccess{}, Upstream{URL: u}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := s.Start(t.Context(), "127.0.0.1:0", certSrc.TLS)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(t.Context()) })
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr.String()+"/v2/", http.NoBody)
	resp, err := certSrc.Client().Do(req)
	if err != nil {
		t.Fatalf("HTTPS ping: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.TLS == nil {
		t.Errorf("HTTPS ping = %d (tls %v), want 401 over TLS", resp.StatusCode, resp.TLS != nil)
	}
}

func TestStartStop(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("http://127.0.0.1:1")
	s, err := New(&fakeAccess{}, Upstream{URL: u}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := s.Start(t.Context(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	resp := do(t, http.MethodGet, "http://"+addr.String()+"/v2/", "good")
	if resp.code != http.StatusOK {
		t.Errorf("ping = %d", resp.code)
	}
	if err := s.Stop(t.Context()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
