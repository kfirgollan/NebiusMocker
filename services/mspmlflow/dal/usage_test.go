package dal_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/dal"
)

func TestQuotaUsageSumsAmountsPerProject(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	clusters := dal.NewClusterRepo(st)
	for _, c := range []*mlflowpb.Cluster{cluster("mlflow-1", project, "a"), cluster("mlflow-2", project, "b"), cluster("mlflow-3", project2, "c")} {
		if err := clusters.Create(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	amounts := func(c *mlflowpb.Cluster) map[string]int64 {
		return map[string]int64{"clusters": 1, "size." + c.GetSpec().GetSize(): 8}
	}
	got, err := dal.NewQuotaUsage(st, amounts).QuotaUsage(t.Context())
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	want := []peer.QuotaUsage{
		{ContainerID: project, Name: "clusters", Amount: 2},
		{ContainerID: project, Name: "size.medium", Amount: 16},
		{ContainerID: project2, Name: "clusters", Amount: 1},
		{ContainerID: project2, Name: "size.medium", Amount: 8},
	}
	less := func(a, b peer.QuotaUsage) bool { return a.ContainerID+a.Name < b.ContainerID+b.Name }
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(less)); diff != "" {
		t.Fatalf("usage mismatch (-want +got):\n%s", diff)
	}
}
