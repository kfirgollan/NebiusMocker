package domain

import (
	"context"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

var _ peer.Buckets = (*BucketService)(nil)

// BucketByName implements peer.Buckets: the bucket named name in the project
// if it is usable (not scheduled for deletion), else errs.NotFound.
func (s *BucketService) BucketByName(ctx context.Context, projectID, name string) (peer.Bucket, error) {
	b, err := s.buckets.GetByName(ctx, projectID, name)
	if err != nil {
		return peer.Bucket{}, err
	}
	if !usable(b) {
		return peer.Bucket{}, errs.NotFound(name)
	}
	return peerBucket(b), nil
}

// CreateBucket implements peer.Buckets: a bucket with default settings,
// created exactly as an S3 CreateBucket call creates one (no IAM check: the
// calling service authorized its own request).
func (s *BucketService) CreateBucket(ctx context.Context, projectID, name string) (peer.Bucket, error) {
	b, err := s.CreateFromS3(ctx, projectID, name)
	if err != nil {
		return peer.Bucket{}, err
	}
	return peerBucket(b), nil
}

func peerBucket(b *storagepb.Bucket) peer.Bucket {
	md := b.GetMetadata()
	return peer.Bucket{ID: md.GetId(), ProjectID: md.GetParentId(), Name: md.GetName()}
}
