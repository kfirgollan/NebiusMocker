package dal

import (
	"context"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// DiskRepo stores compute disks.
type DiskRepo struct{ r protoRepo[*computepb.Disk] }

var _ domain.DiskRepository = (*DiskRepo)(nil)

// NewDiskRepo returns a DiskRepo over st.
func NewDiskRepo(st store.Store) *DiskRepo {
	return &DiskRepo{r: protoRepo[*computepb.Disk]{
		st: st, kind: domain.KindDisk, noun: "disk", newT: func() *computepb.Disk { return &computepb.Disk{} },
	}}
}

// Get returns the disk or errs.NotFound.
func (d *DiskRepo) Get(ctx context.Context, id string) (*computepb.Disk, error) {
	return d.r.get(ctx, id)
}

// GetByName returns the disk named name in parentID or errs.NotFound.
func (d *DiskRepo) GetByName(ctx context.Context, parentID, name string) (*computepb.Disk, error) {
	return d.r.getByName(ctx, parentID, name)
}

// List returns one page of a parent's disks in creation order.
func (d *DiskRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.DiskPage, error) {
	items, next, err := d.r.list(ctx, parentID, pageSize, pageToken, nil)
	return domain.DiskPage{Items: items, NextPageToken: next}, err
}

// Create inserts a new disk; the name must be unique in its parent.
func (d *DiskRepo) Create(ctx context.Context, disk *computepb.Disk) error {
	return d.r.create(ctx, disk)
}

// Mutate implements domain.DiskRepository.
func (d *DiskRepo) Mutate(ctx context.Context, id string, fn func(*computepb.Disk) error) (*computepb.Disk, error) {
	return d.r.mutate(ctx, id, fn)
}

// Delete removes the disk or returns errs.NotFound.
func (d *DiskRepo) Delete(ctx context.Context, id string) error { return d.r.delete(ctx, id) }

// DiskReader gives other compute resources read access to disks and snapshots.
type DiskReader struct {
	disks     *DiskRepo
	snapshots *SnapshotRepo
}

var _ domain.DiskReader = (*DiskReader)(nil)

// NewDiskReader returns a DiskReader over st.
func NewDiskReader(st store.Store) *DiskReader {
	return &DiskReader{disks: NewDiskRepo(st), snapshots: NewSnapshotRepo(st)}
}

// Disk returns the disk or errs.NotFound.
func (r *DiskReader) Disk(ctx context.Context, id string) (*computepb.Disk, error) {
	return r.disks.Get(ctx, id)
}

// Snapshot returns the disk snapshot or errs.NotFound.
func (r *DiskReader) Snapshot(ctx context.Context, id string) (*computepb.DiskSnapshot, error) {
	return r.snapshots.Get(ctx, id)
}
