// Package docker is the Container Registry's docker-facing endpoint: a thin
// HTTP front for the OCI distribution API (/v2/...) that authenticates docker
// clients with Nebius IAM tokens, asks the domain whether the caller may pull
// or push in the target registry, and reverse-proxies allowed requests to
// Harbor with Harbor admin credentials. It stores and serves no image data
// itself.
//
// Login works the way Nebius documents it:
//
//	nebius iam get-access-token | docker login <fqdn> --username iam --password-stdin
//
// The password is the IAM token; the username is ignored. A bearer token in
// the Authorization header is accepted too.
package docker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

// challenge is the WWW-Authenticate header docker answers with credentials.
const challenge = `Basic realm="NebiusMocker Container Registry"`

// Access is the domain decision point (satisfied by *domain.AccessService).
type Access interface {
	Authenticate(token string) (string, error)
	Authorize(ctx context.Context, req domain.AccessRequest) error
}

// Upstream is where allowed requests go: Harbor's base URL and admin credentials.
type Upstream struct {
	URL      *url.URL
	Username string
	Password string
}

// Server serves the docker endpoint. Start binds; Stop drains.
type Server struct {
	access Access
	log    *slog.Logger
	proxy  *httputil.ReverseProxy

	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener
	wg  sync.WaitGroup
}

// New returns a Server that forwards to up.
func New(access Access, up Upstream, log *slog.Logger) (*Server, error) {
	if access == nil || log == nil || up.URL == nil {
		return nil, errors.New("registry docker: access, upstream URL and logger are required")
	}
	s := &Server{access: access, log: log}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up.URL)
			pr.Out.Host = pr.In.Host // registry URLs Harbor generates keep the docker-facing host
			pr.Out.Header.Del("Authorization")
			pr.Out.SetBasicAuth(up.Username, up.Password)
		},
		FlushInterval:  -1, // stream layer uploads and downloads
		ModifyResponse: relativeLocation,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.ErrorContext(r.Context(), "registry upstream", "method", r.Method, "path", r.URL.Path, "error", err)
			writeError(w, http.StatusBadGateway, "UNAVAILABLE", "registry backend unavailable")
		},
	}
	return s, nil
}

// Handler returns the HTTP handler (useful for tests with httptest).
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// Start listens on addr and serves until Stop; with a non-nil tlsCfg it
// serves HTTPS (needed for registry hosts docker does not treat as insecure,
// e.g. cr.<region>.nebius.cloud).
func (s *Server) Start(ctx context.Context, addr string, tlsCfg *tls.Config) (net.Addr, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("registry docker: listen %s: %w", addr, err)
	}
	if tlsCfg != nil {
		cfg := tlsCfg.Clone()
		cfg.NextProtos = []string{"http/1.1"}
		ln = tls.NewListener(ln, cfg)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	s.mu.Lock()
	s.srv, s.ln = srv, ln
	s.mu.Unlock()
	s.wg.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.ErrorContext(ctx, "registry docker endpoint stopped", "error", err)
		}
	})
	return ln.Addr(), nil
}

// Stop shuts the endpoint down, waiting for in-flight requests until ctx ends.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	s.wg.Wait()
	return err
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if r.URL.Path == "/v2" || r.URL.Path == "/v2/" {
		s.ping(w, r)
		return
	}
	reqs, ok := accessRequests(r)
	if !ok {
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "only /v2/<registry-id>/<image>/... is served; list images with the ArtifactService")
		return
	}
	for _, ar := range reqs {
		if err := s.access.Authorize(r.Context(), ar); err != nil {
			s.deny(w, r, err)
			return
		}
	}
	s.proxy.ServeHTTP(w, r)
}

// ping answers the docker login probe: 401 with a challenge, or 200.
func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	if _, err := s.access.Authenticate(credential(r)); err != nil {
		s.deny(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}")) // best-effort: the status line is already sent
}

func (s *Server) deny(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, "UNKNOWN"
	switch errs.KindOf(err) {
	case errs.KindUnauthenticated:
		w.Header().Set("WWW-Authenticate", challenge)
		status, code = http.StatusUnauthorized, "UNAUTHORIZED"
	case errs.KindPermissionDenied:
		status, code = http.StatusForbidden, "DENIED"
	case errs.KindNotFound:
		status, code = http.StatusNotFound, "NAME_UNKNOWN"
	case errs.KindBadState, errs.KindBadRequest, errs.KindOutOfRange:
		status, code = http.StatusBadRequest, "NAME_INVALID"
	case errs.KindUnknown, errs.KindAlreadyExists, errs.KindConflict, errs.KindOperationConflict,
		errs.KindQuotaExceeded, errs.KindInternal:
		s.log.ErrorContext(r.Context(), "registry access check", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, status, code, "internal error")
		return
	default:
		s.log.ErrorContext(r.Context(), "registry access check", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, status, code, "internal error")
		return
	}
	writeError(w, status, code, err.Error())
}

// credential extracts the IAM token: the Basic password or a Bearer token.
func credential(r *http.Request) string {
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	return ""
}

// routeMarkers end the repository name in distribution API paths.
var routeMarkers = []string{"/manifests/", "/blobs/", "/tags/", "/referrers/"}

// accessRequests turns a /v2/<name>/<route> request into the access checks it
// needs: the action on the target namespace, plus pull on the source of a
// cross-repository blob mount.
func accessRequests(r *http.Request) ([]domain.AccessRequest, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		return nil, false
	}
	end := -1
	for _, m := range routeMarkers {
		if i := strings.Index(rest, m); i > 0 && (end < 0 || i < end) {
			end = i
		}
	}
	if end < 0 {
		return nil, false
	}
	token := credential(r)
	reqs := []domain.AccessRequest{{Token: token, Namespace: namespaceOf(rest[:end]), Action: actionOf(r.Method)}}
	if from := r.URL.Query().Get("from"); from != "" && r.Method == http.MethodPost {
		reqs = append(reqs, domain.AccessRequest{Token: token, Namespace: namespaceOf(from), Action: domain.ActionPull})
	}
	return reqs, true
}

func namespaceOf(name string) string {
	ns, _, _ := strings.Cut(name, "/")
	return ns
}

func actionOf(method string) domain.Action {
	switch method {
	case http.MethodGet, http.MethodHead:
		return domain.ActionPull
	case http.MethodDelete:
		return domain.ActionDelete
	default:
		return domain.ActionPush
	}
}

// relativeLocation strips scheme and host from redirect and upload URLs so
// docker keeps talking to this front instead of Harbor directly.
func relativeLocation(resp *http.Response) error {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil
	}
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() {
		return nil //nolint:nilerr // leave unparsable or relative locations untouched
	}
	if !strings.HasPrefix(u.Path, "/v2/") {
		return nil // e.g. a blob redirect to external storage
	}
	resp.Header.Set("Location", u.RequestURI())
	return nil
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}}
	_ = json.NewEncoder(w).Encode(body) // best-effort: the status line is already sent
}
