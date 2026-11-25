// Package objstore is the object-storage backend shared by NebiusMocker
// modules that keep blobs (Object Storage buckets, compute image data).
//
// NebiusMocker does not implement object storage itself. Production use is the
// RustFS implementation (pkg/objstore/rustfs), an S3 client to a RustFS
// container; pkg/objstore/memory is a map-backed implementation for
// Docker-free tests. Consumers declare their own narrow port over Store in
// their domain/ports.go.
//
// The package is transport-free: errors are the sentinels below, which
// consumers map to their domain error kinds.
package objstore

import (
	"context"
	"errors"
	"io"
	"iter"
	"time"
)

// ErrNotFound reports that an object does not exist.
var ErrNotFound = errors.New("objstore: object not found")

// ErrBucketNotFound reports that a bucket does not exist.
var ErrBucketNotFound = errors.New("objstore: bucket not found")

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Bucket       string
	Key          string
	Size         int64
	ETag         string // without quotes
	ContentType  string
	Metadata     map[string]string // user metadata (x-amz-meta-*), lower-case keys
	LastModified time.Time
}

// PutOptions are optional attributes of an uploaded object.
type PutOptions struct {
	ContentType string
	Metadata    map[string]string
}

// Versioning is a bucket's S3 versioning state. A bucket that was never
// versioned is VersioningUnset; once enabled it can only be suspended.
type Versioning int

// Versioning states.
const (
	VersioningUnset Versioning = iota
	VersioningEnabled
	VersioningSuspended
)

// Store is an S3-like object store.
type Store interface {
	// EnsureBucket creates the bucket unless it exists.
	EnsureBucket(ctx context.Context, bucket string) error
	// DeleteBucket removes the bucket with every object and object version
	// in it. A missing bucket is not an error.
	DeleteBucket(ctx context.Context, bucket string) error
	// BucketEmpty reports whether the bucket holds no current objects.
	BucketEmpty(ctx context.Context, bucket string) (bool, error)
	// SetVersioning applies a versioning state; VersioningUnset is a no-op.
	SetVersioning(ctx context.Context, bucket string, v Versioning) error

	// Put stores an object, streaming r. size is the exact length of r, or
	// negative when unknown.
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, opts PutOptions) (ObjectInfo, error)
	// Get streams an object. The caller closes the reader.
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectInfo, error)
	// Stat returns an object's attributes.
	Stat(ctx context.Context, bucket, key string) (ObjectInfo, error)
	// Delete removes an object; a missing object is not an error.
	Delete(ctx context.Context, bucket, key string) error
	// Copy copies an object server-side.
	Copy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (ObjectInfo, error)
	// List yields the bucket's objects whose key starts with prefix, in key order.
	List(ctx context.Context, bucket, prefix string) iter.Seq2[ObjectInfo, error]
}
