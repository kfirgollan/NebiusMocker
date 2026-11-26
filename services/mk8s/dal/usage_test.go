package dal_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/dal"
)

func TestQuotaUsageCountsClustersPerProject(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	clusters := dal.NewClusterRepo(st)
	for _, c := range []struct{ id, project string }{
		{"mk8scluster-1", "project-a"}, {"mk8scluster-2", "project-a"}, {"mk8scluster-3", "project-b"},
	} {
		if err := clusters.Create(t.Context(), cluster(c.id, c.project, c.id)); err != nil {
			t.Fatal(err)
		}
	}
	// Node groups are not counted: compute counts their instances.
	if err := dal.NewNodeGroupRepo(st).Create(t.Context(), group("mk8snodegroup-1", "mk8scluster-1", "g")); err != nil {
		t.Fatal(err)
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
		"project-a": {ContainerID: "project-a", Name: quota.MK8sClusterCount, Amount: 2},
		"project-b": {ContainerID: "project-b", Name: quota.MK8sClusterCount, Amount: 1},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("usage mismatch (-want +got):\n%s", diff)
	}
}
