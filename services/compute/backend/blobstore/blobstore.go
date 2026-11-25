// Package blobstore implements the compute domain's Blobs port over the
// shared object store (pkg/objstore: RustFS, or memory in tests).
//
// Image payloads live in one compute-owned bucket (Bucket) under
// "images/<image-id>". Import sources are read from users' Object Storage
// buckets, whose names map 1:1 to object-store buckets.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// Bucket holds every image payload. The "nebius-mocker-" prefix is reserved
// for internal buckets: Object Storage rejects it for user buckets.
const Bucket = "nebius-mocker-compute-images"

const contentType = "application/octet-stream"

// Blobs stores image payloads in an object store.
type Blobs struct {
	st objstore.Store
}

var _ domain.Blobs = (*Blobs)(nil)

// New returns Blobs over st, creating the image bucket if needed.
func New(ctx context.Context, st objstore.Store) (*Blobs, error) {
	if st == nil {
		return nil, errors.New("compute: object store is required")
	}
	if err := st.EnsureBucket(ctx, Bucket); err != nil {
		return nil, fmt.Errorf("compute: ensure bucket %s: %w", Bucket, err)
	}
	return &Blobs{st: st}, nil
}

// Key is the object key of an image's payload.
func Key(imageID string) string { return "images/" + imageID }

// PutImage implements domain.Blobs.
func (b *Blobs) PutImage(ctx context.Context, imageID string, r io.Reader, size int64, meta map[string]string) (domain.BlobInfo, error) {
	info, err := b.st.Put(ctx, Bucket, Key(imageID), r, size, objstore.PutOptions{ContentType: contentType, Metadata: maps.Clone(meta)})
	if err != nil {
		return domain.BlobInfo{}, toDomain(err, Bucket+"/"+Key(imageID))
	}
	return blobInfo(info), nil
}

// StatImage implements domain.Blobs.
func (b *Blobs) StatImage(ctx context.Context, imageID string) (domain.BlobInfo, error) {
	info, err := b.st.Stat(ctx, Bucket, Key(imageID))
	if err != nil {
		return domain.BlobInfo{}, toDomain(err, Bucket+"/"+Key(imageID))
	}
	return blobInfo(info), nil
}

// DeleteImage implements domain.Blobs.
func (b *Blobs) DeleteImage(ctx context.Context, imageID string) error {
	return toDomain(b.st.Delete(ctx, Bucket, Key(imageID)), Bucket+"/"+Key(imageID))
}

// OpenSource implements domain.Blobs.
func (b *Blobs) OpenSource(ctx context.Context, bucket, object string) (io.ReadCloser, domain.BlobInfo, error) {
	r, info, err := b.st.Get(ctx, bucket, object)
	if err != nil {
		return nil, domain.BlobInfo{}, toDomain(err, bucket+"/"+object)
	}
	return r, blobInfo(info), nil
}

// StatSource implements domain.Blobs.
func (b *Blobs) StatSource(ctx context.Context, bucket, object string) (domain.BlobInfo, error) {
	info, err := b.st.Stat(ctx, bucket, object)
	if err != nil {
		return domain.BlobInfo{}, toDomain(err, bucket+"/"+object)
	}
	return blobInfo(info), nil
}

func blobInfo(info objstore.ObjectInfo) domain.BlobInfo {
	return domain.BlobInfo{Size: info.Size, Metadata: maps.Clone(info.Metadata)}
}

func toDomain(err error, ref string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrBucketNotFound):
		return errs.NotFound(ref)
	default:
		return err
	}
}
