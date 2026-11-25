// Package ai is the NebiusMocker module for Nebius AI serverless endpoints
// (apps.msp, nebius.ai.v1.EndpointService).
//
// The inventory is real: specs are validated, references checked, and
// endpoints move through their lifecycle as operations. Execution is a stub:
// no container runs, and the endpoint URLs point at a small HTTP listener that
// answers authorized calls with HTTP 200. See docs/services/ai.md.
package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"

	"google.golang.org/grpc"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/services/ai/api"
	"github.com/kfirgollan/NebiusMocker/services/ai/dal"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

// Module is the AI service module.
type Module struct {
	listen    *string // overrides Config.AI.Listen when set
	tcpListen *string // overrides Config.AI.TCPListen when set
	tlsDir    *string // overrides Config.AI.TLSDir when set

	cfg       config.AIConfig
	clk       clock.Clock
	svc       *domain.EndpointService
	dataPlane *api.DataPlane
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// Option customizes the module.
type Option func(*Module)

// WithListen overrides the HTTPS and TCP stub listen addresses (tests use
// "127.0.0.1:0").
func WithListen(addr string) Option {
	return func(m *Module) { m.listen, m.tcpListen = &addr, &addr }
}

// WithTLSDir overrides the directory of the local CA (tests use a temp dir
// and trust <dir>/ca.pem, as a user would).
func WithTLSDir(dir string) Option { return func(m *Module) { m.tlsDir = &dir } }

// New returns the AI module.
func New(opts ...Option) *Module {
	m := &Module{}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Name implements mocker.Module.
func (*Module) Name() string { return "ai" }

// Register wires the endpoint service and registers its gRPC server.
func (m *Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	m.cfg = deps.Config.AI
	if m.listen != nil {
		m.cfg.Listen = *m.listen
	}
	if m.tcpListen != nil {
		m.cfg.TCPListen = *m.tcpListen
	}
	if m.tlsDir != nil {
		m.cfg.TLSDir = *m.tlsDir
	}
	m.clk = deps.Clock
	repo := dal.NewEndpointRepo(deps.Store)
	m.dataPlane = api.NewDataPlane(deps.Log)
	svc, err := domain.NewEndpointService(domain.EndpointDeps{
		Endpoints: repo, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Hierarchy: deps.Authz, Secrets: peerSecrets{dir: deps.Peers}, DataPlane: m.dataPlane,
		Perms:   callerPerms{engine: deps.Authz, mode: deps.Config.Authz, log: deps.Log},
		Catalog: catalog(m.cfg.Platforms), Log: deps.Log,
	})
	if err != nil {
		return fmt.Errorf("ai: %w", err)
	}
	m.svc = svc
	deps.Authz.RegisterKind(domain.KindEndpoint, func(ctx context.Context, id string) (string, error) {
		e, err := repo.Get(ctx, id)
		return e.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)
	aipb.RegisterEndpointServiceServer(reg, api.NewEndpointHandler(svc))
	return nil
}

// Start implements mocker.Lifecycle: it starts the stub data plane.
func (m *Module) Start(ctx context.Context) error {
	if m.svc == nil {
		return errors.New("ai: Start before Register")
	}
	ca, err := localca.LoadOrCreate(m.cfg.TLSDir, m.clk)
	if err != nil {
		return fmt.Errorf("ai: %w", err)
	}
	cert, err := ca.Issue(certHosts(m.cfg)...)
	if err != nil {
		return fmt.Errorf("ai: %w", err)
	}
	return m.dataPlane.Start(ctx, m.svc, api.DataPlaneConfig{
		Listen: m.cfg.Listen, PublicURL: m.cfg.PublicURL,
		TCPListen: m.cfg.TCPListen, TCPDomain: m.cfg.TCPDomain, Cert: cert,
	})
}

// CAFile is the CA certificate clients trust to reach the data plane.
func (m *Module) CAFile() string { return filepath.Join(m.cfg.TLSDir, localca.CertFile) }

// certHosts are the names the data-plane certificate is valid for.
func certHosts(cfg config.AIConfig) []string {
	hosts := []string{"127.0.0.1", "localhost", "::1", "*." + cfg.TCPDomain}
	if u, err := url.Parse(cfg.PublicURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	if h, _, err := net.SplitHostPort(cfg.Listen); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
		hosts = append(hosts, h)
	}
	return hosts
}

// Stop implements mocker.Lifecycle.
func (m *Module) Stop(ctx context.Context) error {
	if m.dataPlane == nil {
		return nil
	}
	return m.dataPlane.Stop(ctx)
}

// DataPlaneURL is the base URL of the stub data plane, once started.
func (m *Module) DataPlaneURL() string {
	if m.dataPlane == nil {
		return ""
	}
	return m.dataPlane.BaseURL()
}

// callerPerms checks the caller's own permissions on referenced resources
// with the authz engine, under the configured mode (as the RPC interceptor
// does for the RPC's own rule).
type callerPerms struct {
	engine *authz.Engine
	mode   authz.Mode
	log    *slog.Logger
}

func (c callerPerms) Require(ctx context.Context, perm, resourceID string) error {
	if c.mode == authz.ModeOff {
		return nil
	}
	p := reqctx.PrincipalFrom(ctx)
	var denied error
	if p.Anonymous() {
		denied = errs.Unauthenticated("authorization requires credentials")
	} else {
		d, err := c.engine.Check(ctx, p.ID, authz.Permission(perm), resourceID)
		if err != nil {
			return err
		}
		if d.Allowed {
			return nil
		}
		denied = &errs.Error{
			Kind: errs.KindPermissionDenied, ResourceID: resourceID,
			Message: "permission " + perm + " denied on " + resourceID,
		}
	}
	c.log.WarnContext(ctx, "authz denied", "permission", perm, "resource", resourceID,
		"principal", p.ID, "enforced", c.mode == authz.ModeEnforce)
	if c.mode == authz.ModeEnforce {
		return denied
	}
	return nil
}

// peerSecrets finds the MysteryBox capability at call time.
type peerSecrets struct{ dir *peer.Directory }

func (p peerSecrets) SecretPayloads() (peer.SecretPayloads, bool) {
	return peer.Lookup(p.dir, peer.SecretPayloadsKey)
}

func catalog(ps []config.AIPlatform) domain.Catalog {
	out := make(domain.Catalog, 0, len(ps))
	for _, p := range ps {
		presets := make([]domain.Preset, 0, len(p.Presets))
		for _, pr := range p.Presets {
			presets = append(presets, domain.Preset{Name: pr.Name, GPUs: pr.GPUs, VCPUs: pr.VCPUs})
		}
		out = append(out, domain.Platform{Name: p.Name, GPU: p.GPU, CPU: p.CPU, Presets: presets})
	}
	return out
}
