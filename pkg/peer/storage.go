package peer

import "context"

// BucketResolver maps an Object Storage bucket ID to its pkg/objstore backend bucket name. Provided by the storage module.
type BucketResolver interface {
	BackendBucket(ctx context.Context, bucketID string) (string, error) // errs.NotFound if no such bucket
}

// BucketResolverKey is the capability provided by the storage module.
var BucketResolverKey = NewKey[BucketResolver]("storage.bucket-resolver")
