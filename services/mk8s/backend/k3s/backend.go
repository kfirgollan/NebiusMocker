// Package k3s is the mk8s backend that runs real Kubernetes
// (NEBIUS_MOCKER_MK8S_BACKEND=k3s), the way LocalStack runs EKS: one
// rancher/k3s server container per cluster and one k3s agent container per
// node of every node group, on the local Docker host through the Engine API.
//
//   - The API server is published on NEBIUS_MOCKER_MK8S_K3S_BIND_IP; its
//     certificate covers NEBIUS_MOCKER_MK8S_K3S_ENDPOINT_HOST, the host of the
//     cluster's endpoints. The cluster CA is the real k3s server CA.
//   - Nebius IAM tokens authenticate through the mocker's TokenReview webhook
//     (services/iam), which puts identities in the documented groups
//     nebius:viewer, nebius:editor and nebius:admin; ClusterRoleBindings
//     installed with the server give those groups view, edit and
//     cluster-admin.
//   - Agents are admitted while the Docker host has room (CPU, free memory,
//     NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS); a node that does not fit is
//     reported not ready with the reason.
//   - Every container, network and volume is labeled
//     nebius-mocker.backend=k3s plus its cluster, node group and node. They
//     are removed with their cluster or node, on Close, and at startup
//     (leftovers of a mocker that was killed).
package k3s

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
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
	RemoveContainer(ctx context.Context, id string) error
	InspectContainer(ctx context.Context, idOrName string) (dockerhost.Container, error)
	ListContainers(ctx context.Context, labels map[string]string) ([]dockerhost.Container, error)
	ReadFile(ctx context.Context, id, path string) ([]byte, error)
	MemoryUsage(ctx context.Context, id string) (int64, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
}

// Options configure a Backend.
type Options struct {
	Config config.MK8sK3sConfig
	// Versions is the control-plane catalog (pkg/config MK8sConfig.Versions);
	// each needs an image in Config.Images.
	Versions []string
	// EdgeListen and EdgeTLS locate the mocker's edge, where the TokenReview
	// webhook is served, when Config.WebhookURL is empty.
	EdgeListen string
	EdgeTLS    bool
	Docker     Docker
	Clock      clock.Clock
	Log        *slog.Logger
	// newKube builds the Kubernetes client of a cluster (tests inject a fake).
	newKube func(base string, kc adminKubeconfig) (kubeAPI, error)
	// pollInterval is how often readiness is polled (default 1s).
	pollInterval time.Duration
}

// Backend is the k3s backend. It is safe for concurrent use; the domain
// serializes calls per cluster.
type Backend struct {
	cfg        config.MK8sK3sConfig
	versions   map[string]string // "<major>.<minor>" → image
	webhookURL string
	docker     Docker
	clk        clock.Clock
	log        *slog.Logger
	newKube    func(base string, kc adminKubeconfig) (kubeAPI, error)
	poll       time.Duration
	// secret derives join tokens and node passwords (names.go).
	secret []byte

	mu       sync.Mutex
	clusters map[string]*clusterState
	// admission is a one-slot semaphore around agent admission (nodes.go).
	admission chan struct{}
}

var _ domain.Backend = (*Backend)(nil)

// clusterState is what the backend knows of a running cluster.
type clusterState struct {
	serverID string
	endpoint domain.ClusterEndpoint
	kube     kubeAPI
	image    string
}

