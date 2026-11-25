package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

// Object metadata keys written on every image blob (lower-case: S3 user
// metadata keys are case-insensitive and come back lower-cased).
const (
	MetaSHA256      = "sha256"
	MetaFormat      = "format"
	MetaVirtualSize = "virtual-size"
	MetaImageID     = "image-id"
	MetaSource      = "source"
)

// importPayload copies the source object into the image store and derives the
// image status from the bytes: size, checksum, format and minimum disk size.
// A failure marks the image ERROR and fails the operation.
func (s *ImageService) importPayload(ctx context.Context, id string, src *computepb.ImageSpec_SourceStorage) error {
	in, err := s.copySource(ctx, id, src)
	if err != nil {
		if serr := s.setState(ctx, id, computepb.ImageStatus_ERROR, err.Error()); serr != nil {
			return errors.Join(err, serr)
		}
		return err
	}
	return s.markReady(ctx, id, in)
}

// markReady sets the READY status derived from the stored payload.
func (s *ImageService) markReady(ctx context.Context, id string, in Inspection) error {
	img, err := s.images.Get(ctx, id)
	if err != nil {
		return err
	}
	img.Status = &computepb.ImageStatus{
		State:            computepb.ImageStatus_READY,
		StorageSizeBytes: in.Size,
		MinDiskSizeBytes: in.MinDiskSizeBytes(),
	}
	return s.images.Update(ctx, img)
}

// copySource reads the source twice: once to hash and inspect it (so the blob
// can be written with its checksum as metadata), once to store it. The second
// pass is hashed too, so a source that changes mid-import is detected.
func (s *ImageService) copySource(ctx context.Context, id string, src *computepb.ImageSpec_SourceStorage) (Inspection, error) {
	bucket, object := src.GetBucketName(), src.GetObjectName()
	first, err := s.inspectSource(ctx, bucket, object)
	if err != nil {
		return Inspection{}, err
	}
	r, _, err := s.blobs.OpenSource(ctx, bucket, object)
	if err != nil {
		return Inspection{}, fmt.Errorf("open source s3://%s/%s: %w", bucket, object, err)
	}
	defer func() { _ = r.Close() }() // read-only stream
	verify := NewInspector()
	meta := blobMetadata(id, first)
	meta[MetaSource] = "s3://" + bucket + "/" + object
	info, err := s.blobs.PutImage(ctx, id, io.TeeReader(r, verify), first.Size, meta)
	if err != nil {
		return Inspection{}, fmt.Errorf("store payload of %s: %w", id, err)
	}
	if got := verify.Result(); got.SHA256 != first.SHA256 || info.Size != first.Size {
		_ = s.blobs.DeleteImage(ctx, id) // best-effort: the image fails anyway
		return Inspection{}, fmt.Errorf("source s3://%s/%s changed during import", bucket, object)
	}
	return first, nil
}

func (s *ImageService) inspectSource(ctx context.Context, bucket, object string) (Inspection, error) {
	r, _, err := s.blobs.OpenSource(ctx, bucket, object)
	if err != nil {
		return Inspection{}, fmt.Errorf("open source s3://%s/%s: %w", bucket, object, err)
	}
	defer func() { _ = r.Close() }() // read-only stream
	insp := NewInspector()
	if _, err := io.Copy(insp, r); err != nil {
		return Inspection{}, fmt.Errorf("read source s3://%s/%s: %w", bucket, object, err)
	}
	return insp.Result(), nil
}

func blobMetadata(id string, in Inspection) map[string]string {
	return map[string]string{
		MetaSHA256:      in.SHA256,
		MetaFormat:      string(in.Format),
		MetaVirtualSize: strconv.FormatInt(in.VirtualSize, 10),
		MetaImageID:     id,
	}
}
