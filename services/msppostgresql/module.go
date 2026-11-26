// Package msppostgresql is the NebiusMocker module for Nebius Managed
// PostgreSQL (nebius.msp.postgresql.v1alpha1): clusters and their backups.
// The control plane (resources, validation, operations, backup records and
// schedule) is the mock's; PostgreSQL itself runs in the backend
// NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND selects.
package msppostgresql

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/api"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/dal"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// sweepInterval is how often the backup schedule and retention are applied
// (on the injected clock).
const sweepInterval = time.Minute

// Module is the Managed PostgreSQL service module.
type Module struct {
	mu           sync.Mutex
	backend      domain.Backend
	svc          *domain.Service
	clk          clock.Clock
	log          *slog.Logger
	stop         context.CancelFunc
	diskInterval time.Duration
	wg           sync.WaitGroup
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// New returns the Managed PostgreSQL module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "msp-postgresql" }

// Register wires the cluster and backup services, their authorization and
// the backend.
func (m *Module) Register(ctx context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	backend, err := newBackend(ctx, deps.Config, deps)
	if err != nil {
		return err
	}
	clusters := dal.NewClusterRepo(deps.Store)
	backups := dal.NewBackupRepo(deps.Store)
	svc, err := domain.New(domain.Deps{
		Clusters: clusters, Backups: backups, Bindings: dal.NewNetworkBindingRepo(deps.Store), Backend: backend,
		Peers: peers{deps.Peers}, Hierarchy: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Log: deps.Log,
		Config: domain.Config{ProjectID: deps.Config.ProjectID, MSP: deps.Config.MSP, Detached: detached(backend)},
	})
	if err != nil {
		return fmt.Errorf("msppostgresql: %w", err)
	}
	m.mu.Lock()
	m.backend, m.svc, m.clk, m.log = backend, svc, deps.Clock, deps.Log
	m.diskInterval = deps.Config.MSP.PostgresDiskCheckInterval
	if m.diskInterval <= 0 {
		m.diskInterval = time.Minute
	}
	m.mu.Unlock()
	deps.Authz.RegisterKind(domain.KindCluster, func(ctx context.Context, id string) (string, error) {
		c, err := clusters.Get(ctx, id)
		if err == nil {
			return c.GetMetadata().GetParentId(), nil
		}
		if !errs.IsKind(err, errs.KindNotFound) {
			return "", err
		}
		// A deleted cluster's backups stay readable, authorized through the
		// project they were taken in.
		page, berr := backups.ListByCluster(ctx, id, 1, "")
		if berr != nil || len(page.Items) == 0 {
			return "", err
		}
		return page.Items[0].ProjectID, nil
	})
	api.RegisterAuthzRules(deps.Rules)
	pgpb.RegisterClusterServiceServer(reg, api.NewClusterHandler(svc))
	pgpb.RegisterBackupServiceServer(reg, api.NewBackupHandler(svc))
	return nil
}

// Start runs the backup sweeper.
func (m *Module) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, m.stop = context.WithCancel(ctx)
	m.wg.Go(func() { m.sweep(ctx) })
	m.wg.Go(func() { m.checkDisks(ctx) })
	return nil
}

// Stop stops the sweeper and releases what the backend holds.
func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	b, stop := m.backend, m.stop
	m.mu.Unlock()
	if stop != nil {
		stop()
	}
	m.wg.Wait()
	if b == nil {
		return nil
	}
	return b.Close(ctx)
}

// checkDisks measures the disk of the running clusters every
// NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_CHECK_INTERVAL until ctx ends.
func (m *Module) checkDisks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.clk.After(m.diskInterval):
		}
		if err := m.svc.CheckDisks(ctx); err != nil && ctx.Err() == nil {
			m.log.WarnContext(ctx, "postgresql disk check", "error", err)
		}
	}
}

// sweep applies the backup schedule and retention until ctx ends.
func (m *Module) sweep(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.clk.After(sweepInterval):
		}
		if err := m.svc.Sweep(ctx); err != nil && ctx.Err() == nil {
			m.log.WarnContext(ctx, "postgresql backup sweep", "error", err)
		}
	}
}
