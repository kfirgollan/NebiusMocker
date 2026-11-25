package harbor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// fakeHarbor serves the handful of Harbor API routes the client uses.
type fakeHarbor struct {
	mu       sync.Mutex
	projects map[string]bool
	calls    []string
}

func (f *fakeHarbor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.EscapedPath())
	if u, p, _ := r.BasicAuth(); u != "admin" || p != "pw" || r.Header.Get("X-Is-Resource-Name") != "true" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch path := r.URL.EscapedPath(); {
	case r.Method == http.MethodPost && path == "/api/v2.0/projects":
		var body struct {
			Name string `json:"project_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.projects[body.Name] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.projects[body.Name] = true
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodHead && path == "/api/v2.0/projects":
		if !f.projects[r.URL.Query().Get("project_name")] {
			w.WriteHeader(http.StatusNotFound)
		}
	case path == "/api/v2.0/projects/ns/repositories":
		_, _ = w.Write([]byte(`[{"name":"ns/team/web"}]`))
	case path == "/api/v2.0/projects/ns/repositories/team%252Fweb/artifacts":
		if r.URL.Query().Get("with_tag") != "true" || r.URL.Query().Get("page_size") != "100" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[{"digest":"sha256:1","manifest_media_type":"application/vnd.oci.image.manifest.v1+json",
			"size":42,"push_time":"2026-01-02T03:04:05.000Z","tags":[{"name":"v1"},{"name":"latest"}]},
			{"digest":"sha256:2","manifest_media_type":"application/vnd.oci.image.index.v1+json","references":[{}]}]`))
	case r.Method == http.MethodDelete && path == "/api/v2.0/projects/ns/repositories/team%252Fweb/artifacts/sha256:1":
	case r.Method == http.MethodDelete && path == "/api/v2.0/projects/ns/repositories/team%252Fweb":
	case r.Method == http.MethodDelete && path == "/api/v2.0/projects/ns":
		delete(f.projects, "ns")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newClient(t *testing.T) (*Client, *fakeHarbor) {
	t.Helper()
	f := &fakeHarbor{projects: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL + "/", Username: "admin", Password: "pw"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, f
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{{URL: "ftp://x", Username: "a"}, {URL: "http://", Username: "a"}, {URL: "http://h"}} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}

func TestClient(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c, f := newClient(t)
	if _, err := c.ListImages(ctx, "ns"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("ListImages of missing project: %v, want NotFound", err)
	}
	for range 2 { // idempotent
		if err := c.EnsureNamespace(ctx, "ns"); err != nil {
			t.Fatalf("EnsureNamespace: %v", err)
		}
	}
	imgs, err := c.ListImages(ctx, "ns")
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(imgs) != 2 {
		t.Fatalf("ListImages = %+v, want 2", imgs)
	}
	a, b := imgs[0], imgs[1]
	if a.Repository != "team/web" || a.Digest != "sha256:1" || a.Size != 42 || len(a.Tags) != 2 || a.List || a.PushedAt.Year() != 2026 {
		t.Errorf("first image = %+v", a)
	}
	if !b.List {
		t.Errorf("image index not reported as a list: %+v", b)
	}
	if err := c.DeleteImage(ctx, "ns", "team/web", "sha256:1"); err != nil {
		t.Errorf("DeleteImage: %v", err)
	}
	if err := c.DeleteImage(ctx, "ns", "team/web", "sha256:9"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("DeleteImage missing: %v, want NotFound", err)
	}
	if err := c.DeleteNamespace(ctx, "ns"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	if err := c.DeleteNamespace(ctx, "ns"); err != nil {
		t.Errorf("DeleteNamespace twice: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.projects["ns"] {
		t.Error("project survived DeleteNamespace")
	}
}

func TestBadCredentials(t *testing.T) {
	t.Parallel()
	_, f := newClient(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, _ := New(Config{URL: srv.URL, Username: "admin", Password: "wrong"})
	if err := c.EnsureNamespace(t.Context(), "ns"); !errs.IsKind(err, errs.KindInternal) {
		t.Errorf("EnsureNamespace with bad credentials: %v, want Internal", err)
	}
}
