package peer

import "context"

// FilesystemsKey is the capability provided by the compute module: the data
// plane of shared filesystems (compute.v1 Filesystem), for Object Storage
// filesystem buckets (storage.v1 BucketSpec.filesystem_bucket), which expose
// a filesystem through the S3 API.
var FilesystemsKey = NewKey[Filesystems]("compute.filesystems")

// Filesystems resolves shared filesystems to their stored files.
type Filesystems interface {
	// Filesystem returns the filesystem with the ID, or an errs.NotFound
	// error. It is an internal call: the caller authorizes access itself.
	Filesystem(ctx context.Context, id string) (Filesystem, error)
}

// FilesystemBucketsKey is the capability provided by the storage module: the
// filesystem buckets that expose a filesystem. Compute refuses to delete a
// filesystem while one exists.
var FilesystemBucketsKey = NewKey[FilesystemBuckets]("storage.filesystem-buckets")

// FilesystemBuckets lists the Object Storage buckets that expose a filesystem.
type FilesystemBuckets interface {
	// BucketsOf returns the names of the filesystem buckets whose
	// spec.filesystem_bucket.filesystem_id is filesystemID (none: empty).
	BucketsOf(ctx context.Context, filesystemID string) ([]string, error)
}

// Filesystem is the data-plane view of a shared filesystem.
type Filesystem struct {
	// ID is the filesystem NID ("computefilesystem-…").
	ID string
	// ParentID is the project of the filesystem.
	ParentID string
	// Ready is true when the filesystem is READY or UPDATING (a hot resize):
	// its files can be read and written.
	Ready bool
	// CapacityBytes is the filesystem size (status.size_bytes): the stored
	// files may not exceed it.
	CapacityBytes int64
	// BackendBucket is the pkg/objstore bucket that holds the filesystem's
	// files, one object per file keyed by its path without the leading "/".
	BackendBucket string
}
