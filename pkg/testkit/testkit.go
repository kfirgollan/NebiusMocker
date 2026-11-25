// Package testkit starts an in-process NebiusMocker for Go tests and connects
// the official Nebius Go SDK to it.
package testkit

import (
	"context"
	"log/slog"
	"maps"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/conn"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

// Instance is a running mocker.
type Instance struct {
	// Addr is the edge address, e.g. "127.0.0.1:53211".
	Addr string
	// Token authenticates as the default dev principal.
	Token string
	// TenantID is the seeded default tenant.
	TenantID string
	// ProjectID is the seeded default project (child of TenantID).
	ProjectID string
}

type settings struct {
	cfg  config.Config
	logs bool
}

// Option adjusts the instance before start.
type Option func(*settings)

// WithOperationDelay keeps operations running for d before completing.
func WithOperationDelay(d time.Duration) Option {
	return func(s *settings) { s.cfg.OperationDelay = d }
}

// WithAuthnRequired rejects anonymous requests.
func WithAuthnRequired() Option {
	return func(s *settings) { s.cfg.Authn = config.AuthnRequired }
}

// WithAuthz sets the authorization mode (default: audit).
func WithAuthz(mode authz.Mode) Option {
	return func(s *settings) { s.cfg.Authz = mode }
}

// WithTokens adds static bearer tokens (token → principal ID).
func WithTokens(tokens map[string]string) Option {
	return func(s *settings) {
		merged := maps.Clone(s.cfg.Tokens)
		maps.Copy(merged, tokens)
		s.cfg.Tokens = merged
	}
}

// WithLogs writes the mocker's request log to the test output.
func WithLogs() Option {
	return func(s *settings) { s.logs = true }
}

// StartT starts the modules on a random localhost port with sequential IDs and
// stops them when the test ends. Logs are discarded unless WithLogs is given.
func StartT(tb testing.TB, modules []mocker.Module, opts ...Option) *Instance {
	tb.Helper()
	s := settings{cfg: config.Default()}
	s.cfg.Listen = "127.0.0.1:0"
	s.cfg.IDMode = nid.Sequential
	for _, o := range opts {
		o(&s)
	}
	cfg := s.cfg
	log := slog.New(slog.DiscardHandler)
	if s.logs {
		log = slog.New(slog.NewTextHandler(tb.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	srv, err := mocker.New(cfg, log, modules)
	if err != nil {
		tb.Fatalf("testkit: assemble mocker: %v", err)
	}
	// The server outlives any single test context: it is stopped in Cleanup.
	addr, err := srv.Start(context.WithoutCancel(tb.Context()))
	if err != nil {
		tb.Fatalf("testkit: start mocker: %v", err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(tb.Context()), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			tb.Errorf("testkit: stop mocker: %v", err)
		}
	})
	return &Instance{Addr: addr.String(), Token: config.DefaultDevToken, TenantID: cfg.TenantID, ProjectID: cfg.ProjectID}
}

// SDK returns an official gosdk client routed to the instance, the same way the
// Terraform provider is configured: resolver "*" → Addr, plaintext, static token.
func (i *Instance) SDK(tb testing.TB) *gosdk.SDK {
	tb.Helper()
	return i.SDKAs(tb, i.Token)
}

// SDKAs is SDK authenticated with the given bearer token.
func (i *Instance) SDKAs(tb testing.TB, token string) *gosdk.SDK {
	tb.Helper()
	sdk, err := gosdk.New(tb.Context(),
		gosdk.WithResolvers(conn.NewBasicResolver("*", i.Addr)),
		gosdk.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		gosdk.WithCredentials(gosdk.IAMToken(token)),
	)
	if err != nil {
		tb.Fatalf("testkit: gosdk.New: %v", err)
	}
	tb.Cleanup(func() { _ = sdk.Close() }) // best-effort: connections are local
	return sdk
}
