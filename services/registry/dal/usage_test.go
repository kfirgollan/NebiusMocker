package dal

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

func TestQuotaUsageCountsRegistriesPerProject(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	repo := NewRegistryRepo(st)
	for _, r := range []struct{ id, project string }{
		{"registry-1", "project-a"}, {"registry-2", "project-a"}, {"registry-3", "project-b"},
	} {
		reg := &registrypb.Registry{Metadata: &commonpb.ResourceMetadata{Id: r.id, ParentId: r.project, Name: r.id}}
		if err := repo.Create(t.Context(), reg); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]peer.QuotaUsage{}
	usage, err := NewQuotaUsage(st).QuotaUsage(t.Context())
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	for _, u := range usage {
		got[u.ContainerID] = u
	}
	want := map[string]peer.QuotaUsage{
		"project-a": {ContainerID: "project-a", Name: quota.RegistryCount, Amount: 2},
		"project-b": {ContainerID: "project-b", Name: quota.RegistryCount, Amount: 1},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("usage mismatch (-want +got):\n%s", diff)
	}
}
