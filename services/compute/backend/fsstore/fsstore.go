// Package fsstore implements the compute domain's FilesystemData port over
// the shared object store (pkg/objstore: RustFS, or memory in tests).
//
// Every shared filesystem owns one bucket, "nebius-mocker-fs-<filesystem
// id>", holding one object per file keyed by its path without the leading
// "/". The "nebius-mocker-" prefix is reserved for internal buckets, so a
// user's Object Storage bucket can never collide with one.
package fsstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// bucketPrefix starts the bucket of every filesystem.
const bucketPrefix = "nebius-mocker-fs-"

// Store implements domain.FilesystemData.
type Store struct{ st objstore.Store }

var _ domain.FilesystemData = (*Store)(nil)

// New returns a Store over st.
func New(st objstore.Store) *Store { return &Store{st: st} }

// Bucket implements domain.FilesystemData.
func (s *Store) Bucket(fsID string) string { return bucketPrefix + fsID }

// Provision implements domain.FilesystemData.
func (s *Store) Provision(ctx context.Context, fsID string) error {
	if err := s.st.EnsureBucket(ctx, s.Bucket(fsID)); err != nil {
		return toDomain(err, fsID)
	}
	return nil
}

// Purge implements domain.FilesystemData.
func (s *Store) Purge(ctx context.Context, fsID string) error {
	if err := s.st.DeleteBucket(ctx, s.Bucket(fsID)); err != nil {
		return toDomain(err, fsID)
	}
	return nil
}

func toDomain(err error, ref string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrBucketNotFound):
		return errs.NotFound(ref)
	default:
		return fmt.Errorf("filesystem %s data: %w", ref, err)
	}
}
