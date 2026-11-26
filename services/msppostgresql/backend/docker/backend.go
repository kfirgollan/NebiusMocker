// Package docker is the Managed PostgreSQL backend that runs real PostgreSQL
// (NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND=docker): per cluster one Docker
// network, one primary and Hosts-1 streaming replicas (postgres alpine
// images) and a PgBouncer in front of the primary, all on the local Docker
// host through the Engine API.
//
// # Endpoints
//
//   - PrivateReadWrite is "<EndpointHost>:<port>" of the pooler (container
//     port 5432, published on NEBIUS_MOCKER_MSP_BIND_IP, a port Docker picks).
//     It reaches the primary.
//   - PrivateReadOnly is "<EndpointHost>:<port>" of the first replica's
//     PostgreSQL, or of the primary itself when the cluster has one host. It
//     is PostgreSQL directly, without the pooler.
//   - Public* repeat the private values when the cluster has public access.
//
// Docker picks new ports when a stopped cluster starts or a host is
// recreated, so EnsureCluster always returns the current endpoints.
//
// # TLS
//
// Every cluster gets one server certificate from the mocker's local CA
// (hosts: the endpoint host, localhost, 127.0.0.1, host.docker.internal and
// the in-network names). PostgreSQL and PgBouncer run with it, so clients can
// use sslmode=verify-full with <NEBIUS_MOCKER_MSP_TLS_DIR>/ca.pem, and plain
// or "prefer" clients still connect. The certificate files are copied into the
// containers with their owner set to uid 70 (postgres in both images) and the
// key mode 0600, which PostgreSQL requires.
//
// # Containers, volumes, labels
//
// Names: network nmock-pg-<id>, hosts nmock-pg-<id>-<n> (network alias
// pg-<n>, data volume of the same name), pooler nmock-pg-<id>-pooler. One
// backups volume per mocker, nmock-pg-backups-<owner>, is mounted at /backups
// on every host. Every object carries nebius-mocker.backend=msp-postgresql
// and nebius-mocker.owner=<edge address>; containers and volumes also the
// cluster ID. Startup and Close remove what carries this mocker's owner
// label. A host or the pooler carries nebius-mocker.spec=<hash> of what it was
// created with; a changed hash recreates the container (keeping its volume).
//
// # Replication
//
// The primary archives WAL to /backups/<cluster>/wal. Replicas start from
// pg_basebackup of the primary over the role nmock_replicator and then
// stream. Hosts differ only in their entrypoint.
//
// # Pooler authentication
//
// PgBouncer authenticates clients with auth_query: the role nmock_pooler
// calls a SECURITY DEFINER function owned by the bootstrap user (database
// "postgres"), which returns a user's SCRAM secret from pg_shadow. Users and
// password changes therefore need no pooler restart. The pooler reaches
// PostgreSQL with server_tls_sslmode=require.
//
// # Backup and point-in-time recovery
//
// CreateBackup runs pg_basebackup (tar, gzip, WAL fetched) into
// /backups/<cluster>/base/<backup>. A restore extracts it into the new
// cluster's data volume with a one-shot helper container, adds recovery.signal
// and a restore_command reading the source cluster's WAL archive, and lets
// PostgreSQL recover to recovery_target_time (or just the backup's end) and
// promote. The source's archive is flushed first (pg_switch_wal) so recent
// commits are in it.
package docker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// Name is the NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND value of this backend.
const Name = "docker"

// Labels on every Docker object the backend creates.
const (
	LabelBackend = "nebius-mocker.backend"
	LabelOwner   = "nebius-mocker.owner"
	LabelCluster = "nebius-mocker.cluster"
	LabelHost    = "nebius-mocker.host"
	LabelRole    = "nebius-mocker.role"
	labelSpec    = "nebius-mocker.spec"
)

// backendLabel is the LabelBackend value.
const backendLabel = "msp-postgresql"

// Container roles (LabelRole).
const (
	roleHost   = "host"
	rolePooler = "pooler"
	roleHelper = "helper"
)

const (
	pgPort = "5432/tcp"
	// pgUID is postgres in the PostgreSQL and PgBouncer images.
	pgUID = 70
	// stopTimeout is how long PostgreSQL gets to shut down cleanly.
	stopTimeout = 30 * time.Second
)

// Docker is the part of the Docker Engine API the backend uses
// (*dockerhost.Client; dockerfake.Fake in tests).
type Docker interface {
	Host() string
	Ping(ctx context.Context) error
	Info(ctx context.Context) (dockerhost.Info, error)
	EnsureImage(ctx context.Context, ref string) error
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) (string, error)
	RemoveNetwork(ctx context.Context, name string) error
	ListNetworks(ctx context.Context, labels map[string]string) ([]dockerhost.Network, error)
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	ListVolumes(ctx context.Context, labels map[string]string) ([]string, error)
	CreateContainer(ctx context.Context, spec dockerhost.ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string) error
	InspectContainer(ctx context.Context, idOrName string) (dockerhost.Container, error)
	ListContainers(ctx context.Context, labels map[string]string) ([]dockerhost.Container, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
	Exec(ctx context.Context, id string, e dockerhost.ExecSpec) (dockerhost.ExecResult, error)
}

// Options configure a Backend.
type Options struct {
	Config config.MSPConfig
	// EdgeListen is the mocker's edge address; it tells this mocker's Docker
	// objects from another mocker's on the same Docker host.
	EdgeListen string
	Docker     Docker
	Clock      clock.Clock
	Log        *slog.Logger
	// CA signs the clusters' server certificates.
	CA *localca.CA
	// pollInterval is how often readiness is polled (default 500ms).
	pollInterval time.Duration
}

