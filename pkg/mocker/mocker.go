// Package mocker assembles a NebiusMocker process: shared dependencies, the
// edge server, the operation engine and the enabled service modules.
package mocker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/auth"
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
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
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
	// Peers is the in-process capability registry: modules Provide
	// capabilities other areas consume (e.g. MysteryBox secret payloads)
	// and Lookup the ones they need at call time.
	Peers *peer.Directory
	// AuditLog holds the audit events the audit interceptor records for
	// every module; the audit module serves them.
	AuditLog *audit.Log
	// TLS is the server TLS configuration from Config.TLS (nil: plaintext).
	// Modules that open their own listeners serve TLS with it when set.
	TLS *tls.Config
	// AuditRecorder stamps and appends audit events. The audit module
	// records data-plane events (Object Storage object access) through it.
	AuditRecorder *audit.Recorder
	// Tokens mints, verifies and revokes the access tokens the mocker issues
	// (the IAM module's token exchange; key and service-account lifecycles).
	Tokens *auth.Issuer
	// Authenticator resolves bearer tokens (static dev tokens and issued
	// tokens) to principals, for fronts other than the gRPC edge.
	Authenticator *auth.Authenticator
	// HTTP serves plain HTTP endpoints on the edge port (e.g. the IAM
	// module's POST /oauth2/token/exchange).
	HTTP HTTPRegistrar
}

// HTTPRegistrar registers plain HTTP handlers on the edge port.
type HTTPRegistrar interface {
	HandleHTTP(pattern string, h http.Handler)
}

// loadTLS reads the configured certificate; it returns nil for plaintext.
func loadTLS(c config.TLSConfig) (*tls.Config, error) {
	if !c.Enabled() {
		return nil, nil //nolint:nilnil // nil means plaintext, by contract
	}
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, errors.New("tls: NEBIUS_MOCKER_TLS_CERT_FILE and NEBIUS_MOCKER_TLS_KEY_FILE must be set together")
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: load %s: %w", c.CertFile, err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
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
	// states learns each service's Get as modules register, for the
	// audit events' resource.state.
	states *interceptors.ResourceStates
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
	tlsCfg, err := loadTLS(cfg.TLS)
	if err != nil {
		return nil, err
	}
	engine := authz.NewEngine(authz.DefaultCatalog())
	rules := authz.NewRules()
	states := interceptors.NewResourceStates()
	auditLog, recorder, err := newAudit(cfg, o.clk, log, ops, states)
	if err != nil {
		return nil, err
	}
	issuer, err := newIssuer(cfg.Auth, o.clk)
	if err != nil {
		return nil, err
	}
	authn := auth.NewAuthenticator(cfg.Tokens, issuer)
	srv := edge.NewTLS(log, tlsCfg,
		interceptors.RequestID(),
		interceptors.Recover(log),
		interceptors.Log(log, o.clk),
		interceptors.Authenticate(cfg.Authn, authn),
		interceptors.Audit(interceptors.AuditConfig{
			Recorder: recorder, Hierarchy: engine, Rules: rules,
			Reads: cfg.Audit.Reads, DefaultTenant: cfg.TenantID, States: states,
		}),
		validate,
		interceptors.Authorize(engine, rules, cfg.Authz, log),
	)
	commonpb.RegisterOperationServiceServer(srv.Registrar(), opsapi.NewServer(ops))
	opsapi.RegisterAuthzRules(rules, ops)
	return &Server{
		cfg: cfg, log: log, edge: srv, ops: ops, modules: enabled, states: states,
		deps: Deps{
			Config: cfg, Store: store.NewMemory(), Ops: ops, Clock: o.clk, IDs: ids, Log: log,
			Authz: engine, Rules: rules, ObjStore: objs, Peers: peer.NewDirectory(),
			AuditLog: auditLog, AuditRecorder: recorder, TLS: tlsCfg,
			Tokens: issuer, Authenticator: authn, HTTP: srv,
		},
	}, nil
}

// Start registers modules, starts the operation engine and begins serving.
// It returns the bound edge address.
func (s *Server) Start(ctx context.Context) (net.Addr, error) {
	for _, m := range s.modules {
		if err := m.Register(ctx, s.states.Registrar(s.edge.Registrar()), s.deps); err != nil {
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

// newAudit builds the audit event log and its recorder, and reports
// operation completions to it (the DONE/ERROR event of async calls).
func newAudit(cfg config.Config, clk clock.Clock, log *slog.Logger, ops *operations.Engine, states audit.StateReader) (*audit.Log, *audit.Recorder, error) {
	auditLog, err := audit.NewLog(cfg.Audit.MaxEvents)
	if err != nil {
		return nil, nil, err
	}
	rec, err := audit.NewRecorder(auditLog, clk, log, audit.WithStateReader(states))
	if err != nil {
		return nil, nil, err
	}
	// The IAM module seeds both under this name; events about them carry it
	// in resource.hierarchy.name.
	rec.RememberName(cfg.TenantID, seededName)
	rec.RememberName(cfg.ProjectID, seededName)
	ops.OnFinish(func(ctx context.Context, op *operations.Operation) {
		st := serviceerror.StatusProto(ctx, op.Err)
		rec.OperationFinished(ctx, op.ID, code.Code(st.GetCode()), st.GetMessage())
	})
	return auditLog, rec, nil
}

// newIssuer builds the access-token issuer; an empty secret means a random
// one per process.
func newIssuer(cfg config.AuthConfig, clk clock.Clock) (*auth.Issuer, error) {
	secret := []byte(cfg.TokenSecret)
	if len(secret) == 0 {
		var err error
		if secret, err = auth.NewSecret(); err != nil {
			return nil, err
		}
	}
	return auth.NewIssuer(secret, cfg.TokenTTL, clk)
}

// seededName is the name of the seeded default tenant and project.
const seededName = "default"

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
