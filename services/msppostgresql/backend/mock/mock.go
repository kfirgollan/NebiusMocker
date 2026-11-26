// Package mock is the default Managed PostgreSQL backend
// (NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND=mock): no PostgreSQL runs. Clusters
// get endpoints that resolve to nothing, and backups are records without
// data. It is enough for Terraform, the Go SDK and the CLI, which only manage
// clusters; use the docker backend to connect to a database.
package mock

import (
	"context"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// Name is the NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND value of this backend.
const Name = "mock"

// EndpointDomain is the DNS suffix of mock cluster endpoints. Names under
// .localhost resolve to the loopback address, where nothing listens.
const EndpointDomain = "postgresql.mocker.localhost"

const port = "5432"

// Backend is the mock backend. It is safe for concurrent use.
type Backend struct {
	mu       sync.Mutex
	clusters map[string]bool // cluster ID → running
}

var _ domain.Backend = (*Backend)(nil)

// New returns a mock backend.
func New() *Backend { return &Backend{clusters: map[string]bool{}} }

// Name implements domain.Backend.
func (*Backend) Name() string { return Name }

// EnsureCluster implements domain.Backend: the cluster is up at once.
func (b *Backend) EnsureCluster(_ context.Context, c domain.BackendCluster) (domain.Endpoints, error) {
	b.mu.Lock()
	b.clusters[c.ID] = true
	b.mu.Unlock()
	ep := domain.Endpoints{
		PrivateReadWrite: c.ID + ".rw." + EndpointDomain + ":" + port,
		PrivateReadOnly:  c.ID + ".ro." + EndpointDomain + ":" + port,
	}
	if c.PublicAccess {
		ep.PublicReadWrite = c.ID + ".public-rw." + EndpointDomain + ":" + port
		ep.PublicReadOnly = c.ID + ".public-ro." + EndpointDomain + ":" + port
	}
	return ep, nil
}

// StopCluster implements domain.Backend.
func (b *Backend) StopCluster(_ context.Context, clusterID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.clusters[clusterID]; ok {
		b.clusters[clusterID] = false
	}
	return nil
}

// DeleteCluster implements domain.Backend.
func (b *Backend) DeleteCluster(_ context.Context, clusterID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clusters, clusterID)
	return nil
}

// CreateBackup implements domain.Backend: a backup is a record of size 0. A
// cluster the backend does not know (it has no state across restarts) is
// backed up too; a stopped one is not.
func (b *Backend) CreateBackup(_ context.Context, clusterID, _ string) (domain.BackupInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if running, ok := b.clusters[clusterID]; ok && !running {
		return domain.BackupInfo{}, errs.BadState(clusterID, "the cluster is not running")
	}
	return domain.BackupInfo{}, nil
}

// DeleteBackup implements domain.Backend: there is no data to remove.
func (*Backend) DeleteBackup(context.Context, string, string, time.Time) error { return nil }

// Close implements domain.Backend.
func (*Backend) Close(context.Context) error { return nil }
