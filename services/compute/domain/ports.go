// Package domain holds the Compute business logic (images for now). It is
// transport-free and depends only on the ports declared in this file.
package domain

import (
	"context"
	"io"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// KindImage is the NID type prefix of compute images.
const KindImage = "computeimage"

// ImagePage is one page of an image listing.
type ImagePage struct {
	Items         []*computepb.Image
	NextPageToken string
}

// ImageRepository stores images.
type ImageRepository interface {
	Get(ctx context.Context, id string) (*computepb.Image, error)
	GetByName(ctx context.Context, parentID, name string) (*computepb.Image, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (ImagePage, error)
	// All returns every image of parentID in creation order (family lookups).
	All(ctx context.Context, parentID string) ([]*computepb.Image, error)
	// Create fails with errs.AlreadyExists if the ID or the name in the parent is taken.
	Create(ctx context.Context, img *computepb.Image) error
	// Update fails with errs.NotFound if the image does not exist.
	Update(ctx context.Context, img *computepb.Image) error
	Delete(ctx context.Context, id string) error
}

// OperationIndex remembers which operations ran on images of a parent, so
// ListOperationsByParent still finds operations of deleted images.
type OperationIndex interface {
	Add(ctx context.Context, parentID, operationID string) error
	// IDs returns the parent's operation IDs in submission order.
	IDs(ctx context.Context, parentID string) ([]string, error)
}

// Operations starts and reads long-running operations (satisfied by *operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
	Get(ctx context.Context, id string) (*operations.Operation, error)
}

// IDGenerator creates NIDs (satisfied by pkg/nid generators).
type IDGenerator interface {
	New(typ string) (string, error)
}

// Projects resolves project existence (satisfied by *authz.Engine, whose
// "project" kind is registered by the IAM module).
type Projects interface {
	// Parent returns the parent of id, or errs.NotFound if id does not exist.
	Parent(ctx context.Context, id string) (string, error)
}

// BlobInfo describes a stored object.
type BlobInfo struct {
	Size     int64
	Metadata map[string]string
}

// Blobs is where image payloads live: the mocker's object store (RustFS or
// memory). Image blobs are kept in a compute-owned bucket under a key per
// image; sources are read from users' Object Storage buckets.
type Blobs interface {
	// PutImage stores the payload of an image. size is exact.
	PutImage(ctx context.Context, imageID string, r io.Reader, size int64, meta map[string]string) (BlobInfo, error)
	// StatImage returns the stored blob of an image, or errs.NotFound.
	StatImage(ctx context.Context, imageID string) (BlobInfo, error)
	// DeleteImage removes the blob of an image; a missing blob is not an error.
	DeleteImage(ctx context.Context, imageID string) error
	// OpenSource opens an object in a user bucket, or returns errs.NotFound.
	OpenSource(ctx context.Context, bucket, object string) (io.ReadCloser, BlobInfo, error)
	// StatSource returns an object in a user bucket, or errs.NotFound.
	StatSource(ctx context.Context, bucket, object string) (BlobInfo, error)
}

// NID type prefixes of disks and disk snapshots.
const (
	KindDisk         = "computedisk"
	KindDiskSnapshot = "computedisksnapshot"
)

// DiskReader reads disks and disk snapshots for other compute resources (for
// example images created from a disk or snapshot). Both methods return
// errs.NotFound when the ID does not exist. Disks and snapshots keep no data
// payload: the mocker never allocates storage.
type DiskReader interface {
	Disk(ctx context.Context, id string) (*computepb.Disk, error)
	Snapshot(ctx context.Context, id string) (*computepb.DiskSnapshot, error)
}

// DiskPage is one page of a disk listing.
type DiskPage struct {
	Items         []*computepb.Disk
	NextPageToken string
}

// SnapshotPage is one page of a disk snapshot listing.
type SnapshotPage struct {
	Items         []*computepb.DiskSnapshot
	NextPageToken string
}

// DiskRepository stores disks.
type DiskRepository interface {
	Get(ctx context.Context, id string) (*computepb.Disk, error)
	GetByName(ctx context.Context, parentID, name string) (*computepb.Disk, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (DiskPage, error)
	// Create fails with errs.AlreadyExists if the ID or the name in the parent is taken.
	Create(ctx context.Context, d *computepb.Disk) error
	// Mutate atomically loads the disk, lets fn change it and stores the
	// result. An error from fn aborts without writing. It returns the stored
	// disk, or errs.NotFound.
	Mutate(ctx context.Context, id string, fn func(*computepb.Disk) error) (*computepb.Disk, error)
	Delete(ctx context.Context, id string) error
}

// SnapshotRepository stores disk snapshots.
type SnapshotRepository interface {
	Get(ctx context.Context, id string) (*computepb.DiskSnapshot, error)
	GetByName(ctx context.Context, parentID, name string) (*computepb.DiskSnapshot, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (SnapshotPage, error)
	// ListByDisk pages through the snapshots in parentID whose source is diskID.
	ListByDisk(ctx context.Context, parentID, diskID string, pageSize int, pageToken string) (SnapshotPage, error)
	// Create fails with errs.AlreadyExists if the ID or the name in the parent is taken.
	Create(ctx context.Context, s *computepb.DiskSnapshot) error
	// Mutate is DiskRepository.Mutate for snapshots.
	Mutate(ctx context.Context, id string, fn func(*computepb.DiskSnapshot) error) (*computepb.DiskSnapshot, error)
	Delete(ctx context.Context, id string) error
}

// DiskLocker locks disks while images are created from them (disk.proto:
// "Disk is locked for deletion and for read-write operations while image is
// being created"). Satisfied by *DiskLocks.
type DiskLocker interface {
	// LockForImage atomically checks that the disk exists (errs.NotFound), is
	// in parentID (errs.BadRequest) and is READY (errs.BadState), then adds
	// imageID to status.lock_state.images. Locking twice with the same image
	// is a no-op.
	LockForImage(ctx context.Context, diskID, parentID, imageID string) (*computepb.Disk, error)
	// UnlockForImage removes imageID from the disk's lock. A missing disk or
	// entry is not an error.
	UnlockForImage(ctx context.Context, diskID, imageID string) error
}

// ImageLookup resolves disk sources from images (satisfied by *ImageService).
type ImageLookup interface {
	Get(ctx context.Context, id string) (*computepb.Image, error)
	GetLatestByFamily(ctx context.Context, family, parentID string) (*computepb.Image, error)
	IsPublicProject(id string) bool
}

// KindGpuCluster is the NID type prefix of GPU clusters.
const KindGpuCluster = "computegpucluster"

// GpuClusterPage is one page of a GPU cluster listing.
type GpuClusterPage struct {
	Items         []*computepb.GpuCluster
	NextPageToken string
}

// GpuClusterRepository stores GPU clusters.
type GpuClusterRepository interface {
	Get(ctx context.Context, id string) (*computepb.GpuCluster, error)
	GetByName(ctx context.Context, parentID, name string) (*computepb.GpuCluster, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (GpuClusterPage, error)
	// Create fails with errs.AlreadyExists if the ID or the name in the parent is taken.
	Create(ctx context.Context, c *computepb.GpuCluster) error
	// Mutate is DiskRepository.Mutate for GPU clusters.
	Mutate(ctx context.Context, id string, fn func(*computepb.GpuCluster) error) (*computepb.GpuCluster, error)
	// DeleteIf atomically loads the cluster and deletes it unless check
	// returns an error, which aborts the deletion. A missing cluster is
	// errs.NotFound.
	DeleteIf(ctx context.Context, id string, check func(*computepb.GpuCluster) error) error
}

// ProjectRegions resolves the region of a project (satisfied by
// peer.ProjectRegions, provided by the iam module).
type ProjectRegions interface {
	Region(ctx context.Context, projectID string) (string, error)
}

// GpuClusterMembership is how instances join and leave GPU clusters
// (InstanceSpec.gpu_cluster). Satisfied by *GpuClusterMembers. It keeps
// GpuClusterStatus.instances and a synthetic infiniband_topology_path: no
// fabric is simulated.
type GpuClusterMembership interface {
	// Join atomically checks that the cluster exists (errs.NotFound, naming
	// spec.gpu_cluster.id), is in projectID (errs.BadRequest) and that its
	// fabric accepts platform (errs.BadRequest), then appends instanceID to
	// status.instances and records its topology path, which it returns. A
	// second Join of the same instance returns the recorded path.
	Join(ctx context.Context, clusterID, instanceID, projectID, platform string) (topologyPath []string, err error)
	// Leave removes instanceID from the cluster. A missing cluster or member
	// is not an error.
	Leave(ctx context.Context, clusterID, instanceID string) error
}

// KindFilesystem is the NID type prefix of shared filesystems.
const KindFilesystem = "computefilesystem"

// FilesystemPage is one page of a filesystem listing.
type FilesystemPage struct {
	Items         []*computepb.Filesystem
	NextPageToken string
}

// FilesystemRepository stores shared filesystems.
type FilesystemRepository interface {
	Get(ctx context.Context, id string) (*computepb.Filesystem, error)
	GetByName(ctx context.Context, parentID, name string) (*computepb.Filesystem, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (FilesystemPage, error)
	// Create fails with errs.AlreadyExists if the ID or the name in the parent is taken.
	Create(ctx context.Context, fs *computepb.Filesystem) error
	// Mutate atomically loads the filesystem, lets fn change it and stores
	// the result. An error from fn aborts without writing. It returns the
	// stored filesystem, or errs.NotFound.
	Mutate(ctx context.Context, id string, fn func(*computepb.Filesystem) error) (*computepb.Filesystem, error)
	Delete(ctx context.Context, id string) error
	// Expose atomically loads the filesystem, runs check on it and, if check
	// passes, records bucketID as exposing it (errs.NotFound if missing).
	Expose(ctx context.Context, fsID, bucketID string, check func(*computepb.Filesystem) error) error
	// Unexpose removes that record; a missing record is not an error.
	Unexpose(ctx context.Context, fsID, bucketID string) error
	// MutateExposed is Mutate whose fn also gets the IDs of the buckets
	// exposing the filesystem, read in the same transaction.
	MutateExposed(ctx context.Context, id string, fn func(fs *computepb.Filesystem, exposedBy []string) error) (*computepb.Filesystem, error)
}

// FilesystemAttachments records which instances a shared filesystem is
// attached to (FilesystemStatus.read_write_attachments). The instance service
// calls it on instance Create/Update/Delete; it enforces that the instance is
// stopped and that mount tags are unique. Satisfied by *FilesystemAttach.
type FilesystemAttachments interface {
	// Attach atomically checks that the filesystem exists (errs.NotFound),
	// is in projectID (errs.BadRequest), is READY (errs.BadState) and that
	// mode is READ_WRITE, the only mode Nebius supports (READ_ONLY and
	// UNSPECIFIED are errs.BadRequest), then adds instanceID to
	// read_write_attachments. Attaching the same instance twice is a no-op.
	Attach(ctx context.Context, fsID, instanceID, projectID string, mode computepb.AttachedFilesystemSpec_AttachMode) error
	// Detach removes instanceID from the filesystem's attachments. A missing
	// filesystem or attachment is not an error.
	Detach(ctx context.Context, fsID, instanceID string) error
}

// FilesystemData is the data plane of shared filesystems: the object-store
// bucket that holds a filesystem's files, one object per file. Satisfied by
// backend/fsstore.
type FilesystemData interface {
	// Bucket returns the name of the bucket that holds the filesystem's files.
	Bucket(fsID string) string
	// Provision creates the filesystem's bucket. It is idempotent.
	Provision(ctx context.Context, fsID string) error
	// Purge deletes the bucket and every object in it. A missing bucket is
	// not an error.
	Purge(ctx context.Context, fsID string) error
}

// FilesystemExposures records which Object Storage filesystem buckets expose
// a filesystem (the storage module calls it through peer.Filesystems on
// bucket create and delete). Satisfied by *FilesystemAttach.
type FilesystemExposures interface {
	// Expose atomically checks that the filesystem exists (errs.NotFound),
	// is in projectID (errs.BadRequest) and is READY or UPDATING
	// (errs.BadState), then records bucketID. FilesystemService.Delete checks
	// the records in the transaction that marks the filesystem DELETING, so
	// an Expose and a Delete never both succeed.
	Expose(ctx context.Context, fsID, bucketID, projectID string) error
	// Unexpose removes the record; missing is not an error.
	Unexpose(ctx context.Context, fsID, bucketID string) error
}
