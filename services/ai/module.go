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

	"google.golang.org/grpc"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/ai/api"
	"github.com/kfirgollan/NebiusMocker/services/ai/dal"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

// Module is the AI service module.
type Module struct {
	listen *string // overrides Config.AI.Listen when set

	cfg       config.AIConfig
	svc       *domain.EndpointService
	dataPlane *api.DataPlane
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// Option customizes the module.
type Option func(*Module)

// WithListen overrides the data-plane listen address (tests use
// "127.0.0.1:0").
func WithListen(addr string) Option { return func(m *Module) { m.listen = &addr } }

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
	repo := dal.NewEndpointRepo(deps.Store)
	m.dataPlane = api.NewDataPlane(deps.Log)
	svc, err := domain.NewEndpointService(domain.EndpointDeps{
		Endpoints: repo, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Hierarchy: deps.Authz, Secrets: peerSecrets{dir: deps.Peers}, DataPlane: m.dataPlane,
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
	return m.dataPlane.Start(ctx, m.svc, m.cfg.Listen, m.cfg.PublicURL)
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
