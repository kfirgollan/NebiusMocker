// Package domain holds the Managed MLflow (nebius.msp.mlflow.v1alpha1)
// control-plane logic: tracking server clusters, their size catalog,
// validation, lifecycle phases, and the artifact store each cluster gets in
// Object Storage (a bucket plus an access key of the cluster's service
// account). The MLflow tracking server itself runs in a Backend (the ports
// in this file).
package domain

import (
	"context"

	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Resource type prefixes (NID types) owned by the area, and the kinds it
// refers to.
const (
	KindCluster        = "mlflow"
	KindProject        = "project"
	KindServiceAccount = "serviceaccount"
	KindNetwork        = "vpcnetwork"
)

// Backend is the MLflow data plane behind clusters, selected with
// NEBIUS_MOCKER_MSP_MLFLOW_BACKEND: a real tracking server (docker) or none
// (mock).
//
// The domain calls a backend from operation Work, never inside a store
// transaction, one operation at a time per cluster. Every method is
// idempotent and must return when ctx is canceled. Errors are pkg/errs
// kinds; they fail the operation with that kind.
type Backend interface {
	// Name is the NEBIUS_MOCKER_MSP_MLFLOW_BACKEND value ("mock", "docker").
	Name() string

	// EnsureCluster creates the tracking server, or starts a stopped one,
	// and returns once it answers authenticated API calls.
	EnsureCluster(ctx context.Context, c BackendCluster) (Endpoints, error)

	// StopCluster stops the server, keeping its data (runs, experiments,
	// users). A cluster the backend does not know is not an error.
	StopCluster(ctx context.Context, clusterID string) error

	// DeleteCluster removes the server and its data. The artifacts stay in
	// the bucket. A cluster the backend does not know is not an error.
	DeleteCluster(ctx context.Context, clusterID string) error

	// ExperimentsCount returns the number of active experiments of a
	// running cluster (status.experiments_count).
	ExperimentsCount(ctx context.Context, clusterID string) (int, error)

	// Close releases what the backend holds at mocker shutdown.
	Close(ctx context.Context) error
}

// BackendCluster is the desired tracking server of one cluster.
type BackendCluster struct {
	// ID is the cluster NID (mlflow-…); backends key their state by it.
	ID        string
	ProjectID string
	Name      string
	// AdminUsername and AdminPassword are the MLflow admin's credentials
	// (HTTP basic auth). The password is set on the first EnsureCluster
	// only; the domain never stores it.
	AdminUsername string
	AdminPassword string
	// PublicAccess asks for a public endpoint (spec.public_access).
	PublicAccess bool
	// CPUs and MemoryBytes are the size's resources, the server's limits.
	CPUs        int64
	MemoryBytes int64
	// Artifacts is where the server stores run artifacts.
	Artifacts ArtifactStore
}

// ArtifactStore is an Object Storage bucket and the S3 credentials of the
// cluster's service account. The backend knows how its server reaches the
// mocker's S3 front (config.MSPConfig.S3Endpoint).
type ArtifactStore struct {
	Bucket      string
	Region      string
	AccessKeyID string
	Secret      string
}

// Endpoints are the tracking server addresses, "host:port" without a
// scheme (clients prepend https://, docs.nebius.com/mlflow). Public is ""
// without public access.
type Endpoints struct {
	Private string
	Public  string
}

// Page is one page of a listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// ClusterRepository stores clusters. Names are unique per project.
type ClusterRepository interface {
	// Get returns the cluster or errs.NotFound.
	Get(ctx context.Context, id string) (*mlflowpb.Cluster, error)
	// GetByName returns the cluster named name in the project, or errs.NotFound.
	GetByName(ctx context.Context, parentID, name string) (*mlflowpb.Cluster, error)
	// List returns one page of the project's clusters in creation order.
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (Page[*mlflowpb.Cluster], error)
	// Create fails with errs.AlreadyExists if the name is taken.
	Create(ctx context.Context, c *mlflowpb.Cluster) error
	// Update fails with errs.NotFound if the cluster does not exist.
	Update(ctx context.Context, c *mlflowpb.Cluster) error
	Delete(ctx context.Context, id string) error
}

// Internals is what the domain keeps of a cluster beyond the API resource.
type Internals struct {
	// NetworkID is the network the cluster is bound to.
	NetworkID string
	// AccessKeyID is the IAM access key ("accesskey-…") created for the
	// service account; deleted with the cluster.
	AccessKeyID string
	// AWSAccessKeyID and Secret are its S3 credentials, for restarts of a
	// stopped server.
	AWSAccessKeyID string
	Secret         string
}

// InternalsRepository stores Internals by cluster ID.
type InternalsRepository interface {
	Put(ctx context.Context, clusterID string, in Internals) error
	// Get returns the record or errs.NotFound.
	Get(ctx context.Context, clusterID string) (Internals, error)
	// Delete forgets the record; a missing one is not an error.
	Delete(ctx context.Context, clusterID string) error
}

// QuotaGate admits a quota claim and runs apply, the store write that makes
// the consumption visible to the area's usage source (satisfied by
// peer.QuotaGate).
type QuotaGate interface {
	Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error
}

// Hierarchy resolves IAM containers and resources (deps.Authz).
type Hierarchy interface {
	// Parent returns the parent ID of a resource, or errs.NotFound.
	Parent(ctx context.Context, id string) (string, error)
}

// Peers finds the capabilities of other areas at call time; ok is false
// when the providing module is disabled.
type Peers interface {
	Networks() (peer.Networks, bool)
	DefaultSubnets() (peer.DefaultSubnets, bool)
	ProjectRegions() (peer.ProjectRegions, bool)
	Buckets() (peer.Buckets, bool)
	SystemAccessKeys() (peer.SystemAccessKeys, bool)
}

// Operations runs long-running operations (pkg/operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates resource IDs.
type IDGenerator interface {
	New(typ string) (string, error)
}
