// Package domain holds the Managed PostgreSQL (nebius.msp.postgresql.v1alpha1)
// control-plane logic: clusters, backups, the version/preset catalog,
// validation, lifecycle phases and the automatic backup schedule. PostgreSQL
// itself runs in a Backend (the ports in this file).
package domain

import (
	"context"
	"time"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Resource type prefixes (NID types) owned by the area, and the kinds it
// refers to.
const (
	KindCluster = "postgresql"
	// KindBackup names backup IDs. Backups are not IAM resources of their
	// own (the API addresses them by cluster_id + backup_id).
	KindBackup = "postgresqlbackup"
	// KindProject is the only parent kind a cluster may have (owned by IAM).
	KindProject = "project"
)

// Backend is the PostgreSQL data plane behind clusters, selected with
// NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND. The control plane is the domain's:
// resources, validation, operations, backup records and their schedule. A
// backend runs PostgreSQL (docker) or reports that it does (mock).
//
// The domain calls a backend from operation Work, never inside a store
// transaction, and serializes calls per cluster (one operation at a time on a
// cluster; backup operations lock the cluster too). Every method is
// idempotent and reconciling: it may be called again with the same input.
// Methods may block while containers start and must return when ctx is
// canceled. Errors are pkg/errs kinds; they fail the operation with that
// kind.
type Backend interface {
	// Name is the NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND value ("mock", "docker").
	Name() string

	// EnsureCluster creates the cluster's hosts, or reconciles running (or
	// stopped) ones to c: parameters, resources, host count, pooler,
	// password. It returns once the primary and the pooler accept
	// connections with the bootstrap credentials. A stopped cluster is
	// started.
	EnsureCluster(ctx context.Context, c BackendCluster) (Endpoints, error)

	// StopCluster stops every host and the pooler, keeping the data. A
	// cluster the backend does not know is not an error.
	StopCluster(ctx context.Context, clusterID string) error

	// DeleteCluster removes the hosts, the pooler and the data. Backups (and
	// the WAL archive they need) stay until DeleteBackup. A cluster the
	// backend does not know is not an error.
	DeleteCluster(ctx context.Context, clusterID string) error

	// CreateBackup takes a full base backup of the running cluster's
	// primary into backup storage, under backupID, and returns its size.
	CreateBackup(ctx context.Context, clusterID, backupID string) (BackupInfo, error)

	// DeleteBackup removes a backup's data. keepWALFrom is the start of the
	// oldest backup of the cluster that remains (zero when none remains):
	// archived WAL older than it is no longer needed for point-in-time
	// recovery and may be removed. A missing backup is not an error.
	DeleteBackup(ctx context.Context, clusterID, backupID string, keepWALFrom time.Time) error

	// CheckDisk measures the on-disk size of the cluster's data (the primary's
	// data directory, WAL included; archived WAL and backups do not count)
	// and enforces limitBytes: at or above it every database refuses writes
	// and open sessions are ended; below it a cluster that was full accepts
	// writes again. It is idempotent and keeps no state of its own. A cluster
	// the backend does not know, or a stopped one, reports zero usage.
	CheckDisk(ctx context.Context, clusterID string, limitBytes int64) (DiskUsage, error)

	// Close releases what the backend holds at mocker shutdown.
	Close(ctx context.Context) error
}

// BackendCluster is the desired state of one cluster.
type BackendCluster struct {
	// ID is the cluster NID (postgresql-…); backends key their state by it.
	ID        string
	ProjectID string
	Name      string
	// Version is the PostgreSQL major version ("16"), a key of
	// config.MSPConfig.PostgresImages.
	Version string
	// Hosts is spec.config.template.hosts.count: one primary plus Hosts-1
	// streaming replicas.
	Hosts int
	// CPUs and MemoryBytes are the preset's resources, the limits of every
	// host.
	CPUs        int64
	MemoryBytes int64
	// DiskGiB is spec.config.template.disk.size_gibibytes.
	DiskGiB int64
	// Parameters are the postgresql.conf settings of
	// spec.config.postgresql_config_16, by PostgreSQL parameter name, with
	// values rendered as postgresql.conf accepts them (units included,
	// e.g. "statement_timeout": "30000ms"), unquoted: a backend that writes
	// them into a file quotes them. Only the set fields appear.
	Parameters map[string]string
	// Pooler is the connection pooler of the read-write endpoint.
	Pooler Pooler
	// PublicAccess asks for public endpoints (spec.config.public_access).
	PublicAccess bool
	// UserName and DBName are the bootstrap user and database.
	UserName string
	DBName   string
	// Password is the bootstrap user's password when the request that
	// caused this call set one (create, restore, or an update of
	// spec.bootstrap.user_password); "" keeps the current one. The domain
	// never stores it.
	Password string
	// Restore, when set on the first EnsureCluster of a cluster, seeds its
	// data from a backup instead of initializing an empty database.
	Restore *RestoreSource
}

// Pooler is spec.config.pooler_config with defaults applied.
type Pooler struct {
	// Mode is "session" or "transaction".
	Mode string
	// MaxPoolSize is the pool size per user/database pair.
	MaxPoolSize int64
}

// RestoreSource names the backup a restored cluster starts from.
type RestoreSource struct {
	SourceClusterID string
	BackupID        string
	// RecoveryTime is the point-in-time recovery target; zero restores the
	// backup as taken.
	RecoveryTime time.Time
}

// DiskUsage is what CheckDisk found.
type DiskUsage struct {
	UsedBytes  int64
	LimitBytes int64
	// Full is true while the cluster refuses writes because UsedBytes
	// reached LimitBytes.
	Full bool
}

// Endpoints are the "host:port" addresses clients connect to
// (status.connection_endpoints). Public ones are "" without public access.
type Endpoints struct {
	PrivateReadWrite string
	PrivateReadOnly  string
	PublicReadWrite  string
	PublicReadOnly   string
}

// BackupInfo is what a backend reports of a finished backup.
type BackupInfo struct {
	SizeBytes int64
}

// Page is one page of a listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// ClusterRepository stores clusters. Names are unique per project.
type ClusterRepository interface {
	// Get returns the cluster or errs.NotFound.
	Get(ctx context.Context, id string) (*pgpb.Cluster, error)
	// GetByName returns the cluster named name in the project, or errs.NotFound.
	GetByName(ctx context.Context, parentID, name string) (*pgpb.Cluster, error)
	// All returns every cluster (the backup schedule sweeps them).
	All(ctx context.Context) ([]*pgpb.Cluster, error)
	// List returns one page of the project's clusters in creation order.
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (Page[*pgpb.Cluster], error)
	// Create fails with errs.AlreadyExists if the name is taken.
	Create(ctx context.Context, c *pgpb.Cluster) error
	// Update fails with errs.NotFound if the cluster does not exist.
	Update(ctx context.Context, c *pgpb.Cluster) error
	Delete(ctx context.Context, id string) error
}

// BackupRecord is a stored backup with what the API does not show.
type BackupRecord struct {
	Backup *pgpb.Backup
	// ProjectID is the source cluster's project (List by project).
	ProjectID string
	// Done is false while the backup is being taken.
	Done bool
	// Cluster is the source cluster as it was when the backup started (spec
	// and metadata, never a password): GetForBackup returns it, also after
	// the cluster is deleted.
	Cluster *pgpb.Cluster
	// RetentionDays is how long an automatic backup is kept (the cluster's
	// backup.retention_policy then); 0 for on-demand backups, which stay
	// until BackupService.Delete.
	RetentionDays int
}

// BackupRepository stores backups. They outlive their cluster until
// retention expires.
type BackupRepository interface {
	// Get returns the backup of the cluster, or errs.NotFound.
	Get(ctx context.Context, clusterID, backupID string) (BackupRecord, error)
	// ListByProject returns one page of a project's backups, newest
	// creation_start first, then cluster ID and backup ID ascending.
	ListByProject(ctx context.Context, projectID string, pageSize int, pageToken string) (Page[BackupRecord], error)
	// ListByCluster is ListByProject for one source cluster.
	ListByCluster(ctx context.Context, clusterID string, pageSize int, pageToken string) (Page[BackupRecord], error)
	// All returns every backup (the schedule sweeps them).
	All(ctx context.Context) ([]BackupRecord, error)
	Put(ctx context.Context, b BackupRecord) error
	// Delete removes a record; a missing one is not an error.
	Delete(ctx context.Context, clusterID, backupID string) error
}

// NetworkBindingRepository remembers the network each cluster holds, so the
// binding is released on delete.
type NetworkBindingRepository interface {
	Set(ctx context.Context, clusterID, networkID string) error
	// Get returns the recorded network or errs.NotFound.
	Get(ctx context.Context, clusterID string) (string, error)
	// Delete forgets the record; a missing one is not an error.
	Delete(ctx context.Context, clusterID string) error
}

// Hierarchy resolves IAM containers (deps.Authz).
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
}

// Operations runs long-running operations (pkg/operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates resource IDs.
type IDGenerator interface {
	New(typ string) (string, error)
}
