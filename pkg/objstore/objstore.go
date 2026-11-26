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

// NullVersion is the version ID of objects written while versioning was
// never enabled or is suspended.
const NullVersion = "null"

// Version is one object version or delete marker.
type Version struct {
	Key       string
	VersionID string // NullVersion for unversioned objects
	// IsLatest marks the current version (or a current delete marker).
	IsLatest     bool
	DeleteMarker bool
	Size         int64
	ETag         string // without quotes; multipart ETags end in "-<parts>"
	// StorageClass is the backend's class name ("STANDARD" when unset).
	StorageClass string
	LastModified time.Time
	// ChecksumAlgorithms are the algorithms of the object's stored
	// checksums, e.g. CRC64NVME; empty when it has none.
	ChecksumAlgorithms []string
}

// Multipart reports whether the version was assembled from a multipart upload.
func (v Version) Multipart() bool {
	for i := len(v.ETag) - 1; i >= 0; i-- {
		if v.ETag[i] == '-' {
			return i < len(v.ETag)-1
		}
		if v.ETag[i] < '0' || v.ETag[i] > '9' {
			return false
		}
	}
	return false
}

// Upload is an incomplete multipart upload with its uploaded parts.
type Upload struct {
	Key          string
	UploadID     string
	StorageClass string // "STANDARD" when unset
	Initiated    time.Time
	Parts        int64
	PartsSize    int64
}

// Inventory is the bucket-content introspection and maintenance a backend
// offers on top of Store: bucket counters, size limits and lifecycle rules
// are computed from it.
type Inventory interface {
	// ListVersions yields every version and delete marker whose key starts
	// with prefix, by key and then newest first. Unversioned objects are
	// yielded once with VersionID NullVersion.
	ListVersions(ctx context.Context, bucket, prefix string) iter.Seq2[Version, error]
	// ListUploads yields the incomplete multipart uploads with their parts.
	ListUploads(ctx context.Context, bucket string) iter.Seq2[Upload, error]
	// DeleteVersion permanently removes one version or delete marker; a
	// missing version is not an error.
	DeleteVersion(ctx context.Context, bucket, key, versionID string) error
	// AbortUpload aborts a multipart upload; a missing upload is not an error.
	AbortUpload(ctx context.Context, bucket, key, uploadID string) error
	// ObjectTags returns the tags of one object version.
	ObjectTags(ctx context.Context, bucket, key, versionID string) (map[string]string, error)
}

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
