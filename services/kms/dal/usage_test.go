package dal_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/kms/dal"
)

func TestQuotaUsageCountsEveryKeyPerProject(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	sym, asym := dal.NewSymmetricKeyRepo(st), dal.NewAsymmetricKeyRepo(st)
	ctx := t.Context()
	type usageKey struct{ project, quota string }
	mustCreate := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []struct {
		id, project string
		state       kmspb.KeyState
	}{
		{"s1", "project-a", kmspb.KeyState_ACTIVE},
		{"s2", "project-a", kmspb.KeyState_SCHEDULED_FOR_DELETION}, // still counts
		{"s3", "project-b", kmspb.KeyState_ACTIVE},
	} {
		key := symKey(k.id, k.id, k.state)
		key.Metadata.ParentId = k.project
		mustCreate(sym.Create(ctx, key, []byte("m")))
	}
	a := asymKey("a1", "a1", kmspb.KeyState_SCHEDULED_FOR_DELETION) // counts too
	a.Metadata.ParentId = "project-b"
	mustCreate(asym.Create(ctx, a, []byte("m")))

	got, err := dal.NewQuotaUsage(st).QuotaUsage(ctx)
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	counts := map[usageKey]uint64{}
	for _, u := range got {
		counts[usageKey{u.ContainerID, u.Name}] = u.Amount
	}
	want := map[usageKey]uint64{
		{"project-a", quota.KMSSymmetricKeyCount}:  2,
		{"project-b", quota.KMSSymmetricKeyCount}:  1,
		{"project-b", quota.KMSAsymmetricKeyCount}: 1,
	}
	if diff := cmp.Diff(want, counts); diff != "" {
		t.Fatalf("usage mismatch (-want +got):\n%s", diff)
	}
}
