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
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/api"
	"github.com/kfirgollan/NebiusMocker/services/storage/backend/azblob"
	"github.com/kfirgollan/NebiusMocker/services/storage/backend/s3endpoint"
	"github.com/kfirgollan/NebiusMocker/services/storage/dal"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
	"github.com/kfirgollan/NebiusMocker/services/storage/s3"
)

// sweepInterval is how often expired soft-deleted buckets are purged and
// lifecycle rules are applied (on the injected clock).
const sweepInterval = time.Minute

// s3Off disables the S3 front (NEBIUS_MOCKER_STORAGE_S3_LISTEN=off).
const s3Off = "off"

// Module is the Object Storage service module.
type Module struct {
	s3Listen  string           // overrides config when set
	endpoints domain.Endpoints // overrides the transfer endpoint adapters when set

	mu      sync.Mutex
	s3      *s3.Server
	buckets *domain.BucketService
	invs    *domain.InventoryService
	xfers   *domain.TransferService
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

// WithTransferEndpoints replaces the adapters that data transfers use to reach
// their source and destination (tests inject an in-memory fake to run
// transfers without Docker or network).
func WithTransferEndpoints(e domain.Endpoints) Option { return func(m *Module) { m.endpoints = e } }

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
	content, ok := deps.ObjStore.(domain.ContentBackend)
	if !ok {
		return fmt.Errorf("storage: the object store %T does not support content inventory (objstore.Inventory)", deps.ObjStore)
	}
	svc, err := domain.NewBucketService(domain.BucketDeps{
		Buckets: buckets, Backend: deps.ObjStore, Content: content, Hierarchy: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Settings:    domain.Settings{Region: cfg.Region, Domain: cfg.S3Domain, PurgeGrace: cfg.PurgeGrace},
		Filesystems: peerFilesystems{deps.Peers}, Access: deps.Authz, Mode: deps.Config.Authz, Log: deps.Log,
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

	reports, ok := deps.ObjStore.(domain.ReportStore)
	if !ok {
		return fmt.Errorf("storage: the object store %T cannot write inventory reports", deps.ObjStore)
	}
	invRepo := dal.NewInventoryRepo(deps.Store)
	readLog := dal.NewAccessLogRepo(deps.Store)
	invs, err := domain.NewInventoryService(domain.InventoryDeps{
		Repo: invRepo, Buckets: buckets, Writer: domain.NewReportWriter(domain.ReportWriterDeps{Store: reports, Access: readLog}),
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Access: deps.Authz, Mode: deps.Config.Authz, Log: deps.Log,
	})
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindInventory, func(ctx context.Context, id string) (string, error) {
		inv, err := invRepo.Get(ctx, id)
		return inv.GetMetadata().GetParentId(), err
	})
	api.RegisterInventoryAuthzRules(deps.Rules)
	storagepb.RegisterInventoryServiceServer(reg, api.NewInventoryHandler(invs))

	xfers, err := m.registerTransfers(reg, deps, buckets)
	if err != nil {
		return err
	}

	listen := cfg.S3Listen
	if m.s3Listen != "" {
		listen = m.s3Listen
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets, m.invs, m.xfers, m.clk, m.log = svc, invs, xfers, deps.Clock, deps.Log
	if listen == s3Off {
		return nil
	}
	gate, err := domain.NewS3Gate(domain.GateDeps{
		Buckets: buckets, Keys: peerAccessKeys{deps.Peers}, Checker: deps.Authz, Hierarchy: deps.Authz, Mode: deps.Config.Authz,
		Members: func() domain.GroupMembers {
			if m, ok := peer.Lookup(deps.Peers, peer.GroupMembersKey); ok {
				return m
			}
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	m.s3 = s3.NewServer(s3.Config{Listen: listen, Domain: cfg.S3Domain, Region: cfg.Region}, gate, svc, upstreamFor(deps), deps.Clock, deps.Log)
	m.s3.AuditTo(deps.Peers)                                                         // data-plane audit events, when the audit module is enabled
	m.s3.TrackReads(domain.NewAccessTracker(readLog, invRepo, deps.Clock, deps.Log)) // LAST_ACCESS inventory field
	return nil
}

// registerTransfers wires both TransferService versions, their authorization
// and the endpoint adapters the iterations copy through.
func (m *Module) registerTransfers(reg grpc.ServiceRegistrar, deps mocker.Deps, buckets domain.BucketRepository) (*domain.TransferService, error) {
	repo := dal.NewTransferRepo(deps.Store)
	endpoints := m.endpoints
	if endpoints == nil {
		endpoints = transferEndpoints{
			s3:    s3endpoint.New(s3endpoint.Config{NebiusURL: m.frontURL, Clock: deps.Clock}),
			azure: azblob.New(nil),
		}
	}
	engine := domain.NewTransferEngine(endpoints, dal.NewManagedRepo(deps.Store), deps.Clock, deps.Log)
	svc, err := domain.NewTransferService(domain.TransferDeps{
		Repo: repo, Buckets: buckets, Hierarchy: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Region: deps.Config.Storage.Region, Engine: engine, Log: deps.Log,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindTransfer, func(ctx context.Context, id string) (string, error) {
		rec, err := repo.Get(ctx, id)
		if err != nil {
			return "", err
		}
		return rec.Metadata().GetParentId(), nil
	})
	api.RegisterTransferAuthzRules(deps.Rules)
	storagepb.RegisterTransferServiceServer(reg, api.NewTransferHandler(svc, deps.Config.Storage.Region))
	storagev1alpha1pb.RegisterTransferServiceServer(reg, api.NewTransferAlphaHandler(svc)) //nolint:staticcheck // SA1019: the deprecated v1alpha1 service is served on purpose
	return svc, nil
}

// transferEndpoints routes a transfer side to the adapter of its provider.
type transferEndpoints struct {
	s3    domain.Endpoints
	azure domain.Endpoints
}

func (t transferEndpoints) Open(ctx context.Context, side domain.Side) (domain.Endpoint, error) {
	if side.Kind == domain.ProviderAzureBlob {
		return t.azure.Open(ctx, side)
	}
	return t.s3.Open(ctx, side)
}

// frontURL is the base URL of the S3 front as a client on this machine
// reaches it ("" when the front is disabled or not started). An unspecified
// listen host (0.0.0.0, [::]) becomes the loopback address.
func (m *Module) frontURL() string {
	addr := m.S3Addr()
	if addr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
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

// peerFilesystems resolves filesystem-bucket filesystems through the compute
// module's capability, looked up per call (compute may be disabled).
type peerFilesystems struct{ d *peer.Directory }

func (p peerFilesystems) Filesystem(ctx context.Context, id string) (peer.Filesystem, error) {
	fs, ok := peer.Lookup(p.d, peer.FilesystemsKey)
	if !ok {
		return peer.Filesystem{}, errs.BadRequest(errs.Violation{
			Field: "spec.filesystem_bucket.filesystem_id", Message: "filesystem buckets need the compute module, which is not enabled",
		})
	}
	return fs.Filesystem(ctx, id)
}

func (p peerFilesystems) Expose(ctx context.Context, fsID, bucketID, projectID string) error {
	fs, ok := peer.Lookup(p.d, peer.FilesystemsKey)
	if !ok {
		return errs.BadRequest(errs.Violation{
			Field: "spec.filesystem_bucket.filesystem_id", Message: "filesystem buckets need the compute module, which is not enabled",
		})
	}
	return fs.Expose(ctx, fsID, bucketID, projectID)
}

func (p peerFilesystems) Unexpose(ctx context.Context, fsID, bucketID string) error {
	fs, ok := peer.Lookup(p.d, peer.FilesystemsKey)
	if !ok {
		return nil // nothing was exposed without compute
	}
	return fs.Unexpose(ctx, fsID, bucketID)
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
	m.wg.Go(func() { m.xfers.Run(ctx) })
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

// sweep purges expired soft-deleted buckets and applies lifecycle rules
// until ctx ends.
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
		r, err := m.buckets.ApplyLifecycles(ctx)
		if err != nil {
			m.log.WarnContext(ctx, "apply lifecycle rules", "result", r, "error", err)
		} else if r != (domain.LifecycleResult{}) {
			m.log.InfoContext(ctx, "applied lifecycle rules", "expired", r.Expired, "noncurrent_expired", r.NoncurrentExpired,
				"markers_removed", r.MarkersRemoved, "uploads_aborted", r.UploadsAborted)
		}
		m.sweepInventories(ctx)
	}
}

// sweepInventories drops the inventories of purged buckets and writes the
// reports that are due.
func (m *Module) sweepInventories(ctx context.Context) {
	if n, err := m.invs.SweepOrphans(ctx); err != nil {
		m.log.WarnContext(ctx, "remove inventories of purged buckets", "removed", n, "error", err)
	} else if n > 0 {
		m.log.InfoContext(ctx, "removed inventories of purged buckets", "removed", n)
	}
	r, err := m.invs.RunDue(ctx)
	if err != nil {
		m.log.WarnContext(ctx, "run inventory reports", "reports", r.Reports, "failures", r.Failures, "error", err)
	} else if r != (domain.InventoryRunResult{}) {
		m.log.InfoContext(ctx, "ran inventory reports", "reports", r.Reports, "failures", r.Failures)
	}
}
