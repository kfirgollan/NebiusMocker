// Package domain holds the Nebius Managed Kubernetes (mk8s) control-plane
// logic: clusters, node groups and their nodes (compute instances created
// through pkg/peer), the version catalog and preflight checks. Kubernetes
// itself runs in a Backend (the ports in this file).
package domain

import "context"

// Backend is the Kubernetes data plane behind mk8s clusters and node groups,
// selected with NEBIUS_MOCKER_MK8S_BACKEND. The control plane is the
// domain's: resources, validation, operations, and the node group's nodes as
// compute instances. A backend only runs Kubernetes (k3s), or reports that
// it does (mock).
//
// The domain calls a backend from operation Work, never inside a store
// transaction, and serializes calls per cluster (one operation at a time on
// a cluster or on one of its node groups). Every method is idempotent and
// reconciling: it may be called again with the same input (a retried
// operation, an update that changed nothing the backend sees). Methods may
// block while containers start, and must return when ctx is canceled (the
// mocker is stopping). Errors are pkg/errs kinds; they fail the operation
// with that kind.
type Backend interface {
	// Name is the NEBIUS_MOCKER_MK8S_BACKEND value ("mock", "k3s").
	Name() string

	// CheckVersion reports whether the backend can run Kubernetes
	// "<major>.<minor>" (a version of the control-plane catalog). The domain
	// calls it while validating a request, so an unsupported version fails
	// the RPC: return an errs.BadRequest naming the version.
	CheckVersion(version string) error

	// EnsureCluster creates the cluster's control plane, or reconciles an
	// existing one to c (a version upgrade, endpoint changes), and returns
	// once its API server is ready.
	EnsureCluster(ctx context.Context, c BackendCluster) (ClusterEndpoint, error)

	// DeleteCluster removes the control plane and whatever is left of its
	// nodes. A cluster the backend does not know is not an error. The domain
	// empties every node group (EnsureNodes with no nodes) first.
	DeleteCluster(ctx context.Context, clusterID string) error

	// EnsureNodes makes the node group's Kubernetes nodes exactly g.Nodes,
	// removing the group's other nodes, and returns the readiness of each
	// node of g.Nodes, in that order. A node the backend cannot run (host
	// capacity, NEBIUS_MOCKER_MK8S_MAX_AGENTS) is reported not ready with a
	// reason instead of failing the call: the node group then shows
	// node_count > ready_node_count, honestly.
	EnsureNodes(ctx context.Context, g BackendNodeGroup) ([]NodeReadiness, error)

	// Close releases what the backend holds at mocker shutdown.
	Close(ctx context.Context) error
}

// BackendCluster is the desired control plane of one cluster.
type BackendCluster struct {
	// ID is the cluster NID (mk8scluster-…); backends key their state by it.
	ID        string
	ProjectID string
	Name      string
	// Version is the Kubernetes version "<major>.<minor>", e.g. "1.35".
	Version string
	// ServiceCIDR is spec.kube_network.service_cidrs[0] as the user sent it:
	// a CIDR ("10.96.0.0/16"), a prefix length ("/16") or "" (the default /16).
	ServiceCIDR string
	// PublicEndpoint asks for an endpoint reachable from outside the VPC
	// (spec.control_plane.endpoints.public_endpoint set).
	PublicEndpoint bool
	// AllowedCIDRs restricts the public endpoint (empty: unrestricted).
	AllowedCIDRs []string
	// EtcdClusterSize is the effective etcd size (1 or 3).
	EtcdClusterSize int64
}

// ClusterEndpoint is how to reach a ready control plane. Endpoints are
// Kubernetes API server URLs, "https://host[:port]": `nebius mk8s cluster
// get-credentials` writes status.control_plane.endpoints.public_endpoint
// (--external) or private_endpoint (--internal) verbatim as the kubeconfig
// cluster server.
type ClusterEndpoint struct {
	// PublicEndpoint is "" when BackendCluster.PublicEndpoint is false.
	PublicEndpoint  string
	PrivateEndpoint string
	// CACertificate is the PEM cluster CA the API server's certificate
	// chains to (status.control_plane.auth.cluster_ca_certificate).
	CACertificate string
	// KubernetesVersion is the running "<major>.<minor>.<patch>", e.g.
	// "1.35.2"; the domain adds the "-nebius-cp.<n>" infra suffix.
	KubernetesVersion string
}

// BackendNodeGroup is the desired set of Kubernetes nodes of a node group.
type BackendNodeGroup struct {
	ClusterID   string
	NodeGroupID string
	Name        string
	// Version is the node Kubernetes version "<major>.<minor>".
	Version string
	// Labels are the Kubernetes Node labels (spec.template.metadata.labels
	// plus the service's own, see NodeLabels).
	Labels map[string]string
	Taints []NodeTaint
	Nodes  []BackendNode
}

// BackendNode is one node: a compute instance the domain created for the
// node group through peer.ComputeNodes.
type BackendNode struct {
	// Name is the Kubernetes Node name, equal to the compute instance name.
	Name       string
	InstanceID string
	// PrivateIP and PublicIP are the instance's addresses ("" when none).
	PrivateIP string
	PublicIP  string
	Platform  string
	Preset    string
	// GPUs is the GPU count of the preset (compute platform catalog), 0 for
	// CPU presets.
	GPUs int
}

// NodeTaint is a Kubernetes Node taint; Effect is "NoSchedule",
// "PreferNoSchedule" or "NoExecute".
type NodeTaint struct {
	Key    string
	Value  string
	Effect string
}

// NodeReadiness is one node's state as the backend sees it.
type NodeReadiness struct {
	Name  string
	Ready bool
	// Reason says why a node is not ready ("" when ready).
	Reason string
}
