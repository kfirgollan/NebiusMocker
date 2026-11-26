// Package docker is the Managed MLflow backend that runs a real MLflow
// tracking server (NEBIUS_MOCKER_MSP_MLFLOW_BACKEND=docker): one container per
// cluster on the local Docker host, through the Engine API.
//
//   - Image: the official MLflow image plus boto3 and the auth extras, built
//     once on first use as <MLflowImage>-nebius-mocker (about a minute).
//   - The server runs the basic-auth app with HTTPS (certificate from the
//     mocker's local CA, so clients trust <NEBIUS_MOCKER_MSP_TLS_DIR>/ca.pem),
//     a SQLite backend store and the auth database in the cluster's volume,
//     and proxies artifacts to the cluster's bucket through the mocker's S3
//     front (--serve-artifacts).
//   - The admin user comes from the first EnsureCluster (MLflow creates it
//     when the auth database is first created); the password is kept in
//     memory for readiness checks and ExperimentsCount.
//   - Endpoint: "<EndpointHost>:<port>" of the container's 5000/tcp,
//     published on NEBIUS_MOCKER_MSP_BIND_IP. The port is chosen once and
//     pinned (label nebius-mocker.port), so it survives stop/start and
//     recreation.
//   - Every container and volume carries nebius-mocker.backend=msp-mlflow,
//     nebius-mocker.owner=<edge address> and the cluster ID; startup and Close
//     remove this mocker's. A changed spec hash (image, resources, artifact
//     store) recreates the container and keeps the volume.
package docker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

// Name is the NEBIUS_MOCKER_MSP_MLFLOW_BACKEND value of this backend.
const Name = "docker"

// Labels on every Docker object the backend creates.
const (
	LabelBackend = "nebius-mocker.backend"
	LabelOwner   = "nebius-mocker.owner"
	LabelCluster = "nebius-mocker.cluster"
	labelSpec    = "nebius-mocker.spec"
	labelPort    = "nebius-mocker.port"
)

const (
	backendLabel = "msp-mlflow"
	serverPort   = "5000/tcp"
	stopTimeout  = 15 * time.Second
	logTail      = 25
)

// Docker is the part of the Docker Engine API the backend uses
// (*dockerhost.Client; dockerfake.Fake in tests).
type Docker interface {
	Host() string
	Ping(ctx context.Context) error
	Info(ctx context.Context) (dockerhost.Info, error)
	EnsureImage(ctx context.Context, ref string) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	BuildImage(ctx context.Context, tag, dockerfile string, labels map[string]string) error
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
}

// Options configure a Backend.
type Options struct {
	Config config.MSPConfig
	// EdgeListen is the mocker's edge address; it tells this mocker's Docker
	// objects from another mocker's on the same Docker host.
	EdgeListen string
	// S3Listen is the mocker's S3 front address (NEBIUS_MOCKER_STORAGE_S3_LISTEN),
	// used when Config.S3Endpoint is empty.
	S3Listen string
	Docker   Docker
	Clock    clock.Clock
	Log      *slog.Logger
	// CA signs the servers' certificates and is what readiness checks trust.
	CA *localca.CA
	// pollInterval is how often readiness is polled (default 500ms).
	pollInterval time.Duration
	// httpClient replaces the TLS client of readiness checks (tests).
	httpClient *http.Client
}

// Backend is the docker backend. It is safe for concurrent use.
type Backend struct {
	cfg      config.MSPConfig
	owner    string
	s3       string // endpoint the servers reach the S3 front at
	docker   Docker
	clk      clock.Clock
	log      *slog.Logger
	ca       *localca.CA
	poll     time.Duration
	http     *http.Client
	secret   []byte
	image    string
	buildMu  sync.Mutex
	adminsMu sync.Mutex
	admins   map[string]credentials // by cluster ID
}

type credentials struct{ user, password string }

var _ domain.Backend = (*Backend)(nil)

