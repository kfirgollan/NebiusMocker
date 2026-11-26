// Package mspmlflow is the NebiusMocker module for Nebius Managed MLflow
// (nebius.msp.mlflow.v1alpha1): tracking server clusters. The control plane
// (resources, validation, phases, operations, the artifact bucket and its
// access key) is the mock's; the tracking server itself runs in the backend
// NEBIUS_MOCKER_MSP_MLFLOW_BACKEND selects.
package mspmlflow

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"

	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/api"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/dal"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

// Module is the Managed MLflow service module.
type Module struct {
	mu      sync.Mutex
	backend domain.Backend
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// New returns the Managed MLflow module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "msp-mlflow" }

// Register wires the cluster service, its authorization and the backend.
func (m *Module) Register(ctx context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	backend, err := newBackend(ctx, deps.Config, deps)
	if err != nil {
		return err
	}
	clusters := dal.NewClusterRepo(deps.Store)
	msp := deps.Config.MSP
	svc, err := domain.New(domain.Deps{
		Clusters: clusters, Internals: dal.NewInternalsRepo(deps.Store), Backend: backend, Peers: peers{deps.Peers}, Hierarchy: deps.Authz,
		Quota: peer.QuotaGateFor(deps.Peers), Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Log: deps.Log,
		Config: domain.Config{
			ProjectID: deps.Config.ProjectID, MLflowVersion: msp.MLflowVersion, Sizes: msp.MLflowSizes, SizeOrder: msp.MLflowSizeOrder,
			Detached: true, // Work creates a bucket through a nested operation, which the engine's own goroutine cannot await
		},
	})
	if err != nil {
		return fmt.Errorf("msp-mlflow: %w", err)
	}
	m.mu.Lock()
	m.backend = backend
	m.mu.Unlock()
	deps.Authz.RegisterKind(domain.KindCluster, func(ctx context.Context, id string) (string, error) {
		c, err := clusters.Get(ctx, id)
		return c.GetMetadata().GetParentId(), err
	})
	if err := peer.Provide(deps.Peers, peer.QuotaUsageKey(m.Name()), dal.NewQuotaUsage(deps.Store, svc.QuotaAmounts)); err != nil {
		return fmt.Errorf("msp-mlflow: %w", err)
	}
	api.RegisterAuthzRules(deps.Rules)
	mlflowpb.RegisterClusterServiceServer(reg, api.NewClusterHandler(svc))
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

// peers finds the capabilities the module uses at call time (module order
// is not guaranteed; a provider may be disabled).
type peers struct{ d *peer.Directory }

var _ domain.Peers = peers{}

func (p peers) Networks() (peer.Networks, bool) { return peer.Lookup(p.d, peer.NetworksKey) }

func (p peers) DefaultSubnets() (peer.DefaultSubnets, bool) {
	return peer.Lookup(p.d, peer.DefaultSubnetsKey)
}

func (p peers) ProjectRegions() (peer.ProjectRegions, bool) {
	return peer.Lookup(p.d, peer.ProjectRegionsKey)
}

func (p peers) Buckets() (peer.Buckets, bool) { return peer.Lookup(p.d, peer.BucketsKey) }

func (p peers) SystemAccessKeys() (peer.SystemAccessKeys, bool) {
	return peer.Lookup(p.d, peer.SystemAccessKeysKey)
}
