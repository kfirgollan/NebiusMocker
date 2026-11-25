package domain_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

var baseTime = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

func counters(cur *storagepb.CurrentBucketCounters, non *storagepb.NonCurrentBucketCounters, c storagepb.StorageClass) *storagepb.BucketCounters {
	if cur == nil {
		cur = &storagepb.CurrentBucketCounters{}
	}
	if non == nil {
		non = &storagepb.NonCurrentBucketCounters{}
	}
	return &storagepb.BucketCounters{StorageClass: c, Counters: cur, NonCurrentCounters: non}
}

func TestComputeCounters(t *testing.T) {
	mixed := newContentFake(true).
		put("a", "a1", baseTime, 10, "etag", "").
		put("a", "a2", baseTime, 20, "etag", "STANDARD").
		put("b", "b1", baseTime, 100, "etag-3", "STANDARD").
		put("b", "b2", baseTime, 200, "etag-4", "STANDARD").
		put("c", "c1", baseTime, 5, "etag", "REDUCED_REDUNDANCY").
		put("d", "d1", baseTime, 7, "etag", "").
		putMarker("d", "d2", baseTime)
	mixed.uploads = []objstore.Upload{
		{Key: "u1", UploadID: "1", Parts: 2, PartsSize: 30},
		{Key: "u2", UploadID: "2", StorageClass: "REDUCED_REDUNDANCY", Parts: 1, PartsSize: 4},
		{Key: "u3", UploadID: "3", Parts: 1, PartsSize: 6},
	}

	tests := []struct {
		name    string
		backend *contentFake
		want    []*storagepb.BucketCounters
		used    int64
	}{
		{
			name:    "empty bucket has a zero STANDARD entry",
			backend: newContentFake(false),
			want:    []*storagepb.BucketCounters{counters(nil, nil, storagepb.StorageClass_STANDARD)},
		},
		{
			name:    "mixed simple, multipart, noncurrent, markers, uploads and two classes",
			backend: mixed,
			want: []*storagepb.BucketCounters{
				counters(
					&storagepb.CurrentBucketCounters{
						SimpleObjectsQuantity: 1, SimpleObjectsSize: 20,
						MultipartObjectsQuantity: 1, MultipartObjectsSize: 200,
						MultipartUploadsQuantity: 2, InflightPartsQuantity: 3, InflightPartsSize: 36,
					},
					&storagepb.NonCurrentBucketCounters{
						SimpleObjectsQuantity: 2, SimpleObjectsSize: 17, // a1 and d1 (behind a delete marker)
						MultipartObjectsQuantity: 1, MultipartObjectsSize: 100,
					},
					storagepb.StorageClass_STANDARD),
				counters(
					&storagepb.CurrentBucketCounters{
						SimpleObjectsQuantity: 1, SimpleObjectsSize: 5,
						MultipartUploadsQuantity: 1, InflightPartsQuantity: 1, InflightPartsSize: 4,
					},
					nil, storagepb.StorageClass_ENHANCED_THROUGHPUT),
			},
			used: 20 + 200 + 36 + 17 + 100 + 5 + 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domain.ComputeCounters(t.Context(), tt.backend, "bkt")
			if err != nil {
				t.Fatalf("ComputeCounters: %v", err)
			}
			if diff := cmp.Diff(tt.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("counters mismatch (-want +got):\n%s", diff)
			}
			if used := domain.UsedBytes(got); used != tt.used {
				t.Errorf("UsedBytes = %d, want %d", used, tt.used)
			}
		})
	}
}
