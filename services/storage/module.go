// Package storage is the NebiusMocker module for Nebius Object Storage
// (cpl.storage): nebius.storage.v1.BucketService on the gRPC edge, and an S3
// front (default :4580) that authenticates Nebius access keys and relays
// object traffic to RustFS. The mocker stores no object data itself; see
// docs/services/storage.md.
package storage

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/api"
	"github.com/kfirgollan/NebiusMocker/services/storage/dal"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
	"github.com/kfirgollan/NebiusMocker/services/storage/s3"
)

// sweepInterval is how often expired soft-deleted buckets are purged.
const sweepInterval = time.Minute

// s3Off disables the S3 front (NEBIUS_MOCKER_STORAGE_S3_LISTEN=off).
const s3Off = "off"

// Module is the Object Storage service module.
type Module struct {
	s3Listen string // overrides config when set

	mu      sync.Mutex
	s3      *s3.Server
	buckets *domain.BucketService
	clk     clock.Clock
	log     *slog.Logger
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// Option customizes the module.
type Option func(*Module)

// WithS3Listen overrides NEBIUS_MOCKER_STORAGE_S3_LISTEN (tests use "127.0.0.1:0").
func WithS3Listen(addr string) Option { return func(m *Module) { m.s3Listen = addr } }

// New returns the Object Storage module.
func New(opts ...Option) *Module {
	m := &Module{}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Name implements mocker.Module.
func (*Module) Name() string { return "storage" }

// Register wires the bucket service, its authorization and the S3 front.
func (m *Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	cfg := deps.Config.Storage
	buckets := dal.NewBucketRepo(deps.Store)
	svc, err := domain.NewBucketService(domain.BucketDeps{
		Buckets: buckets, Backend: deps.ObjStore, Hierarchy: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Settings: domain.Settings{Region: cfg.Region, Domain: cfg.S3Domain, PurgeGrace: cfg.PurgeGrace},
	})
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindBucket, func(ctx context.Context, id string) (string, error) {
		b, err := buckets.Get(ctx, id)
		return b.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)
	if err := peer.Provide[peer.BucketResolver](deps.Peers, peer.BucketResolverKey, svc); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	storagepb.RegisterBucketServiceServer(reg, api.NewBucketHandler(svc))

	listen := cfg.S3Listen
	if m.s3Listen != "" {
		listen = m.s3Listen
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets, m.clk, m.log = svc, deps.Clock, deps.Log
	if listen == s3Off {
		return nil
	}
	gate, err := domain.NewS3Gate(buckets, peerAccessKeys{deps.Peers}, deps.Authz, deps.Config.Authz)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	m.s3 = s3.NewServer(s3.Config{Listen: listen, Domain: cfg.S3Domain, Region: cfg.Region}, gate, upstreamFor(deps), deps.Clock, deps.Log)
	return nil
}

// peerAccessKeys resolves access keys through the iam module's capability,
// looked up per request (module order is not guaranteed; iam may be disabled).
type peerAccessKeys struct{ d *peer.Directory }

func (p peerAccessKeys) ResolveAccessKey(ctx context.Context, id string) (peer.AccessKey, error) {
	keys, ok := peer.Lookup(p.d, peer.AccessKeysKey)
	if !ok {
		return peer.AccessKey{}, errs.Unauthenticated("no access-key source (the iam module is not enabled)")
	}
	return keys.ResolveAccessKey(ctx, id)
}

// upstreamFor picks the S3 relay for the configured object store.
func upstreamFor(deps mocker.Deps) s3.Upstream {
	if c, ok := deps.ObjStore.(*rustfs.Client); ok {
		return c.Forwarder(deps.Clock, deps.Log)
	}
	return s3.NoDataPlane{}
}

// Start opens the S3 front and starts the purge sweeper.
func (m *Module) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.s3 != nil {
		if err := m.s3.Start(ctx); err != nil {
			return err
		}
	}
	ctx, m.stop = context.WithCancel(ctx)
	m.wg.Go(func() { m.sweep(ctx) })
	return nil
}

// Stop closes the S3 front and stops the sweeper.
func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	srv, stop := m.s3, m.stop
	m.mu.Unlock()
	if stop != nil {
		stop()
	}
	m.wg.Wait()
	if srv != nil {
		return srv.Stop(ctx)
	}
	return nil
}

// S3Addr is the bound S3 front address ("" when disabled or not started).
func (m *Module) S3Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.s3 == nil || m.s3.Addr() == nil {
		return ""
	}
	return m.s3.Addr().String()
}

// sweep purges expired soft-deleted buckets until ctx ends.
func (m *Module) sweep(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.clk.After(sweepInterval):
		}
		if n, err := m.buckets.SweepExpired(ctx); err != nil {
			m.log.WarnContext(ctx, "purge expired buckets", "purged", n, "error", err)
		} else if n > 0 {
			m.log.InfoContext(ctx, "purged expired buckets", "purged", n)
		}
	}
}
