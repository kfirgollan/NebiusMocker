package dal

import (
	"context"
	"errors"
	"fmt"

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

// kindFilesystemExposure is the store kind of "bucket exposes filesystem"
// records (not a NID type): parent = filesystem ID, ID = "<fs>/<bucket>".
const kindFilesystemExposure = "computefilesystem.exposure"

func exposureID(fsID, bucketID string) string { return fsID + "/" + bucketID }

// Expose implements domain.FilesystemRepository: check and the record write
// run in one store transaction.
func (f *FilesystemRepo) Expose(ctx context.Context, fsID, bucketID string, check func(*computepb.Filesystem) error) error {
	err := f.r.st.Update(ctx, func(tx store.Tx) error {
		rec, err := tx.Get(f.r.kind, fsID)
		if err != nil {
			return err
		}
		fs, err := f.r.decode(rec)
		if err != nil {
			return err
		}
		if err := check(fs); err != nil {
			return err
		}
		_, err = tx.Put(store.Record{Kind: kindFilesystemExposure, ID: exposureID(fsID, bucketID), ParentID: fsID, Data: []byte(bucketID)})
		return err
	})
	return f.r.toDomain(err, fsID)
}

// Unexpose implements domain.FilesystemRepository.
func (f *FilesystemRepo) Unexpose(ctx context.Context, fsID, bucketID string) error {
	err := f.r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(kindFilesystemExposure, exposureID(fsID, bucketID)) })
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return f.r.toDomain(err, fsID)
}

// MutateExposed implements domain.FilesystemRepository: Mutate whose fn also
// sees the IDs of the buckets exposing the filesystem, read in the same
// transaction.
func (f *FilesystemRepo) MutateExposed(ctx context.Context, id string, fn func(*computepb.Filesystem, []string) error) (*computepb.Filesystem, error) {
	var out *computepb.Filesystem
	err := f.r.st.Update(ctx, func(tx store.Tx) error {
		rec, err := tx.Get(f.r.kind, id)
		if err != nil {
			return err
		}
		fs, err := f.r.decode(rec)
		if err != nil {
			return err
		}
		exposures, err := tx.List(store.ListQuery{Kind: kindFilesystemExposure, ParentID: id})
		if err != nil {
			return err
		}
		buckets := make([]string, 0, len(exposures))
		for _, e := range exposures {
			buckets = append(buckets, string(e.Data))
		}
		if err := fn(fs, buckets); err != nil {
			return err
		}
		data, err := marshal.Marshal(fs)
		if err != nil {
			return fmt.Errorf("encode filesystem %s: %w", id, err)
		}
		md := fs.GetMetadata()
		if _, err := tx.Put(store.Record{Kind: f.r.kind, ID: id, ParentID: md.GetParentId(), Name: md.GetName(), Data: data}); err != nil {
			return err
		}
		out = fs
		return nil
	})
	if err != nil {
		return nil, f.r.toDomain(err, id)
	}
	return out, nil
}
