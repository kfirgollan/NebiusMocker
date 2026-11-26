// Package domain holds the Object Storage business logic: Nebius bucket
// semantics (naming, lifecycle states, soft delete, versioning) and the
// access decisions of the S3 data plane. It is transport-free and depends only
// on the ports declared in this file.
package domain

import (
	"context"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// KindBucket is the NID type prefix of buckets.
const KindBucket = "storagebucket"

// KindProject is the NID type prefix of a bucket's parent.
const KindProject = "project"

// BucketPage is one page of a bucket listing.
type BucketPage struct {
	Items         []*storagepb.Bucket
	NextPageToken string
}

// BucketRepository stores bucket metadata. Bucket names are unique across the
// whole mocker (S3 DNS semantics), not only per project.
type BucketRepository interface {
	Get(ctx context.Context, id string) (*storagepb.Bucket, error)
	// GetByName finds a bucket by its parent project and name.
	GetByName(ctx context.Context, parentID, name string) (*storagepb.Bucket, error)
	// GetByBucketName finds a bucket by its global name (the S3 bucket).
	GetByBucketName(ctx context.Context, name string) (*storagepb.Bucket, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (BucketPage, error)
	// ListAll returns every bucket (for the purge sweeper and S3 ListBuckets).
	ListAll(ctx context.Context) ([]*storagepb.Bucket, error)
	// Create fails with errs.AlreadyExists when the name is taken anywhere.
	Create(ctx context.Context, b *storagepb.Bucket) error
	Update(ctx context.Context, b *storagepb.Bucket) error
	// Delete removes the bucket and releases its name.
	Delete(ctx context.Context, id string) error
}

// ObjectBackend is the slice of the object store (pkg/objstore) buckets need.
type ObjectBackend interface {
	EnsureBucket(ctx context.Context, bucket string) error
	DeleteBucket(ctx context.Context, bucket string) error
	BucketEmpty(ctx context.Context, bucket string) (bool, error)
	SetVersioning(ctx context.Context, bucket string, v objstore.Versioning) error
}

// Operations starts long-running operations (satisfied by *operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
	// Get returns an operation (synchronous S3 calls wait on it).
	Get(ctx context.Context, id string) (*operations.Operation, error)
}

// IDGenerator creates NIDs (satisfied by pkg/nid generators).
type IDGenerator interface {
	New(typ string) (string, error)
}

// Hierarchy resolves resource parents (satisfied by *authz.Engine). Projects
// are owned by IAM; Parent returns errs.NotFound for unknown IDs.
type Hierarchy interface {
	Parent(ctx context.Context, id string) (string, error)
}

// Filesystems resolves the compute shared filesystems that filesystem buckets
// expose (the compute module's peer.Filesystems). Filesystem returns
// errs.NotFound for unknown IDs.
type Filesystems interface {
	Filesystem(ctx context.Context, id string) (peer.Filesystem, error)
	// Expose records the bucket on the filesystem atomically with the
	// filesystem's checks, so the filesystem cannot be deleted while the
	// bucket exists; Unexpose removes the record (idempotent).
	Expose(ctx context.Context, fsID, bucketID, projectID string) error
	Unexpose(ctx context.Context, fsID, bucketID string) error
}

// AccessChecker evaluates permissions (satisfied by *authz.Engine).
type AccessChecker interface {
	Check(ctx context.Context, principalID string, perm authz.Permission, target string) (authz.Decision, error)
}

// KeyResolver authenticates S3 access keys (the iam module's peer.AccessKeys).
type KeyResolver interface {
	ResolveAccessKey(ctx context.Context, accessKeyID string) (peer.AccessKey, error)
}

// ContentBackend is the object-store introspection behind bucket counters,
// size limits and lifecycle rules (objstore.Inventory plus deletion of the
// current object version, which leaves a delete marker in versioned buckets).
type ContentBackend interface {
	objstore.Inventory
	Delete(ctx context.Context, bucket, key string) error
}

// GroupMembers answers group membership for bucket policies (the iam
// module's peer.GroupMembers).
type GroupMembers interface {
	IsMember(ctx context.Context, principalID, groupID string) (bool, error)
}

// KindInventory is the NID type prefix of bucket inventory configurations
// (children of their source bucket).
const KindInventory = "storagebucketinventory"

// KindTransfer is the NID type prefix of data transfers (children of a project).
const KindTransfer = "storagetransfer"

// InventoryPage is one page of an inventory listing.
type InventoryPage struct {
	Items         []*storagepb.Inventory
	NextPageToken string
}

// InventoryRepository stores inventory configurations and their scheduler
// state. Names are unique per source bucket.
type InventoryRepository interface {
	Get(ctx context.Context, id string) (*storagepb.Inventory, error)
	GetByName(ctx context.Context, bucketID, name string) (*storagepb.Inventory, error)
	List(ctx context.Context, bucketID string, pageSize int, pageToken string) (InventoryPage, error)
	// ListAll returns every inventory (for the report scheduler).
	ListAll(ctx context.Context) ([]*storagepb.Inventory, error)
	// Count returns how many inventories a bucket has.
	Count(ctx context.Context, bucketID string) (int, error)
	// Create fails with errs.AlreadyExists when the name is taken in the bucket,
	// and with KindQuotaExceeded when the bucket already has
	// MaxInventoriesPerBucket inventories (checked atomically with the insert).
	Create(ctx context.Context, inv *storagepb.Inventory, st InventoryState) error
	Update(ctx context.Context, inv *storagepb.Inventory) error
	// State and SetState read and write the scheduler state of an inventory.
	State(ctx context.Context, id string) (InventoryState, error)
	SetState(ctx context.Context, id string, st InventoryState) error
	// Delete removes the inventory and its state.
	Delete(ctx context.Context, id string) error
}

// AccessLog records object reads for the LAST_ACCESS inventory field.
type AccessLog interface {
	// Touch records that key in bucketID was read at t.
	Touch(ctx context.Context, bucketID, key string, t time.Time) error
	// LastAccess returns the last recorded read of key, ok=false when none.
	LastAccess(ctx context.Context, bucketID, key string) (t time.Time, ok bool, err error)
	// Forget drops every record of a bucket (on purge).
	Forget(ctx context.Context, bucketID string) error
}

// QuotaGate admits a quota claim and runs apply, the store write that makes the
// consumption visible to the usage source (satisfied by peer.QuotaGate).
type QuotaGate interface {
	Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error
}
