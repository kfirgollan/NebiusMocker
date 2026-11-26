// Package mk8s is the NebiusMocker module for Nebius Managed Kubernetes
// (nebius.mk8s.v1): clusters and node groups. The control plane (resources,
// validation, versions, operations) is the mock's; node group nodes are
// compute instances (through pkg/peer), and Kubernetes itself runs in the
// backend NEBIUS_MOCKER_MK8S_BACKEND selects. See docs/services/mk8s.md.
package mk8s

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	mk8sv1alpha1pb "github.com/nebius/gosdk/proto/nebius/mk8s/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/api"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/dal"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// Module is the mk8s service module.
type Module struct {
	mu      sync.Mutex
	backend domain.Backend
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// New returns the mk8s module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "mk8s" }

// Register wires the cluster and node group services, their authorization,
// the backend and the peer.MK8sClusters capability.
func (m *Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	backend, err := newBackend(deps.Config, deps)
	if err != nil {
		return err
	}
	clusters := dal.NewClusterRepo(deps.Store)
	groups := dal.NewNodeGroupRepo(deps.Store)
	svc, err := domain.New(domain.Deps{
		Clusters: clusters, NodeGroups: groups, Bindings: dal.NewNetworkBindingRepo(deps.Store), Backend: backend, Peers: peers{deps.Peers}, Hierarchy: deps.Authz,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Config: domain.Config{
			ProjectID: deps.Config.ProjectID, Versions: deps.Config.MK8s.Versions, Detached: detached(backend),
			RolloutStep: deps.Config.MK8s.RolloutStep,
		},
	})
	if err != nil {
		return fmt.Errorf("mk8s: %w", err)
	}
	m.mu.Lock()
	m.backend = backend
	m.mu.Unlock()
	deps.Authz.RegisterKind(domain.KindCluster, func(ctx context.Context, id string) (string, error) {
		c, err := clusters.Get(ctx, id)
		return c.GetMetadata().GetParentId(), err
	})
	deps.Authz.RegisterKind(domain.KindNodeGroup, func(ctx context.Context, id string) (string, error) {
		g, err := groups.Get(ctx, id)
		return g.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)
	api.RegisterAlphaAuthzRules(deps.Rules)
	if err := peer.Provide[peer.MK8sClusters](deps.Peers, peer.MK8sClustersKey, svc); err != nil {
		return fmt.Errorf("mk8s: %w", err)
	}
	mk8spb.RegisterClusterServiceServer(reg, api.NewClusterHandler(svc))
	mk8spb.RegisterNodeGroupServiceServer(reg, api.NewNodeGroupHandler(svc))
	// The deprecated v1alpha1 services are a second view of the same resources.
	mk8sv1alpha1pb.RegisterClusterServiceServer(reg, api.NewAlphaClusterHandler(svc))     //nolint:staticcheck // SA1019: v1alpha1 is deprecated upstream, still served
	mk8sv1alpha1pb.RegisterNodeGroupServiceServer(reg, api.NewAlphaNodeGroupHandler(svc)) //nolint:staticcheck // SA1019: v1alpha1 is deprecated upstream, still served
	return nil
}

// Start implements mocker.Lifecycle; the backends start nothing up front.
func (*Module) Start(context.Context) error { return nil }

// Stop releases what the backend holds.
func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	b := m.backend
	m.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.Close(ctx)
}
