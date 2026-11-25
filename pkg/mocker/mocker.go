// Package mocker assembles a NebiusMocker process: shared dependencies, the
// edge server, the operation engine and the enabled service modules.
package mocker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"

	"google.golang.org/grpc"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/edge"
	"github.com/kfirgollan/NebiusMocker/pkg/interceptors"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	objmemory "github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	objrustfs "github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	opsdal "github.com/kfirgollan/NebiusMocker/pkg/operations/dal"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

// Module is one Nebius API area (e.g. "iam").
type Module interface {
	// Name is the area name used in NEBIUS_MOCKER_SERVICES.
	Name() string
	// Register seeds the module's data and registers its gRPC services.
	Register(ctx context.Context, reg grpc.ServiceRegistrar, deps Deps) error
}

// Lifecycle is implemented by modules that run background work or extra
// listeners (e.g. a data-plane HTTP server). Start is called after every module
// is registered and before the edge serves; Stop runs in reverse order on shutdown.
type Lifecycle interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Deps are the shared dependencies handed to every module.
type Deps struct {
	Config config.Config
	Store  store.Store
	Ops    *operations.Engine
	Clock  clock.Clock
	IDs    nid.Generator
	Log    *slog.Logger
	// Authz is where modules register their resource kinds (for ancestry) and,
	// for IAM, the bindings source.
	Authz *authz.Engine
	// Rules is where modules declare the permission each RPC needs.
	Rules *authz.Rules
	// ObjStore is the object-storage backend shared by modules that keep
	// blobs (memory or RustFS, from Config.ObjStore).
	ObjStore objstore.Store
}

// Option customizes a Server.
type Option func(*options)

type options struct {
	clk      clock.Clock
	objStore objstore.Store
}

// WithClock replaces the real clock (tests use clock.Manual).
func WithClock(c clock.Clock) Option { return func(o *options) { o.clk = c } }

// WithObjStore replaces the object store built from Config.ObjStore.
func WithObjStore(s objstore.Store) Option { return func(o *options) { o.objStore = s } }

// Server is an assembled, not-yet-started mocker.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	edge    *edge.Server
	ops     *operations.Engine
	modules []Module
	deps    Deps
}

// New assembles a server with the enabled subset of modules.
func New(cfg config.Config, log *slog.Logger, modules []Module, opts ...Option) (*Server, error) {
	o := options{clk: clock.Real{}}
	for _, opt := range opts {
		opt(&o)
	}
	enabled, err := selectModules(cfg.Services, modules)
	if err != nil {
		return nil, err
	}
	ids, err := nid.NewGenerator(cfg.IDMode, cfg.RoutingCode, 0)
	if err != nil {
		return nil, err
	}
	ops, err := operations.NewEngine(opsdal.NewMemory(), o.clk, ids, cfg.OperationDelay, log)
	if err != nil {
		return nil, err
	}
	validate, err := interceptors.Validate()
	if err != nil {
		return nil, err
	}
	objs := o.objStore
	if objs == nil {
		if objs, err = newObjStore(cfg.ObjStore, o.clk); err != nil {
			return nil, err
		}
	}
	engine := authz.NewEngine(authz.DefaultCatalog())
	rules := authz.NewRules()
	srv := edge.New(log,
		interceptors.RequestID(),
		interceptors.Recover(log),
		interceptors.Log(log, o.clk),
		interceptors.Authenticate(cfg.Authn, cfg.Tokens),
		validate,
		interceptors.Authorize(engine, rules, cfg.Authz, log),
	)
	commonpb.RegisterOperationServiceServer(srv.Registrar(), opsapi.NewServer(ops))
	opsapi.RegisterAuthzRules(rules, ops)
	return &Server{
		cfg: cfg, log: log, edge: srv, ops: ops, modules: enabled,
		deps: Deps{
			Config: cfg, Store: store.NewMemory(), Ops: ops, Clock: o.clk, IDs: ids, Log: log,
			Authz: engine, Rules: rules, ObjStore: objs,
		},
	}, nil
}

// Start registers modules, starts the operation engine and begins serving.
// It returns the bound edge address.
func (s *Server) Start(ctx context.Context) (net.Addr, error) {
	for _, m := range s.modules {
		if err := m.Register(ctx, s.edge.Registrar(), s.deps); err != nil {
			return nil, fmt.Errorf("register module %s: %w", m.Name(), err)
		}
	}
	s.ops.Start(ctx)
	if err := s.startModules(ctx); err != nil {
		s.ops.Stop()
		return nil, err
	}
	addr, err := s.edge.Start(ctx, s.cfg.Listen)
	if err != nil {
		s.stopModules(ctx)
		s.ops.Stop()
		return nil, err
	}
	s.log.InfoContext(ctx, "nebius-mocker listening", "addr", addr.String(), "services", moduleNames(s.modules))
	return addr, nil
}

// Stop drains the edge server, stops modules and the operation engine.
func (s *Server) Stop(ctx context.Context) error {
	err := s.edge.Stop(ctx)
	s.stopModules(ctx)
	s.ops.Stop()
	return err
}

// startModules starts every Lifecycle module; on failure it stops the ones
// already started.
func (s *Server) startModules(ctx context.Context) error {
	for i, m := range s.modules {
		lc, ok := m.(Lifecycle)
		if !ok {
			continue
		}
		if err := lc.Start(ctx); err != nil {
			for _, prev := range slices.Backward(s.modules[:i]) {
				if plc, ok := prev.(Lifecycle); ok {
					_ = plc.Stop(ctx) // best-effort rollback; the start error is what matters
				}
			}
			return fmt.Errorf("start module %s: %w", m.Name(), err)
		}
	}
	return nil
}

func (s *Server) stopModules(ctx context.Context) {
	for _, m := range slices.Backward(s.modules) {
		if lc, ok := m.(Lifecycle); ok {
			if err := lc.Stop(ctx); err != nil {
				s.log.ErrorContext(ctx, "stop module", "module", m.Name(), "error", err)
			}
		}
	}
}

// newObjStore builds the configured object-storage backend.
func newObjStore(cfg config.ObjStoreConfig, clk clock.Clock) (objstore.Store, error) {
	switch cfg.Backend {
	case config.ObjStoreMemory:
		return objmemory.New(clk), nil
	case config.ObjStoreRustFS:
		c, err := objrustfs.New(objrustfs.Config{
			Endpoint: cfg.Endpoint, AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey, Region: cfg.Region,
		})
		if err != nil {
			return nil, fmt.Errorf("objstore: %w", err)
		}
		return c, nil
	default:
		return nil, fmt.Errorf("objstore: unknown backend %d", cfg.Backend)
	}
}

func selectModules(names []string, all []Module) ([]Module, error) {
	if len(names) == 0 {
		return all, nil
	}
	var out []Module
	var unknown []string
	for _, n := range names {
		i := slices.IndexFunc(all, func(m Module) bool { return m.Name() == n })
		if i < 0 {
			unknown = append(unknown, n)
			continue
		}
		out = append(out, all[i])
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown services %v (available: %v)", unknown, moduleNames(all))
	}
	return out, nil
}

func moduleNames(ms []Module) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name())
	}
	return out
}
