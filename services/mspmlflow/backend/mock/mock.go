// Package mock is the default Managed MLflow backend
// (NEBIUS_MOCKER_MSP_MLFLOW_BACKEND=mock): no tracking server runs. Clusters
// get endpoints that resolve to nothing, which is enough for Terraform, the
// Go SDK and the CLI, which only manage clusters.
package mock

import (
	"context"
	"sync"

	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

// Name is the NEBIUS_MOCKER_MSP_MLFLOW_BACKEND value of this backend.
const Name = "mock"

// EndpointDomain is the DNS suffix of mock tracking endpoints. Names under
// .localhost resolve to the loopback address, where nothing serves them.
const EndpointDomain = "mlflow.mocker.localhost"

// Backend is the mock backend. It is safe for concurrent use.
type Backend struct {
	mu      sync.Mutex
	stopped map[string]bool // cluster ID -> stopped; present for every known cluster
}

var _ domain.Backend = (*Backend)(nil)

// New returns a mock backend.
func New() *Backend { return &Backend{stopped: map[string]bool{}} }

// Name implements domain.Backend.
func (*Backend) Name() string { return Name }

// EnsureCluster implements domain.Backend: the private endpoint is
// "<id>.private.mlflow.mocker.localhost", the public one (with
// PublicAccess) "<id>.mlflow.mocker.localhost".
func (b *Backend) EnsureCluster(_ context.Context, c domain.BackendCluster) (domain.Endpoints, error) {
	b.mu.Lock()
	b.stopped[c.ID] = false
	b.mu.Unlock()
	ep := domain.Endpoints{Private: c.ID + ".private." + EndpointDomain}
	if c.PublicAccess {
		ep.Public = c.ID + "." + EndpointDomain
	}
	return ep, nil
}

// StopCluster implements domain.Backend.
func (b *Backend) StopCluster(_ context.Context, clusterID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.stopped[clusterID]; ok {
		b.stopped[clusterID] = true
	}
	return nil
}

// DeleteCluster implements domain.Backend.
func (b *Backend) DeleteCluster(_ context.Context, clusterID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.stopped, clusterID)
	return nil
}

// ExperimentsCount implements domain.Backend: a mock server has none.
func (*Backend) ExperimentsCount(context.Context, string) (int, error) { return 0, nil }

// Close implements domain.Backend.
func (*Backend) Close(context.Context) error { return nil }

// Stopped reports whether the backend knows the cluster as stopped (for tests).
func (b *Backend) Stopped(clusterID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped[clusterID]
}
