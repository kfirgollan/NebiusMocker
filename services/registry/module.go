// Package registry is the NebiusMocker module for Nebius Container Registry
// (nebius.registry.v1).
//
// Implemented: RegistryService (Get, GetByName, List, Create, Update, Delete)
// and ArtifactService (Get, List, Delete). Images live in a backend: an
// in-memory one for tests, or Harbor (NEBIUS_MOCKER_REGISTRY_BACKEND=harbor),
// where each registry is a Harbor project and the module serves a docker
// endpoint (default :4590) that accepts Nebius IAM tokens and forwards to
// Harbor. See docs/services/registry.md.
package registry

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"

	"google.golang.org/grpc"

	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/registry/api"
	"github.com/kfirgollan/NebiusMocker/services/registry/backend/harbor"
	"github.com/kfirgollan/NebiusMocker/services/registry/backend/memory"
	"github.com/kfirgollan/NebiusMocker/services/registry/dal"
	"github.com/kfirgollan/NebiusMocker/services/registry/docker"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

// listenOff disables the docker endpoint.
const listenOff = "off"

// Module is the Container Registry service module.
type Module struct {
	backend  domain.Backend // set by WithBackend, or built from config in Register
	cfg      config.RegistryConfig
	front    *docker.Server // nil unless the harbor backend serves docker
	endpoint *endpoint
	tls      *tls.Config // serve the docker endpoint over TLS when set (Deps.TLS)
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// Option customizes the module.
type Option func(*Module)

// WithBackend replaces the configured image backend (tests pass a
// memory.Backend to add images without docker).
func WithBackend(b domain.Backend) Option { return func(m *Module) { m.backend = b } }

// New returns the registry module.
func New(opts ...Option) *Module {
	m := &Module{endpoint: &endpoint{}}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Name implements mocker.Module.
func (*Module) Name() string { return "registry" }

// Register builds the module and registers its gRPC services and authz rules.
func (m *Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	m.cfg = deps.Config.Registry
	m.tls = deps.TLS
	m.endpoint.configure(m.cfg)
	if err := m.buildBackend(); err != nil {
		return err
	}
	registries := dal.NewRegistryRepo(deps.Store)
	index := dal.NewArtifactIndex(deps.Store)
	regSvc, err := domain.NewRegistryService(domain.RegistryDeps{
		Registries: registries, Artifacts: index, Backend: m.backend, Hierarchy: deps.Authz, Endpoint: m.endpoint,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return err
	}
	artSvc, err := domain.NewArtifactService(registries, index, m.backend, deps.Ops, deps.IDs)
	if err != nil {
		return err
	}
	registerAuthz(deps, registries, artSvc)
	registrypb.RegisterRegistryServiceServer(reg, api.NewRegistryHandler(regSvc))
	registrypb.RegisterArtifactServiceServer(reg, api.NewArtifactHandler(artSvc))

	hc, isHarbor := m.backend.(*harbor.Client)
	if !isHarbor || m.cfg.Listen == listenOff {
		return nil
	}
	access, err := domain.NewAccessService(deps.Authenticator, deps.Authz, registries, deps.Config.Authz, deps.Log)
	if err != nil {
		return err
	}
	m.front, err = docker.New(access, docker.Upstream{URL: hc.URL(), Username: hc.Username(), Password: hc.Password()}, deps.Log)
	return err
}

// buildBackend picks the configured image backend unless WithBackend set one.
func (m *Module) buildBackend() error {
	if m.backend != nil {
		return nil
	}
	switch m.cfg.Backend {
	case config.RegistryMemory:
		m.backend = memory.New()
		return nil
	case config.RegistryHarbor:
		hc, err := harbor.New(harbor.Config{URL: m.cfg.HarborURL, Username: m.cfg.HarborUsername, Password: m.cfg.HarborPassword})
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		m.backend = hc
		return nil
	default:
		return fmt.Errorf("registry: unknown backend %d", m.cfg.Backend)
	}
}

func registerAuthz(deps mocker.Deps, registries *dal.RegistryRepo, artifacts *domain.ArtifactService) {
	deps.Authz.RegisterKind(domain.KindRegistry, func(ctx context.Context, id string) (string, error) {
		r, err := registries.Get(ctx, id)
		return r.GetMetadata().GetParentId(), err
	})
	deps.Authz.RegisterKind(domain.KindArtifact, artifacts.RegistryOf)
	api.RegisterAuthzRules(deps.Rules)
}

// Start serves the docker endpoint when the Harbor backend is configured.
func (m *Module) Start(ctx context.Context) error {
	if m.front == nil {
		return nil
	}
	addr, err := m.front.Start(ctx, m.cfg.Listen, m.tls)
	if err != nil {
		return err
	}
	m.endpoint.bound(addr)
	return nil
}

// Stop shuts the docker endpoint down.
func (m *Module) Stop(ctx context.Context) error {
	if m.front == nil {
		return nil
	}
	return m.front.Stop(ctx)
}

// endpoint computes status.registry_fqdn: the configured FQDN, else
// 127.0.0.1:<port of the docker endpoint> (the bound port once listening).
type endpoint struct {
	fqdn atomic.Pointer[string]
	set  bool // an explicit FQDN was configured
}

var _ domain.Endpoint = (*endpoint)(nil)

func (e *endpoint) configure(cfg config.RegistryConfig) {
	if cfg.FQDN != "" {
		e.set = true
		e.store(cfg.FQDN)
		return
	}
	port := portOf(config.DefaultRegistryListen)
	if p := portOf(cfg.Listen); p != "" && p != "0" {
		port = p
	}
	e.store(net.JoinHostPort("127.0.0.1", port))
}

func (e *endpoint) bound(addr net.Addr) {
	if e.set {
		return
	}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		e.store(net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port)))
	}
}

func (e *endpoint) store(v string) { e.fqdn.Store(&v) }

// FQDN implements domain.Endpoint.
func (e *endpoint) FQDN() string {
	if p := e.fqdn.Load(); p != nil {
		return *p
	}
	return ""
}

func portOf(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	return port
}
