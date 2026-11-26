package dal_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/dal"
)

func TestQuotaUsageCountsEverySecretPerProject(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	repo := dal.NewSecretRepo(st)
	for _, s := range []struct {
		id, project string
		state       mysteryboxpb.SecretStatus_State
	}{
		{"s1", "project-a", mysteryboxpb.SecretStatus_ACTIVE},
		{"s2", "project-a", mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION}, // still counts
		{"s3", "project-b", mysteryboxpb.SecretStatus_ACTIVE},
	} {
		sec := secret(s.id, s.id, s.state)
		sec.Metadata.ParentId = s.project
		if err := repo.Create(t.Context(), sec); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := dal.NewQuotaUsage(st).QuotaUsage(t.Context())
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	got := map[string]peer.QuotaUsage{}
	for _, u := range usage {
		got[u.ContainerID] = u
	}
	want := map[string]peer.QuotaUsage{
		"project-a": {ContainerID: "project-a", Name: quota.MysteryBoxSecretCount, Amount: 2},
		"project-b": {ContainerID: "project-b", Name: quota.MysteryBoxSecretCount, Amount: 1},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("usage mismatch (-want +got):\n%s", diff)
	}
}
