package k3s

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// Paths inside the server container.
const (
	adminKubeconfigPath = "/etc/rancher/k3s/k3s.yaml"
	webhookConfigPath   = "/etc/nebius-mocker/token-webhook.yaml"
	// rbacManifestPath is in k3s's auto-deploy directory: the server applies
	// it at startup.
	rbacManifestPath = "/var/lib/rancher/k3s/server/manifests/nebius-mocker-rbac.yaml"
	dataDir          = "/var/lib/rancher/k3s"
)

// defaultServiceBase is the k3s default service network; a service CIDR
// given only as a prefix length ("/16") is taken from it.
var defaultServiceBase = netip.MustParseAddr("10.43.0.0")

// rbacManifest binds the groups the TokenReview webhook returns (the groups
// Nebius documents for users with Nebius credentials) to the built-in
// Kubernetes roles.
const rbacManifest = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nebius:viewer
  labels: {app.kubernetes.io/managed-by: nebius-mocker}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view}
subjects:
- {apiGroup: rbac.authorization.k8s.io, kind: Group, name: "nebius:viewer"}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nebius:editor
  labels: {app.kubernetes.io/managed-by: nebius-mocker}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: edit}
subjects:
- {apiGroup: rbac.authorization.k8s.io, kind: Group, name: "nebius:editor"}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nebius:admin
  labels: {app.kubernetes.io/managed-by: nebius-mocker}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}
subjects:
- {apiGroup: rbac.authorization.k8s.io, kind: Group, name: "nebius:admin"}
---
# view and edit are namespaced roles: they do not cover Nodes, which every
# cluster user reads (kubectl get nodes).
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: nebius:node-reader
  labels: {app.kubernetes.io/managed-by: nebius-mocker}
rules:
- {apiGroups: [""], resources: [nodes], verbs: [get, list, watch]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nebius:node-reader
  labels: {app.kubernetes.io/managed-by: nebius-mocker}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: "nebius:node-reader"}
subjects:
- {apiGroup: rbac.authorization.k8s.io, kind: Group, name: "nebius:viewer"}
- {apiGroup: rbac.authorization.k8s.io, kind: Group, name: "nebius:editor"}
`

// webhookConfig is the kubeconfig the API server calls the TokenReview
// webhook with. The webhook is the mocker's own edge; with a TLS edge the
// certificate is the user's (issued for the API names, not for
// host.docker.internal), so it is not verified on this internal hop.
func webhookConfig(base, clusterID string) string {
	server := base + "/mk8s/v1/clusters/" + clusterID + "/tokenreview"
	insecure := ""
	if strings.HasPrefix(base, "https://") {
		insecure = "\n    insecure-skip-tls-verify: true"
	}
	return `apiVersion: v1
kind: Config
clusters:
- name: nebius-mocker
  cluster:
    server: ` + server + insecure + `
users:
- name: kube-apiserver
  user: {}
contexts:
- name: webhook
  context: {cluster: nebius-mocker, user: kube-apiserver}
