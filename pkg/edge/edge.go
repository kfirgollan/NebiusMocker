// Package edge owns the listener that serves every enabled service on one
// port (ADR-0001), plus gRPC health and reflection. The same port also serves
// plain HTTP endpoints that Nebius exposes over HTTP (the OAuth token
// exchange): gRPC requests (HTTP/2, content-type application/grpc) go to the
// gRPC server, everything else to the HTTP mux. Plaintext connections may be
// HTTP/1.1 or prior-knowledge HTTP/2 (h2c, what gRPC clients use); TLS
// connections negotiate h2 or http/1.1 through ALPN.
package edge

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// readHeaderTimeout bounds how long a client may take to send request headers.
const readHeaderTimeout = 30 * time.Second

// Server is the edge server.
type Server struct {
	grpc   *grpc.Server
	health *health.Server
	mux    *http.ServeMux
	http   *http.Server
	tls    *tls.Config
	log    *slog.Logger

	wg      sync.WaitGroup
	serveMu sync.Mutex
	err     error
}

// New builds a plaintext server with the given interceptor chain
// (outermost first).
func New(log *slog.Logger, interceptors ...grpc.UnaryServerInterceptor) *Server {
	return NewTLS(log, nil, interceptors...)
}

// NewTLS is New serving TLS with tlsCfg; a nil tlsCfg means plaintext.
func NewTLS(log *slog.Logger, tlsCfg *tls.Config, interceptors ...grpc.UnaryServerInterceptor) *Server {
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(interceptors...))
	h := health.NewServer()
	healthpb.RegisterHealthServer(g, h)
	reflection.Register(g)
	s := &Server{grpc: g, health: h, mux: http.NewServeMux(), log: log}
	if tlsCfg != nil {
		s.tls = tlsCfg.Clone()
		s.tls.NextProtos = []string{"h2", "http/1.1"}
	}
	return s
}

// Registrar is where modules register their gRPC services.
func (s *Server) Registrar() grpc.ServiceRegistrar { return s.grpc }

// HandleHTTP serves a plain HTTP endpoint on the edge port (pattern as in
// http.ServeMux, e.g. "POST /oauth2/token/exchange"). Call it before Start.
func (s *Server) HandleHTTP(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// ServeHTTP routes gRPC requests to the gRPC server and the rest to the mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		s.grpc.ServeHTTP(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// Start listens on addr and serves in an owned goroutine; ctx bounds only the
// listen call, the server runs until Stop. It returns the bound
// address, which differs from addr when addr uses port 0.
func (s *Server) Start(ctx context.Context, addr string) (net.Addr, error) {
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("edge: listen %s: %w", addr, err)
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(s.tls == nil)
	serveCtx := context.WithoutCancel(ctx) // for logging only; serving is stopped by Stop
	s.http = &http.Server{
		Handler: s, Protocols: protocols, TLSConfig: s.tls, ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	if s.tls != nil {
		lis = tls.NewListener(lis, s.tls)
	}
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	s.wg.Go(func() {
		if err := s.http.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveMu.Lock()
			s.err = err
			s.serveMu.Unlock()
			s.log.ErrorContext(serveCtx, "edge: serve stopped", "error", err)
		}
	})
	return lis.Addr(), nil
}

// Stop drains in-flight requests until ctx is done, then forces the server
// closed.
func (s *Server) Stop(ctx context.Context) error {
	s.health.Shutdown()
	if s.http == nil {
		s.grpc.Stop()
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		_ = s.http.Close() // deadline passed: force-close what is left
	}
	s.grpc.Stop()
	s.wg.Wait()
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	return s.err
}
