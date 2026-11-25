// Package domain holds the Object Storage business logic: Nebius bucket
// semantics (naming, lifecycle states, soft delete, versioning) and the
// access decisions of the S3 data plane. It is transport-free and depends only
// on the ports declared in this file.
package domain

import (
	"context"

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
