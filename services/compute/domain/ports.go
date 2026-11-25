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

// ImageLookup resolves disk sources from images (satisfied by *ImageService).
type ImageLookup interface {
	Get(ctx context.Context, id string) (*computepb.Image, error)
	GetLatestByFamily(ctx context.Context, family, parentID string) (*computepb.Image, error)
	IsPublicProject(id string) bool
}
