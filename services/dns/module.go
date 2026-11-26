// Package dns is the NebiusMocker module for Nebius DNS (nebius.dns.v1).
//
// Implemented: ZoneService and RecordService (custom private zones bound to a
// VPC network, with the service-managed SOA and NS records), and a DNS
// responder that serves the zones over UDP and TCP
// (NEBIUS_MOCKER_DNS_LISTEN). See docs/services/dns.md.
package dns

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"

	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/dns/api"
	"github.com/kfirgollan/NebiusMocker/services/dns/dal"
	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
	"github.com/kfirgollan/NebiusMocker/services/dns/responder"
)

// Module is the DNS service module.
type Module struct {
	listen string // overrides config when set

	mu  sync.Mutex
	srv *responder.Server
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// Option customizes the module.
type Option func(*Module)

// WithListen overrides NEBIUS_MOCKER_DNS_LISTEN (tests use "127.0.0.1:0" or "off").
func WithListen(addr string) Option { return func(m *Module) { m.listen = addr } }

// New returns the DNS module.
func New(opts ...Option) *Module {
	m := &Module{}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Name implements mocker.Module.
func (*Module) Name() string { return "dns" }

// Register wires the zone and record services, their authorization and the
// DNS responder.
func (m *Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	zones := dal.NewZoneRepo(deps.Store)
	records := dal.NewRecordRepo(deps.Store)
	svc, err := domain.New(domain.Deps{
		Zones: zones, Records: records, Networks: peerNetworks{deps.Peers}, Hierarchy: deps.Authz,
		Quota: peer.QuotaGateFor(deps.Peers),
		Ops:   deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Config: domain.Config{ProjectID: deps.Config.ProjectID},
	})
	if err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	if err := peer.Provide[peer.QuotaUsageSource](deps.Peers, peer.QuotaUsageKey("dns"), svc); err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindZone, func(ctx context.Context, id string) (string, error) {
		z, err := zones.Get(ctx, id)
		return z.GetMetadata().GetParentId(), err
	})
	deps.Authz.RegisterKind(domain.KindRecord, func(ctx context.Context, id string) (string, error) {
		r, err := records.Get(ctx, id)
		return r.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)
	dnspb.RegisterZoneServiceServer(reg, api.NewZoneHandler(svc))
	dnspb.RegisterRecordServiceServer(reg, api.NewRecordHandler(svc))

	listen := deps.Config.DNS.Listen
	if m.listen != "" {
		listen = m.listen
	}
	if listen == config.DNSOff {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.srv = responder.New(listen, svc, deps.Log)
	return nil
}

// Start opens the DNS responder.
func (m *Module) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srv == nil {
		return nil
	}
	return m.srv.Start(ctx)
}

// Stop closes the DNS responder.
func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	srv := m.srv
	m.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Stop(ctx)
}

// DNSAddr is the responder's bound address, "host:port" for both UDP and TCP
// ("" when disabled or not started).
func (m *Module) DNSAddr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srv == nil || m.srv.Addr() == nil {
		return ""
	}
	return m.srv.Addr().String()
}

// peerNetworks resolves networks through the vpc module's capability,
// looked up per call (module order is not guaranteed; vpc may be disabled).
type peerNetworks struct{ d *peer.Directory }

var _ domain.Networks = peerNetworks{}

func (p peerNetworks) lookup() (peer.Networks, error) {
	n, ok := peer.Lookup(p.d, peer.NetworksKey)
	if !ok {
		return nil, errs.BadRequest(errs.Violation{
			Field: "spec.vpc.primary_network_id", Message: "DNS zones are attached to VPC networks, and the vpc module is not enabled",
		})
	}
	return n, nil
}

func (p peerNetworks) Network(ctx context.Context, id string) (domain.Network, error) {
	n, err := p.lookup()
	if err != nil {
		return domain.Network{}, err
	}
	nw, err := n.Network(ctx, id)
	if err != nil {
		return domain.Network{}, err
	}
	return domain.Network{ID: nw.ID, ProjectID: nw.ProjectID}, nil
}

func (p peerNetworks) Bind(ctx context.Context, networkID, zoneID string) error {
	n, err := p.lookup()
	if err != nil {
		return err
	}
	return n.Bind(ctx, networkID, zoneID)
}

func (p peerNetworks) Unbind(ctx context.Context, networkID, zoneID string) error {
	n, ok := peer.Lookup(p.d, peer.NetworksKey)
	if !ok {
		return nil // nothing was bound without vpc
	}
	return n.Unbind(ctx, networkID, zoneID)
}
