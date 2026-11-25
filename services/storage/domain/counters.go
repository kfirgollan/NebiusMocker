package domain

import (
	"context"
	"fmt"
	"slices"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
)

// ComputeCounters returns the bucket's per-storage-class counters. The
// STANDARD entry is always present; other classes appear only when they hold
// data. The result is sorted by storage class. A missing bucket yields an
// error wrapping objstore.ErrBucketNotFound.
func ComputeCounters(ctx context.Context, backend ContentBackend, bucket string) ([]*storagepb.BucketCounters, error) {
	byClass := map[storagepb.StorageClass]*storagepb.BucketCounters{}
	entry := func(backendClass string) *storagepb.BucketCounters {
		c := NebiusClass(backendClass)
		if e, ok := byClass[c]; ok {
			return e
		}
		e := &storagepb.BucketCounters{
			StorageClass:       c,
			Counters:           &storagepb.CurrentBucketCounters{},
			NonCurrentCounters: &storagepb.NonCurrentBucketCounters{},
		}
		byClass[c] = e
		return e
	}
	entry(backendStandard)

	for v, err := range backend.ListVersions(ctx, bucket, "") {
		if err != nil {
			return nil, fmt.Errorf("list versions of bucket %q: %w", bucket, err)
		}
		if v.DeleteMarker {
			continue
		}
		e := entry(v.StorageClass)
		switch {
		case v.IsLatest && v.Multipart():
			e.Counters.MultipartObjectsQuantity++
			e.Counters.MultipartObjectsSize += v.Size
		case v.IsLatest:
			e.Counters.SimpleObjectsQuantity++
			e.Counters.SimpleObjectsSize += v.Size
		case v.Multipart():
			e.NonCurrentCounters.MultipartObjectsQuantity++
			e.NonCurrentCounters.MultipartObjectsSize += v.Size
		default:
			e.NonCurrentCounters.SimpleObjectsQuantity++
			e.NonCurrentCounters.SimpleObjectsSize += v.Size
		}
	}
	for u, err := range backend.ListUploads(ctx, bucket) {
		if err != nil {
			return nil, fmt.Errorf("list uploads of bucket %q: %w", bucket, err)
		}
		e := entry(u.StorageClass)
		e.Counters.MultipartUploadsQuantity++
		e.Counters.InflightPartsQuantity += u.Parts
		e.Counters.InflightPartsSize += u.PartsSize
	}

	out := make([]*storagepb.BucketCounters, 0, len(byClass))
	for _, e := range byClass {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b *storagepb.BucketCounters) int {
		return int(a.GetStorageClass()) - int(b.GetStorageClass())
	})
	return out, nil
}

// UsedBytes is the storage a bucket consumes for size-limit purposes: current
// and noncurrent object bytes plus the bytes of in-flight multipart parts.
func UsedBytes(cs []*storagepb.BucketCounters) int64 {
	var n int64
	for _, c := range cs {
		cur, non := c.GetCounters(), c.GetNonCurrentCounters()
		n += cur.GetSimpleObjectsSize() + cur.GetMultipartObjectsSize() + cur.GetInflightPartsSize()
		n += non.GetSimpleObjectsSize() + non.GetMultipartObjectsSize()
	}
	return n
}