// New checks that Docker answers, removes leftovers of an earlier mocker
// and returns the backend. Without a reachable Docker daemon it fails,
// naming the setting, so NEBIUS_MOCKER_MK8S_BACKEND=k3s never runs
// half-working.
func New(ctx context.Context, o Options) (*Backend, error) {
	if o.Docker == nil || o.Clock == nil || o.Log == nil {
		return nil, errors.New("mk8s k3s backend: Docker, Clock and Log are required")
	}
	versions := map[string]string{}
	for _, v := range o.Versions {
		img, ok := o.Config.Images[v]
		if !ok {
			return nil, fmt.Errorf("mk8s k3s backend: no k3s image for control-plane version %s (set NEBIUS_MOCKER_MK8S_K3S_IMAGES=%s=rancher/k3s:<tag>)", v, v)
		}
		versions[v] = img
	}
	hook, err := webhookURL(o)
	if err != nil {
		return nil, err
	}
	if err := o.Docker.Ping(ctx); err != nil {
		return nil, fmt.Errorf("mk8s k3s backend (NEBIUS_MOCKER_MK8S_BACKEND=k3s) needs Docker, but %s does not answer "+
			"(set NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST or DOCKER_HOST, or use NEBIUS_MOCKER_MK8S_BACKEND=mock): %w", o.Docker.Host(), err)
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails (Go 1.24+)
	b := &Backend{
		cfg: o.Config, versions: versions, webhookURL: hook, docker: o.Docker, clk: o.Clock, log: o.Log,
		newKube: o.newKube, poll: o.pollInterval, secret: secret, clusters: map[string]*clusterState{},
		admission: make(chan struct{}, 1),
	}
	if b.newKube == nil {
		b.newKube = func(base string, kc adminKubeconfig) (kubeAPI, error) { return newRestKube(base, kc) }
	}
	if b.poll <= 0 {
		b.poll = time.Second
	}
	if err := b.reap(ctx); err != nil {
		return nil, fmt.Errorf("mk8s k3s backend: remove leftovers: %w", err)
	}
	return b, nil
}

// webhookURL is the TokenReview webhook base the API servers call.
func webhookURL(o Options) (string, error) {
	if o.Config.WebhookURL != "" {
		return o.Config.WebhookURL, nil
	}
	_, port, err := net.SplitHostPort(o.EdgeListen)
	if err != nil || port == "" || port == "0" {
		return "", fmt.Errorf("mk8s k3s backend: cannot derive the webhook URL from edge address %q; set NEBIUS_MOCKER_MK8S_K3S_WEBHOOK_URL", o.EdgeListen)
	}
	scheme := "http"
	if o.EdgeTLS {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort("host.docker.internal", port)}).String(), nil
}

// Name implements domain.Backend.
func (*Backend) Name() string { return backendName }

// CheckVersion implements domain.Backend: the version needs a k3s image.
func (b *Backend) CheckVersion(version string) error {
	if _, ok := b.versions[version]; ok {
		return nil
	}
	return errs.BadRequest(errs.Violation{
		Field:   "spec.control_plane.version",
		Message: fmt.Sprintf("the k3s backend has no image for Kubernetes %s (NEBIUS_MOCKER_MK8S_K3S_IMAGES)", version),
	})
}

// Close implements domain.Backend: it removes every cluster's containers.
func (b *Backend) Close(ctx context.Context) error {
	b.mu.Lock()
	b.clusters = map[string]*clusterState{}
	b.mu.Unlock()
	return b.reap(ctx)
}

// reap removes everything this mocker's backend created (LabelOwner):
// containers first, then the volumes and networks they used.
func (b *Backend) reap(ctx context.Context) error {
	return b.removeLabeled(ctx, map[string]string{LabelBackend: backendName, LabelOwner: b.webhookURL})
}

// removeLabeled removes the containers matching labels, then the matching
// volumes and networks.
func (b *Backend) removeLabeled(ctx context.Context, labels map[string]string) error {
	cs, err := b.docker.ListContainers(ctx, labels)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
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
		return err
	}
	vols, err := b.docker.ListVolumes(ctx, labels)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range vols {
		if err := b.docker.RemoveVolume(ctx, v); err != nil {
			return fmt.Errorf("remove volume %s: %w", v, err)
		}
	}
	nets, err := b.docker.ListNetworks(ctx, labels)
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	for _, n := range nets {
		if err := b.docker.RemoveNetwork(ctx, n.Name); err != nil {
			return fmt.Errorf("remove network %s: %w", n.Name, err)
		}
	}
	if len(cs) > 0 {
		b.log.InfoContext(ctx, "mk8s k3s: removed containers", "count", len(cs), "labels", labels)
	}
	return nil
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

// adminHost is where the mocker reaches published API server ports.
func (b *Backend) adminHost() string {
	if ip := net.ParseIP(b.cfg.BindIP); ip == nil || ip.IsUnspecified() {
		return "127.0.0.1"
	}
	return b.cfg.BindIP
}

// hostPort returns the published host port of the server's API port.
func hostPort(c dockerhost.Container) (string, error) {
	for _, pb := range c.Ports[apiPort] {
		if _, err := strconv.Atoi(pb.HostPort); err == nil {
			return pb.HostPort, nil
		}
	}
	return "", fmt.Errorf("container %s publishes no %s port", c.Name, apiPort)
}
