package dal

import (
	"slices"
	"sync"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

func gpuCluster(id, name string) *computepb.GpuCluster {
	return &computepb.GpuCluster{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-3"},
		Status:   &computepb.GpuClusterStatus{},
	}
}

func testFabrics(t *testing.T) domain.FabricCatalog {
	t.Helper()
	c, err := domain.ParseFabricCatalog([]byte(`fabrics:
  - {name: fabric-3, region: eu-north1, platforms: [gpu-h100-sxm]}
`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func emptyCheck(c *computepb.GpuCluster) error {
	if len(c.GetStatus().GetInstances()) > 0 {
		return errs.BadState(c.GetMetadata().GetId(), "not empty")
	}
	return nil
}

func TestGpuClusterRepoRoundTripAndDeleteIf(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewGpuClusterRepo(store.NewMemory())
	if err := r.Create(ctx, gpuCluster("computegpucluster-a", "a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, gpuCluster("computegpucluster-b", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := r.Create(ctx, gpuCluster("computegpucluster-b", "b")); err != nil {
		t.Fatal(err)
	}
	byName, err := r.GetByName(ctx, parent, "b")
	if err != nil || byName.GetMetadata().GetId() != "computegpucluster-b" {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	page, err := r.List(ctx, parent, 1, "")
	if err != nil || len(page.Items) != 1 || page.NextPageToken == "" {
		t.Fatalf("first page = %v, %v", page, err)
	}
	page, err = r.List(ctx, parent, 1, page.NextPageToken)
	if err != nil || len(page.Items) != 1 || page.Items[0].GetMetadata().GetName() != "b" || page.NextPageToken != "" {
		t.Fatalf("second page = %v, %v", page, err)
	}

	if _, err := r.Mutate(ctx, "computegpucluster-a", func(c *computepb.GpuCluster) error {
		c.Status.Instances = []string{"computeinstance-x"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteIf(ctx, "computegpucluster-a", emptyCheck); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("DeleteIf on a non-empty cluster: %v", err)
	}
	if _, err := r.Get(ctx, "computegpucluster-a"); err != nil {
		t.Fatalf("aborted DeleteIf removed the cluster: %v", err)
	}
	if err := r.DeleteIf(ctx, "computegpucluster-b", emptyCheck); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteIf(ctx, "computegpucluster-b", emptyCheck); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("DeleteIf of a missing cluster: %v", err)
	}
}

func TestGpuClusterMembersJoinLeave(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewGpuClusterRepo(store.NewMemory())
	if err := r.Create(ctx, gpuCluster("computegpucluster-a", "a")); err != nil {
		t.Fatal(err)
	}
	m, err := domain.NewGpuClusterMembers(r, testFabrics(t))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, cluster, project, platform string
		want                             errs.Kind
	}{
		{"missing cluster", "computegpucluster-missing", parent, "gpu-h100-sxm", errs.KindNotFound},
		{"other project", "computegpucluster-a", "project-e00other", "gpu-h100-sxm", errs.KindBadRequest},
		{"wrong platform", "computegpucluster-a", parent, "gpu-h200-sxm", errs.KindBadRequest},
		{"no cluster id", "", parent, "gpu-h100-sxm", errs.KindBadRequest},
	} {
		if _, err := m.Join(ctx, tc.cluster, "computeinstance-x", tc.project, tc.platform); !errs.IsKind(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}

	path, err := m.Join(ctx, "computegpucluster-a", "computeinstance-x", parent, "gpu-h100-sxm")
	if err != nil {
		t.Fatal(err)
	}
	if want := domain.TopologyPath("fabric-3", "computeinstance-x"); !slices.Equal(path, want) || len(path) != 3 || path[0] != "fabric-3" {
		t.Fatalf("path = %v, want %v", path, want)
	}
	again, err := m.Join(ctx, "computegpucluster-a", "computeinstance-x", parent, "gpu-h100-sxm")
	if err != nil || !slices.Equal(again, path) {
		t.Fatalf("second Join = %v, %v", again, err)
	}
	if _, err := m.Join(ctx, "computegpucluster-a", "computeinstance-y", parent, "gpu-h100-sxm"); err != nil {
		t.Fatal(err)
	}
	c, err := r.Get(ctx, "computegpucluster-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.GetStatus().GetInstances(); !slices.Equal(got, []string{"computeinstance-x", "computeinstance-y"}) {
		t.Fatalf("instances = %v", got)
	}
	if topo := c.GetStatus().GetInfinibandTopologyPath().GetInstances(); len(topo) != 2 || topo[0].GetInstanceId() != "computeinstance-x" ||
		!slices.Equal(topo[0].GetPath(), path) {
		t.Fatalf("topology = %v", topo)
	}

	for _, id := range []string{"computeinstance-x", "computeinstance-x", "computeinstance-y"} {
		if err := m.Leave(ctx, "computegpucluster-a", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Leave(ctx, "computegpucluster-missing", "computeinstance-x"); err != nil {
		t.Fatalf("Leave of a missing cluster: %v", err)
	}
	c, err = r.Get(ctx, "computegpucluster-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.GetStatus().GetInstances()) != 0 || c.GetStatus().GetTopology() != nil {
		t.Fatalf("status after leaving = %v", c.GetStatus())
	}
}

// TestGpuClusterJoinVersusDelete races joins against a delete that requires
// an empty cluster: either the delete wins and every later join is NotFound,
// or a join wins and the delete is refused. Never both.
func TestGpuClusterJoinVersusDelete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for round := range 50 {
		r := NewGpuClusterRepo(store.NewMemory())
		if err := r.Create(ctx, gpuCluster("computegpucluster-a", "a")); err != nil {
			t.Fatal(err)
		}
		m, err := domain.NewGpuClusterMembers(r, testFabrics(t))
		if err != nil {
			t.Fatal(err)
		}
		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			joined int
			delErr error
		)
		for i := range 4 {
			wg.Go(func() {
				_, err := m.Join(ctx, "computegpucluster-a", "computeinstance-"+string(rune('a'+i)), parent, "gpu-h100-sxm")
				switch {
				case err == nil:
					mu.Lock()
					joined++
					mu.Unlock()
				case !errs.IsKind(err, errs.KindNotFound):
					t.Errorf("join: %v", err)
				}
			})
		}
		wg.Go(func() { delErr = r.DeleteIf(ctx, "computegpucluster-a", emptyCheck) })
		wg.Wait()
		switch {
		case delErr == nil && joined > 0:
			t.Fatalf("round %d: cluster deleted although %d instances joined", round, joined)
		case delErr != nil && !errs.IsKind(delErr, errs.KindBadState):
			t.Fatalf("round %d: delete: %v", round, delErr)
		case delErr != nil && joined == 0:
			t.Fatalf("round %d: delete refused with no members", round)
		}
	}
}