// New checks that Docker answers, removes leftovers of an earlier mocker and
// returns the backend. The server image is built on the first EnsureCluster.
func New(ctx context.Context, o Options) (*Backend, error) {
	if o.Docker == nil || o.Clock == nil || o.Log == nil || o.CA == nil {
		return nil, errors.New("msp mlflow docker backend: Docker, Clock, Log and CA are required")
	}
	s3, err := s3Endpoint(o.Config, o.S3Listen)
	if err != nil {
		return nil, err
	}
	if err := o.Docker.Ping(ctx); err != nil {
		return nil, fmt.Errorf("msp mlflow docker backend (NEBIUS_MOCKER_MSP_MLFLOW_BACKEND=docker) needs Docker, but %s does not answer "+
			"(set NEBIUS_MOCKER_MSP_DOCKER_HOST or DOCKER_HOST, or use NEBIUS_MOCKER_MSP_MLFLOW_BACKEND=mock): %w", o.Docker.Host(), err)
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails (Go 1.24+)
	b := &Backend{
		cfg: o.Config, owner: ownerOf(o.EdgeListen), s3: s3, docker: o.Docker, clk: o.Clock, log: o.Log, ca: o.CA,
		poll: o.pollInterval, secret: secret, image: o.Config.MLflowImage + "-nebius-mocker", admins: map[string]credentials{},
	}
	b.http = o.httpClient
	if b.http == nil {
		b.http = &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: o.CA.Pool(), MinVersion: tls.VersionTLS12}, DisableKeepAlives: true},
		}
	}
	if b.poll <= 0 {
		b.poll = 500 * time.Millisecond
	}
	if err := b.reap(ctx); err != nil {
		return nil, fmt.Errorf("msp mlflow docker backend: remove leftovers: %w", err)
	}
	return b, nil
}

// Name implements domain.Backend.
func (*Backend) Name() string { return Name }

// Close implements domain.Backend: it removes everything this mocker created.
func (b *Backend) Close(ctx context.Context) error {
	b.adminsMu.Lock()
	b.admins = map[string]credentials{}
	b.adminsMu.Unlock()
	return b.reap(ctx)
}

func (b *Backend) baseLabels() map[string]string {
	return map[string]string{LabelBackend: backendLabel, LabelOwner: b.owner}
}

func (b *Backend) clusterLabels(id string) map[string]string {
	l := b.baseLabels()
	l[LabelCluster] = id
	return l
}

func (b *Backend) reap(ctx context.Context) error { return b.removeLabeled(ctx, b.baseLabels()) }

// removeLabeled removes the containers matching labels, then the volumes.
func (b *Backend) removeLabeled(ctx context.Context, labels map[string]string) error {
	cs, err := b.docker.ListContainers(ctx, labels)
	if err != nil {
		return internal("list containers", err)
	}
	for _, c := range cs {
		if err := b.docker.RemoveContainer(ctx, c.ID); err != nil {
			return internal("remove container "+c.Name, err)
		}
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
	if len(cs) > 0 {
		b.log.InfoContext(ctx, "msp mlflow: removed containers", "count", len(cs), "labels", labels)
	}
	return nil
}

// internal wraps a failure as errs.Internal, keeping context errors and
// errs kinds as they are.
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

// mac derives a stable secret for what from the per-process secret.
func (b *Backend) mac(what string) string {
	h := sha256.New()
	h.Write(b.secret)
	h.Write([]byte{0})
	h.Write([]byte(what))
	return hex.EncodeToString(h.Sum(nil))
}

// ownerOf is the owner label of this backend's objects: the mocker's edge
// address, whose port tells live owners from gone ones (reaping). An address
// without a fixed port cannot tell two mockers apart, so it gets a random
// owner instead.
func ownerOf(edge string) string {
	if _, port, err := net.SplitHostPort(edge); err == nil && port != "" && port != "0" {
		return edge
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return "unbound-" + hex.EncodeToString(b)
}

// Owner is the nebius-mocker.owner label value of this backend's objects.
func (b *Backend) Owner() string { return b.owner }