// Backend is the docker backend. It is safe for concurrent use; the domain
// serializes calls per cluster and the backend keeps no per-cluster state:
// everything is read back from Docker (labels, inspect).
type Backend struct {
	cfg     config.MSPConfig
	owner   string
	tag     string // dockerhost.OwnerTag(owner), in every object name
	docker  Docker
	clk     clock.Clock
	log     *slog.Logger
	ca      *localca.CA
	poll    time.Duration
	secret  []byte // derives role passwords; lives as long as the process
	backups string // name of the backups volume
}

var _ domain.Backend = (*Backend)(nil)

// New checks that Docker answers, removes leftovers of an earlier mocker and
// returns the backend.
func New(ctx context.Context, o Options) (*Backend, error) {
	if o.Docker == nil || o.Clock == nil || o.Log == nil || o.CA == nil {
		return nil, errors.New("msp postgresql docker backend: Docker, Clock, Log and CA are required")
	}
	if len(o.Config.PostgresImages) == 0 {
		return nil, errors.New("msp postgresql docker backend: no PostgreSQL images configured (NEBIUS_MOCKER_MSP_POSTGRESQL_IMAGES)")
	}
	if err := o.Docker.Ping(ctx); err != nil {
		return nil, fmt.Errorf("msp postgresql docker backend (NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND=docker) needs Docker, but %s does not answer "+
			"(set NEBIUS_MOCKER_MSP_DOCKER_HOST or DOCKER_HOST, or use NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND=mock): %w", o.Docker.Host(), err)
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails (Go 1.24+)
	owner := o.EdgeListen
	if _, port, err := net.SplitHostPort(owner); err != nil || port == "" || port == "0" {
		// Without a port of its own the edge address does not tell mockers
		// apart: use a random owner, so two never share objects.
		owner = "random-" + hex.EncodeToString(secret[:8])
		o.Log.InfoContext(ctx, "msp postgresql: the edge address has no port; using a random owner", "owner", owner)
	}
	b := &Backend{
		cfg: o.Config, owner: owner, tag: dockerhost.OwnerTag(owner), docker: o.Docker, clk: o.Clock, log: o.Log, ca: o.CA,
		poll: o.pollInterval, secret: secret, backups: "nmock-pg-backups-" + dockerhost.OwnerTag(owner),
	}
	if b.poll <= 0 {
		b.poll = 500 * time.Millisecond
	}
	if err := b.reap(ctx); err != nil {
		return nil, fmt.Errorf("msp postgresql docker backend: remove leftovers: %w", err)
	}
	return b, nil
}

// Name implements domain.Backend.
func (*Backend) Name() string { return Name }

// Close implements domain.Backend: it removes everything this mocker created.
func (b *Backend) Close(ctx context.Context) error { return b.reap(ctx) }

func (b *Backend) baseLabels() map[string]string {
	return map[string]string{LabelBackend: backendLabel, LabelOwner: b.owner}
}

func (b *Backend) clusterLabels(clusterID string) map[string]string {
	l := b.baseLabels()
	l[LabelCluster] = clusterID
	return l
}

func (b *Backend) reap(ctx context.Context) error { return b.removeLabeled(ctx, b.baseLabels()) }

// removeLabeled removes the containers matching labels, then the matching
// volumes and networks.
func (b *Backend) removeLabeled(ctx context.Context, labels map[string]string) error {
	cs, err := b.docker.ListContainers(ctx, labels)
	if err != nil {
		return internal("list containers", err)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, c := range cs {
		g.Go(func() error {
			if err := b.docker.RemoveContainer(gctx, c.ID); err != nil {
				return fmt.Errorf("remove container %s: %w", c.Name, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return internal("remove containers", err)
	}
	vols, err := b.docker.ListVolumes(ctx, labels)
	if err != nil {
		return internal("list volumes", err)
	}
	for _, v := range vols {
		if err := b.docker.RemoveVolume(ctx, v); err != nil {
			return internal("remove volume "+v, err)
		}
	}
	nets, err := b.docker.ListNetworks(ctx, labels)
	if err != nil {
		return internal("list networks", err)
	}
	for _, n := range nets {
		if err := b.docker.RemoveNetwork(ctx, n.Name); err != nil {
			return internal("remove network "+n.Name, err)
		}
	}
	if len(cs) > 0 {
		b.log.InfoContext(ctx, "msp postgresql: removed containers", "count", len(cs), "labels", labels)
	}
	return nil
}

// internal wraps a Docker failure as errs.Internal, keeping a canceled
// context a plain context error so the operation reports the cancellation.
func internal(what string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", what, err)
	}
	if _, ok := errors.AsType[*errs.Error](err); ok {
		return err
	}
	return errs.Internal(fmt.Sprintf("%s: %v", what, err))
}

// sleep waits d on the backend clock, or until ctx is done.
func (b *Backend) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-b.clk.After(d):
		return nil
	}
}

// mac derives a stable secret (a role password) for what from the backend's
// per-process secret.
func (b *Backend) mac(what string) string {
	h := sha256.New()
	h.Write(b.secret)
	h.Write([]byte{0})
	h.Write([]byte(what))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Owner is what this backend's Docker objects carry in nebius-mocker.owner
// (the edge address, or a random one when it has no port).
func (b *Backend) Owner() string { return b.owner }