current-context: webhook
`
}

// EnsureCluster implements domain.Backend. It starts the cluster's k3s
// server (or replaces it with the image of a new version, keeping its data
// volume and port) and returns once the API server is ready.
func (b *Backend) EnsureCluster(ctx context.Context, c domain.BackendCluster) (domain.ClusterEndpoint, error) {
	image, ok := b.versions[c.Version]
	if !ok {
		return domain.ClusterEndpoint{}, b.CheckVersion(c.Version)
	}
	if len(c.AllowedCIDRs) > 0 {
		// Every client of a local API server comes through the Docker host's
		// published port, so a source restriction cannot be applied.
		return domain.ClusterEndpoint{}, errs.BadRequest(errs.Violation{
			Field:   "spec.control_plane.endpoints.public_endpoint.allowed_cidrs",
			Message: "the k3s backend cannot restrict API server clients by CIDR; leave allowed_cidrs empty",
		})
	}
	serviceCIDR, err := resolveServiceCIDR(c.ServiceCIDR)
	if err != nil {
		return domain.ClusterEndpoint{}, err
	}
	if st, ok := b.cluster(c.ID); ok && st.image == image {
		if _, err := b.docker.InspectContainer(ctx, st.serverID); err == nil {
			return forCluster(st.endpoint, c), nil
		}
	}
	port := ""
	existing, err := b.docker.InspectContainer(ctx, serverName(c.ID))
	switch {
	case err == nil && existing.Running && existing.Labels[labelSpec] == image:
		return b.attach(ctx, c, existing, image)
	case err == nil:
		// A new version or a stopped server: replace the container, keep
		// the data volume and the published port.
		port, _ = hostPort(existing)
		if err := b.docker.RemoveContainer(ctx, existing.ID); err != nil {
			return domain.ClusterEndpoint{}, fmt.Errorf("replace k3s server of %s: %w", c.ID, err)
		}
	case !errors.Is(err, dockerhost.ErrNotFound):
		return domain.ClusterEndpoint{}, fmt.Errorf("inspect k3s server of %s: %w", c.ID, err)
	}
	id, err := b.startServer(ctx, c, image, serviceCIDR, port)
	if err != nil {
		return domain.ClusterEndpoint{}, err
	}
	ctr, err := b.docker.InspectContainer(ctx, id)
	if err != nil {
		return domain.ClusterEndpoint{}, fmt.Errorf("inspect k3s server of %s: %w", c.ID, err)
	}
	return b.attach(ctx, c, ctr, image)
}

func (b *Backend) startServer(ctx context.Context, c domain.BackendCluster, image, serviceCIDR, port string) (string, error) {
	if err := b.docker.EnsureImage(ctx, image); err != nil {
		return "", fmt.Errorf("pull %s: %w", image, err)
	}
	if _, err := b.docker.EnsureNetwork(ctx, networkName(c.ID), b.clusterLabels(c.ID, "")); err != nil {
		return "", fmt.Errorf("network of %s: %w", c.ID, err)
	}
	if err := b.docker.EnsureVolume(ctx, volumeName(c.ID), b.clusterLabels(c.ID, roleServer)); err != nil {
		return "", fmt.Errorf("volume of %s: %w", c.ID, err)
	}
	sans := []string{"127.0.0.1", "localhost", serverName(c.ID)}
	if !slices.Contains(sans, b.cfg.EndpointHost) {
		sans = append(sans, b.cfg.EndpointHost)
	}
	cmd := []string{
		"server",
		"--disable-agent", // like Nebius: the control plane is not one of the cluster's nodes
		"--disable=traefik",
		"--service-cidr=" + serviceCIDR,
		"--kube-apiserver-arg=authentication-token-webhook-config-file=" + webhookConfigPath,
		"--kube-apiserver-arg=authentication-token-webhook-version=v1",
		"--kube-apiserver-arg=authentication-token-webhook-cache-ttl=10s",
	}
	for _, s := range sans {
		cmd = append(cmd, "--tls-san="+s)
	}
	spec := dockerhost.ContainerSpec{
		Name:       serverName(c.ID),
		Image:      image,
		Cmd:        cmd,
		Env:        []string{"K3S_TOKEN=" + joinToken(b.secret, c.ID)},
		Labels:     b.clusterLabels(c.ID, roleServer),
		Privileged: true,
		Tmpfs:      map[string]string{"/run": "", "/var/run": ""},
		Network:    networkName(c.ID),
		Aliases:    []string{serverName(c.ID)},
		Ports:      []dockerhost.PortBinding{{ContainerPort: apiPort, HostIP: b.cfg.BindIP, HostPort: port}},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		Binds:      []string{volumeName(c.ID) + ":" + dataDir},
		Files: []dockerhost.File{
			{Path: webhookConfigPath, Mode: 0o600, Content: []byte(webhookConfig(b.webhookURL, c.ID))},
			{Path: rbacManifestPath, Mode: 0o600, Content: []byte(rbacManifest)},
		},
	}
	spec.Labels[labelSpec] = image
	id, err := b.docker.CreateContainer(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create k3s server of %s: %w", c.ID, err)
	}
	if err := b.docker.StartContainer(ctx, id); err != nil {
		return "", fmt.Errorf("start k3s server of %s: %w", c.ID, err)
	}
	b.log.InfoContext(ctx, "mk8s k3s: server started", "cluster", c.ID, "image", image, "container", spec.Name)
	return id, nil
}

// attach waits for the server's API, reads its admin credentials and CA, and
// records the cluster.
func (b *Backend) attach(ctx context.Context, c domain.BackendCluster, ctr dockerhost.Container, image string) (domain.ClusterEndpoint, error) {
	port, err := hostPort(ctr)
	if err != nil {
		return domain.ClusterEndpoint{}, err
	}
	kube, kc, err := b.waitServer(ctx, c.ID, ctr.ID, "https://"+net.JoinHostPort(b.adminHost(), port))
	if err != nil {
		return domain.ClusterEndpoint{}, err
	}
	version, err := kube.Version(ctx)
	if err != nil {
		return domain.ClusterEndpoint{}, fmt.Errorf("version of %s: %w", c.ID, err)
	}
	url := "https://" + net.JoinHostPort(b.cfg.EndpointHost, port)
	ep := domain.ClusterEndpoint{
		// One network: the private endpoint is the same local address.
		PublicEndpoint: url, PrivateEndpoint: url, CACertificate: string(kc.CA), KubernetesVersion: version,
	}
	b.mu.Lock()
	b.clusters[c.ID] = &clusterState{serverID: ctr.ID, endpoint: ep, kube: kube, image: image}
	b.mu.Unlock()
	return forCluster(ep, c), nil
}

// waitServer polls until the API server answers /readyz with the admin
// credentials, failing early, with the server's last log lines, if the
// container exits.
func (b *Backend) waitServer(ctx context.Context, clusterID, containerID, base string) (kubeAPI, adminKubeconfig, error) {
	deadline := b.clk.Now().Add(b.cfg.StartTimeout)
	var last error
	for {
		ctr, err := b.docker.InspectContainer(ctx, containerID)
		if err != nil {
			return nil, adminKubeconfig{}, fmt.Errorf("inspect k3s server of %s: %w", clusterID, err)
		}
		if !ctr.Running {
			return nil, adminKubeconfig{}, b.startFailure(ctx, clusterID, containerID, fmt.Sprintf("the k3s server exited (code %d)", ctr.ExitCode))
		}
		kube, kc, err := b.tryServer(ctx, containerID, base)
		if err == nil {
			return kube, kc, nil
		}
		last = err
		if !b.clk.Now().Before(deadline) {
			return nil, adminKubeconfig{}, b.startFailure(ctx, clusterID, containerID,
				fmt.Sprintf("the k3s API server was not ready within %s (NEBIUS_MOCKER_MK8S_K3S_START_TIMEOUT): %v", b.cfg.StartTimeout, last))
		}
		if err := b.sleep(ctx, b.poll); err != nil {
			return nil, adminKubeconfig{}, err
		}
	}
}

func (b *Backend) tryServer(ctx context.Context, containerID, base string) (kubeAPI, adminKubeconfig, error) {
	raw, err := b.docker.ReadFile(ctx, containerID, adminKubeconfigPath)
	if err != nil {
		return nil, adminKubeconfig{}, fmt.Errorf("read %s: %w", adminKubeconfigPath, err)
	}
	kc, err := parseAdminKubeconfig(raw)
	if err != nil {
		return nil, adminKubeconfig{}, err
	}
	kube, err := b.newKube(base, kc)
	if err != nil {
		return nil, adminKubeconfig{}, err
	}
	if err := kube.Ready(ctx); err != nil {
		return nil, adminKubeconfig{}, err
	}
	return kube, kc, nil
}

// startFailure is the error a cluster that cannot start fails with: a
// BadState naming the cause and the server's last log lines, so the
// operation's status says what went wrong.
func (b *Backend) startFailure(ctx context.Context, clusterID, containerID, cause string) error {
	logs, err := b.docker.Logs(ctx, containerID, 5)
	if err != nil {
		logs = "(logs unavailable: " + err.Error() + ")"
	}
	b.log.WarnContext(ctx, "mk8s k3s: server failed", "cluster", clusterID, "cause", cause, "logs", logs)
	return errs.BadState(clusterID, "control plane did not start: "+cause+"; last k3s server log lines: "+strings.TrimSpace(logs))
}

// DeleteCluster implements domain.Backend: it removes every container,
// volume and network of the cluster.
func (b *Backend) DeleteCluster(ctx context.Context, clusterID string) error {
	b.mu.Lock()
	delete(b.clusters, clusterID)
	b.mu.Unlock()
	if err := b.removeLabeled(ctx, map[string]string{LabelBackend: backendName, LabelCluster: clusterID}); err != nil {
		return fmt.Errorf("delete k3s cluster %s: %w", clusterID, err)
	}
	return nil
}

func (b *Backend) cluster(id string) (*clusterState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.clusters[id]
	return st, ok
}

// forCluster drops the public endpoint when c does not ask for one.
func forCluster(ep domain.ClusterEndpoint, c domain.BackendCluster) domain.ClusterEndpoint {
	if !c.PublicEndpoint {
		ep.PublicEndpoint = ""
	}
	return ep
}

// resolveServiceCIDR turns spec.kube_network.service_cidrs[0] into the k3s
// --service-cidr: a CIDR as is, "/N" carved from the k3s default network,
// "" the k3s default /16.
func resolveServiceCIDR(s string) (string, error) {
	const field = "spec.kube_network.service_cidrs"
	bits := 16
	if n, ok := strings.CutPrefix(s, "/"); ok {
		v, err := strconv.Atoi(n)
		if err != nil {
			return "", errs.BadRequest(errs.Violation{Field: field, Message: "invalid prefix length " + s})
		}
		bits = v
	} else if s != "" {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() {
			return "", errs.BadRequest(errs.Violation{Field: field, Message: "invalid IPv4 CIDR " + s})
		}
		return p.Masked().String(), nil
	}
	p, err := defaultServiceBase.Prefix(bits)
	if err != nil || bits < 12 || bits > 24 {
		return "", errs.BadRequest(errs.Violation{Field: field, Message: "the service network prefix must be between /12 and /24"})
	}
	return p.String(), nil
}
