package dal_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/dal"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func TestQuotaUsageCountsBucketsAndTransfersPerProject(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	ctx := t.Context()
	buckets, transfers := dal.NewBucketRepo(st), dal.NewTransferRepo(st)

	soft := bucket("b3", "project-a", "soft")
	soft.Status.State = storagepb.BucketStatus_SCHEDULED_FOR_DELETION // still counts until purged
	for _, b := range []*storagepb.Bucket{bucket("b1", "project-a", "one"), bucket("b2", "project-b", "two"), soft} {
		if err := buckets.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// One v1 and one v1alpha1 transfer share the transfer quota.
	for _, rec := range []*domain.TransferRecord{trV1("t1", "x"), trAlpha("t2", "project-a", "y"), trAlpha("t3", "project-b", "z")} {
		if err := transfers.Create(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	usage, err := dal.NewQuotaUsage(st).QuotaUsage(ctx)
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	type key struct{ project, quota string }
	got := map[key]uint64{}
	for _, u := range usage {
		got[key{u.ContainerID, u.Name}] = u.Amount
	}
	want := map[key]uint64{
		{"project-a", quota.StorageBucketCount}:   2,
		{"project-b", quota.StorageBucketCount}:   1,
		{"project-a", quota.StorageTransferCount}: 2,
		{"project-b", quota.StorageTransferCount}: 1,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("usage mismatch (-want +got):\n%s", diff)
	}
}
