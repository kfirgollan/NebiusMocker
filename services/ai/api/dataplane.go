package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

// CallAuthorizer makes the data-plane decisions (satisfied by
// *domain.EndpointService).
type CallAuthorizer interface {
	AuthorizeCall(ctx context.Context, target, authorization string) (domain.Call, error)
}

// DataPlane is the stub endpoint data plane: an HTTP listener that answers
// calls to RUNNING endpoints with a fixed JSON body instead of running a
// workload. Requests are routed by path: /<endpoint-id>[:<port>]/<app path>.
type DataPlane struct {
	calls CallAuthorizer
	log   *slog.Logger

	mu      sync.Mutex
	srv     *http.Server
	baseURL string
	done    chan struct{}
}

// NewDataPlane returns an unstarted data plane. It answers 503 until Start
// gives it the CallAuthorizer (which itself needs the data plane's URL).
func NewDataPlane(log *slog.Logger) *DataPlane {
	return &DataPlane{log: log}
}

// callResponse is the body of every successful stub call.
type callResponse struct {
	Message    string `json:"message"`
	EndpointID string `json:"endpoint_id"`
	Port       int32  `json:"port"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Query      string `json:"query,omitempty"`
}

type errorResponse struct {
	Error      string `json:"error"`
	EndpointID string `json:"endpoint_id,omitempty"`
}

// ServeHTTP implements http.Handler.
func (d *DataPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if target == "" {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no endpoint in path; use /<endpoint-id>/..."})
		return
	}
	d.mu.Lock()
	calls := d.calls
	d.mu.Unlock()
	if calls == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "data plane not started"})
		return
	}
	call, err := calls.AuthorizeCall(r.Context(), target, r.Header.Get("Authorization"))
	if err != nil {
		status := statusOf(err)
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nebius-ai-endpoint"`)
		}
		writeJSON(w, status, errorResponse{Error: errorMessage(err), EndpointID: target})
		return
	}
	writeJSON(w, http.StatusOK, callResponse{
		Message:    "endpoint was called",
		EndpointID: call.EndpointID,
		Port:       call.Port,
		Method:     r.Method,
		Path:       "/" + rest,
		Query:      r.URL.RawQuery,
	})
}

// httpStatus maps domain error kinds to data-plane HTTP statuses.
var httpStatus = map[errs.Kind]int{
	errs.KindNotFound:        http.StatusNotFound,
	errs.KindUnauthenticated: http.StatusUnauthorized,
	errs.KindBadState:        http.StatusServiceUnavailable,
}

func statusOf(err error) int {
	if st, ok := httpStatus[errs.KindOf(err)]; ok {
		return st
	}
	return http.StatusInternalServerError
}

func errorMessage(err error) string {
	if e, ok := errors.AsType[*errs.Error](err); ok && e.Message != "" {
		return e.Message
	}
	if errs.IsKind(err, errs.KindNotFound) {
		return "endpoint not found"
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the client is gone if this fails
}

// Start listens on addr and serves calls until Stop. publicURL, when set,
// replaces the base URL derived from the bound address.
func (d *DataPlane) Start(ctx context.Context, calls CallAuthorizer, addr, publicURL string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("ai data plane: listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: d, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	d.mu.Lock()
	d.srv, d.done, d.calls = srv, done, calls
	d.baseURL = strings.TrimSuffix(publicURL, "/")
	if d.baseURL == "" {
		d.baseURL = "http://" + ln.Addr().String()
	}
	d.mu.Unlock()
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.ErrorContext(ctx, "ai data plane stopped", "error", err)
		}
	}()
	d.log.InfoContext(ctx, "ai endpoint data plane listening", "url", d.BaseURL())
	return nil
}

// Stop shuts the listener down and waits for the serve loop to exit.
func (d *DataPlane) Stop(ctx context.Context) error {
	d.mu.Lock()
	srv, done := d.srv, d.done
	d.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	<-done
	return err
}

// BaseURL is the scheme and authority endpoint URLs start with; empty until
// Start.
func (d *DataPlane) BaseURL() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.baseURL
}
