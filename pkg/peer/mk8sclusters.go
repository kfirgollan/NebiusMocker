package peer

import "context"

// MK8sClustersKey is the capability provided by the mk8s module: Managed
// Kubernetes clusters as seen by what is installed into them (Applications
// K8s releases).
var MK8sClustersKey = NewKey[MK8sClusters]("mk8s.clusters")

// MK8sClusters resolves mk8s clusters. It is an internal call: the caller
// authorizes its own request.
type MK8sClusters interface {
	// Cluster returns the cluster with the ID, or errs.NotFound.
	Cluster(ctx context.Context, id string) (MK8sCluster, error)
}

// MK8sCluster is a resolved mk8s cluster.
type MK8sCluster struct {
	ID        string
	ProjectID string
	Name      string
	// Running is true when status.state is RUNNING.
	Running bool
}

// ClusterReleasesKey is the capability provided by the applications module:
// the K8s releases installed into an mk8s cluster.
var ClusterReleasesKey = NewKey[ClusterReleases]("applications.cluster-releases")

// ClusterReleases removes what is installed into a cluster when the cluster
// goes away (mk8s cluster Delete cascades to its releases).
type ClusterReleases interface {
	// DeleteClusterReleases deletes every K8s release of the cluster. A
	// cluster without releases is not an error.
	DeleteClusterReleases(ctx context.Context, clusterID string) error
}
