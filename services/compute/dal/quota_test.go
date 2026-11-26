package dal

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

func TestQuotaRecordsListEveryProject(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	const other = "project-e00mock0000000000other"
	clusters := NewGpuClusterRepo(st)
	for _, c := range []*computepb.GpuCluster{gpuCluster("computegpucluster-1", "a"), {
		Metadata: &commonpb.ResourceMetadata{Id: "computegpucluster-2", ParentId: other, Name: "b"},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-3"},
	}} {
		if err := clusters.Create(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	rec := NewQuotaRecords(st)
	got, err := rec.GpuClusters(t.Context())
	if err != nil || len(got) != 2 {
		t.Fatalf("GpuClusters = %d items / %v, want the clusters of both projects", len(got), err)
	}
	for name, list := range map[string]func() (int, error){
		"instances":   func() (int, error) { l, err := rec.Instances(t.Context()); return len(l), err },
		"disks":       func() (int, error) { l, err := rec.Disks(t.Context()); return len(l), err },
		"snapshots":   func() (int, error) { l, err := rec.Snapshots(t.Context()); return len(l), err },
		"filesystems": func() (int, error) { l, err := rec.Filesystems(t.Context()); return len(l), err },
	} {
		if n, err := list(); err != nil || n != 0 {
			t.Fatalf("%s of an empty store = %d / %v", name, n, err)
		}
	}
}
