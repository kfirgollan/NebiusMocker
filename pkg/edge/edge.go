// Package edge owns the gRPC listener that serves every enabled service on one
// port (ADR-0001), plus gRPC health and reflection.
package edge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// Server is the edge gRPC server.
type Server struct {
	grpc   *grpc.Server
	health *health.Server
	log    *slog.Logger

	lis     net.Listener
	wg      sync.WaitGroup
	serveMu sync.Mutex
	err     error
}

// New builds a server with the given interceptor chain (outermost first).
func New(log *slog.Logger, interceptors ...grpc.UnaryServerInterceptor) *Server {
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(interceptors...))
	h := health.NewServer()
	healthpb.RegisterHealthServer(g, h)
	reflection.Register(g)
	return &Server{grpc: g, health: h, log: log}
}

// Registrar is where modules register their services.
func (s *Server) Registrar() grpc.ServiceRegistrar { return s.grpc }

// Start listens on addr and serves in an owned goroutine; ctx bounds only the
// listen call, the server runs until Stop. It returns the bound
// address, which differs from addr when addr uses port 0.
func (s *Server) Start(ctx context.Context, addr string) (net.Addr, error) {
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("edge: listen %s: %w", addr, err)
	}
	s.lis = lis
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	serveCtx := context.WithoutCancel(ctx) // for logging only; serving is stopped by Stop
	s.wg.Go(func() {
		if err := s.grpc.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.serveMu.Lock()
			s.err = err
			s.serveMu.Unlock()
			s.log.ErrorContext(serveCtx, "edge: serve stopped", "error", err)
		}
	})
	return lis.Addr(), nil
}

// Stop drains in-flight RPCs until ctx is done, then forces the server closed.
func (s *Server) Stop(ctx context.Context) error {
	s.health.Shutdown()
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
	}
	s.wg.Wait()
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	return s.err
}
