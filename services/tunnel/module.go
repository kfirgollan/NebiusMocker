// Package tunnel is the NebiusMocker module for Nebius Tunnels
// (nebius.tunnel.v1.TunnelService, endpoint applicationtunnel.mkt).
//
// Tunnels are inventory only: the Nebius data plane is a proprietary agent
// that dials a regional edge over an undocumented protocol, so no agent can
// connect to the mock and every tunnel reads as DISCONNECTED with no
// services. See docs/services/tunnel.md.
package tunnel

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/api"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/dal"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/domain"
)

// Module is the Tunnels service module.
type Module struct{}

var _ mocker.Module = (*Module)(nil)

// New returns the Tunnels module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "tunnel" }

// Register wires the tunnel service and its authorization.
func (*Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	tunnels := dal.NewTunnelRepo(deps.Store)
	svc, err := domain.New(domain.Deps{
		Tunnels: tunnels, Hierarchy: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Config: domain.Config{ProjectID: deps.Config.ProjectID},
	})
	if err != nil {
		return fmt.Errorf("tunnel: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindTunnel, func(ctx context.Context, id string) (string, error) {
		t, err := tunnels.Get(ctx, id)
		return t.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)
	tunnelpb.RegisterTunnelServiceServer(reg, api.NewTunnelHandler(svc))
	return nil
}
