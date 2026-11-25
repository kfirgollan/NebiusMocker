package dal

import (
	"context"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// FilesystemRepo stores shared filesystems.
type FilesystemRepo struct {
	r protoRepo[*computepb.Filesystem]
}

var _ domain.FilesystemRepository = (*FilesystemRepo)(nil)

// NewFilesystemRepo returns a FilesystemRepo over st.
func NewFilesystemRepo(st store.Store) *FilesystemRepo {
	return &FilesystemRepo{r: protoRepo[*computepb.Filesystem]{
		st: st, kind: domain.KindFilesystem, noun: "filesystem", newT: func() *computepb.Filesystem { return &computepb.Filesystem{} },
	}}
}

// Get returns the filesystem or errs.NotFound.
func (f *FilesystemRepo) Get(ctx context.Context, id string) (*computepb.Filesystem, error) {
	return f.r.get(ctx, id)
}

// GetByName returns the filesystem named name in parentID or errs.NotFound.
func (f *FilesystemRepo) GetByName(ctx context.Context, parentID, name string) (*computepb.Filesystem, error) {
	return f.r.getByName(ctx, parentID, name)
}

// List returns one page of a parent's filesystems in creation order.
func (f *FilesystemRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.FilesystemPage, error) {
	items, next, err := f.r.list(ctx, parentID, pageSize, pageToken, nil)
	return domain.FilesystemPage{Items: items, NextPageToken: next}, err
}

// Create inserts a new filesystem; the name must be unique in its parent.
func (f *FilesystemRepo) Create(ctx context.Context, fs *computepb.Filesystem) error {
	return f.r.create(ctx, fs)
}

// Mutate implements domain.FilesystemRepository.
func (f *FilesystemRepo) Mutate(ctx context.Context, id string, fn func(*computepb.Filesystem) error) (*computepb.Filesystem, error) {
	return f.r.mutate(ctx, id, fn)
}

// Delete removes the filesystem or returns errs.NotFound.
func (f *FilesystemRepo) Delete(ctx context.Context, id string) error { return f.r.delete(ctx, id) }
