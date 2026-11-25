package api

import (
	"context"
	"crypto/tls"
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
	AuthorizeTCP(ctx context.Context, endpointID string, port int32) (domain.Call, error)
}

// DataPlane is the stub endpoint data plane. Nothing runs behind it:
//   - an HTTPS listener answers calls to RUNNING endpoints with a fixed JSON
//     body, routed by path: /<endpoint-id>[:<port>]/<app path>;
//   - a TLS listener stands in for TCP ports, routed by SNI
//     (<endpoint-id>-p<port>.<domain>): it writes one line and closes.
type DataPlane struct {
	calls CallAuthorizer
	log   *slog.Logger

	mu        sync.Mutex
	srv       *http.Server
	stopTCP   context.CancelFunc
	baseURL   string
	tcpDomain string
	tcpPort   string
	wg        sync.WaitGroup
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

// DataPlaneConfig says where the stub listeners bind and what they present.
type DataPlaneConfig struct {
	// Listen is the HTTPS listener address.
	Listen string
	// PublicURL, when set, replaces the base URL derived from Listen.
	PublicURL string
	// TCPListen is the TLS listener for TCP ports; TCPDomain the DNS suffix
	// of their addresses (routed by SNI).
	TCPListen string
	TCPDomain string
	// Cert is served on both listeners.
	Cert tls.Certificate
}

// Start serves HTTPS calls on cfg.Listen and TLS connections to TCP ports on
// cfg.TCPListen until Stop.
func (d *DataPlane) Start(ctx context.Context, calls CallAuthorizer, cfg DataPlaneConfig) error {
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cfg.Cert}, MinVersion: tls.VersionTLS12}
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("ai data plane: listen %s: %w", cfg.Listen, err)
	}
	tcpLn, err := lc.Listen(ctx, "tcp", cfg.TCPListen)
	if err != nil {
		_ = ln.Close() // the TCP listener error is what matters
		return fmt.Errorf("ai data plane: listen %s: %w", cfg.TCPListen, err)
	}
	_, tcpPort, err := net.SplitHostPort(tcpLn.Addr().String())
	if err != nil {
		_, _ = ln.Close(), tcpLn.Close()
		return fmt.Errorf("ai data plane: %w", err)
	}
	srv := &http.Server{
		Handler: d, ReadHeaderTimeout: 10 * time.Second, TLSConfig: tlsCfg,
		// Handshake failures (clients that do not trust the CA) are routine.
		ErrorLog: slog.NewLogLogger(d.log.Handler(), slog.LevelDebug),
	}
	tcpCtx, stopTCP := context.WithCancel(context.WithoutCancel(ctx))

	d.mu.Lock()
	d.srv, d.calls, d.stopTCP = srv, calls, stopTCP
	d.baseURL = strings.TrimSuffix(cfg.PublicURL, "/")
	if d.baseURL == "" {
		d.baseURL = "https://" + ln.Addr().String()
	}
	d.tcpDomain, d.tcpPort = cfg.TCPDomain, tcpPort
	d.mu.Unlock()

	d.wg.Go(func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.ErrorContext(ctx, "ai data plane stopped", "error", err)
		}
	})
	d.wg.Go(func() { d.serveTCP(tcpCtx, tls.NewListener(tcpLn, tlsCfg)) })
	d.log.InfoContext(ctx, "ai endpoint data plane listening", "url", d.BaseURL(), "tcp", tcpLn.Addr().String())
	return nil
}

// Stop shuts both listeners down and waits for them to exit.
func (d *DataPlane) Stop(ctx context.Context) error {
	d.mu.Lock()
	srv, stopTCP := d.srv, d.stopTCP
	d.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	stopTCP()
	d.wg.Wait()
	return err
}

// BaseURL is the scheme and authority HTTP endpoint URLs start with; empty
// until Start.
func (d *DataPlane) BaseURL() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.baseURL
}

// TCPURL is the address of one TCP port of an endpoint:
// tls://<endpoint-id>-p<port>.<domain>:<tcp listener port>.
func (d *DataPlane) TCPURL(endpointID string, containerPort int32) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fmt.Sprintf("tls://%s-p%d.%s:%s", endpointID, containerPort, d.tcpDomain, d.tcpPort)
}
