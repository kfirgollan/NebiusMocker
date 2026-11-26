package peer

import "context"

// BucketsKey is the capability provided by the storage module: Object
// Storage buckets created and resolved on behalf of other services, e.g. the
// artifact bucket of a Managed MLflow cluster.
var BucketsKey = NewKey[Buckets]("storage.buckets")

// Buckets resolves and creates buckets by name. It is an internal call: the
// provider runs no IAM checks, because the consumer has already authorized
// its own request. Created buckets are ordinary buckets (default settings),
// visible and deletable through BucketService.
type Buckets interface {
	// BucketByName returns the active bucket named name in the project, or
	// errs.NotFound.
	BucketByName(ctx context.Context, projectID, name string) (Bucket, error)
	// CreateBucket creates a bucket with default settings in the project and
	// returns it once active. Errors: errs.AlreadyExists when the name is
	// taken; errs.BadRequest for an invalid name; errs.NotFound for a missing
	// project.
	CreateBucket(ctx context.Context, projectID, name string) (Bucket, error)
}

// Bucket is a resolved bucket.
type Bucket struct {
	ID        string
	ProjectID string
	Name      string
}
