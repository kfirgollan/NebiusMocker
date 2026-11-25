package peer

import "context"

// FilesystemsKey is the capability provided by the compute module: the data
// plane of shared filesystems (compute.v1 Filesystem), for Object Storage
// filesystem buckets (storage.v1 BucketSpec.filesystem_bucket), which expose
// a filesystem through the S3 API.
var FilesystemsKey = NewKey[Filesystems]("compute.filesystems")

// Filesystems resolves shared filesystems to their stored files and records
// which buckets expose them. All calls are internal: the caller authorizes
// access itself.
type Filesystems interface {
	// Filesystem returns the filesystem with the ID, or an errs.NotFound error.
	Filesystem(ctx context.Context, id string) (Filesystem, error)
	// Expose atomically checks that the filesystem exists (errs.NotFound), is
	// in projectID (errs.BadRequest) and is READY or UPDATING (errs.BadState),
	// and records bucketID as exposing it. While any bucket exposes it, the
	// filesystem cannot be deleted; the check and the deletion are atomic
	// against Expose. Exposing twice is a no-op.
	Expose(ctx context.Context, fsID, bucketID, projectID string) error
	// Unexpose removes the record; a missing filesystem or record is not an error.
	Unexpose(ctx context.Context, fsID, bucketID string) error
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
