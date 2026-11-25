package domain

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

type fakeGpuClusters struct {
	repo memRepo[*computepb.GpuCluster]
}

func (f *fakeGpuClusters) Get(_ context.Context, id string) (*computepb.GpuCluster, error) {
	return f.repo.get(id)
}

func (f *fakeGpuClusters) GetByName(_ context.Context, parentID, name string) (*computepb.GpuCluster, error) {
	return f.repo.getByName(parentID, name)
}

func (f *fakeGpuClusters) List(_ context.Context, parentID string, size int, token string) (GpuClusterPage, error) {
	items, next, err := f.repo.page(func(c *computepb.GpuCluster) bool { return c.GetMetadata().GetParentId() == parentID }, size, token)
	return GpuClusterPage{Items: items, NextPageToken: next}, err
}

func (f *fakeGpuClusters) Create(_ context.Context, c *computepb.GpuCluster) error {
	return f.repo.create(c)
}

func (f *fakeGpuClusters) Mutate(_ context.Context, id string, fn func(*computepb.GpuCluster) error) (*computepb.GpuCluster, error) {
	return f.repo.mutate(id, fn)
}

func (f *fakeGpuClusters) DeleteIf(_ context.Context, id string, check func(*computepb.GpuCluster) error) error {
	c, err := f.repo.get(id)
	if err != nil {
		return err
	}
	if err := check(c); err != nil {
		return err
	}
	return f.repo.remove(id)
}

// fakeRegions maps a project to its region.
type fakeRegions map[string]string

func (f fakeRegions) Region(_ context.Context, projectID string) (string, error) {
	r, ok := f[projectID]
	if !ok {
		return "", errs.NotFound(projectID)
	}
	return r, nil
}

type gpuFixture struct {
	clusters *fakeGpuClusters
	ops      *deferOps
	idx      *fakeOpIndex
	clk      *clock.Manual
	svc      *GpuClusterService
	members  *GpuClusterMembers
}

func newGpuFixture(t *testing.T) *gpuFixture {
	t.Helper()
	fabrics, err := DefaultFabricCatalog()
	if err != nil {
		t.Fatal(err)
	}
	f := &gpuFixture{
		clusters: &fakeGpuClusters{}, ops: &deferOps{}, idx: &fakeOpIndex{},
		clk: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	f.svc, err = NewGpuClusterService(GpuClusterDeps{
		Clusters: f.clusters, Fabrics: fabrics, OpIndex: f.idx,
		Projects: fakeProjects{testProject: true, otherProject: true},
		Regions:  fakeRegions{testProject: "eu-north1", otherProject: "eu-west1"},
		Ops:      f.ops, IDs: &fakeIDs{}, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.members, err = NewGpuClusterMembers(f.clusters, fabrics); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *gpuFixture) create(t *testing.T, name string) string {
	t.Helper()
	op, err := f.svc.Create(t.Context(), CreateGpuClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-3"},
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return op.ResourceID
}

func (f *gpuFixture) get(t *testing.T, id string) *computepb.GpuCluster {
	t.Helper()
	c, err := f.clusters.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return c
}

func violationFields(err error) []string {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range e.Violations {
		out = append(out, v.Field)
	}
	return out
}

func TestGpuClusterCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		md        *commonpb.ResourceMetadata
		fabric    string
		wantKind  errs.Kind
		wantField string
	}{
		{name: "fabric missing", md: &commonpb.ResourceMetadata{ParentId: testProject}, wantKind: errs.KindBadRequest, wantField: "spec.infiniband_fabric"},
		{name: "fabric unknown", md: &commonpb.ResourceMetadata{ParentId: testProject}, fabric: "fabric-99", wantKind: errs.KindBadRequest, wantField: "spec.infiniband_fabric"},
		{name: "fabric of another region", md: &commonpb.ResourceMetadata{ParentId: testProject}, fabric: "fabric-5", wantKind: errs.KindBadRequest, wantField: "spec.infiniband_fabric"},
		{name: "id set", md: &commonpb.ResourceMetadata{Id: "x", ParentId: testProject}, fabric: "fabric-3", wantKind: errs.KindBadRequest, wantField: "metadata.id"},
		{name: "parent missing", md: &commonpb.ResourceMetadata{}, fabric: "fabric-3", wantKind: errs.KindBadRequest, wantField: "metadata.parent_id"},
		{name: "parent unknown", md: &commonpb.ResourceMetadata{ParentId: "project-e00mock00000000unknown"}, fabric: "fabric-3", wantKind: errs.KindNotFound},
		{name: "ok", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "ok"}, fabric: "fabric-3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newGpuFixture(t)
			op, err := f.svc.Create(t.Context(), CreateGpuClusterCmd{Metadata: tt.md, Spec: &computepb.GpuClusterSpec{InfinibandFabric: tt.fabric}})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil && op != nil {
				t.Fatalf("operation returned next to error %v", err)
			}
			if tt.wantField != "" && !slices.Contains(violationFields(err), tt.wantField) {
				t.Fatalf("violations = %v, want field %s", violationFields(err), tt.wantField)
			}
		})
	}
}

func TestGpuClusterCreateFabricHintListsRegionFabrics(t *testing.T) {
	t.Parallel()
	f := newGpuFixture(t)
	_, err := f.svc.Create(t.Context(), CreateGpuClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-5"},
	})
	if err == nil || !strings.Contains(err.Error(), "fabric-3") || !strings.Contains(err.Error(), "eu-west1") {
		t.Fatalf("err = %v, want a hint naming the other region and fabric-3", err)
	}
}

func TestGpuClusterCreateReconciles(t *testing.T) {
	t.Parallel()
	f := newGpuFixture(t)
	f.ops.deferred = true
	id := f.create(t, "c1")
	c := f.get(t, id)
	if c.GetSpec().GetInfinibandFabric() != "fabric-3" || !c.GetStatus().GetReconciling() ||
		c.GetMetadata().GetResourceVersion() != 1 || c.GetMetadata().GetParentId() != testProject {
		t.Fatalf("cluster before op work = %v", c)
	}
	f.ops.finish()
	c = f.get(t, id)
	if c.GetStatus().GetReconciling() || c.GetSpec().GetInfinibandFabric() != "fabric-3" {
		t.Fatalf("cluster after op work = %v", c)
	}
	if got, err := f.svc.GetByName(t.Context(), testProject, "c1"); err != nil || got.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", got, err)
	}
	if _, err := f.svc.Get(t.Context(), ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("Get empty id: %v", err)
	}
}

func TestGpuClusterUpdate(t *testing.T) {
	t.Parallel()
	f := newGpuFixture(t)
	id := f.create(t, "c1")
	if _, err := f.members.Join(t.Context(), id, "computeinstance-1", testProject, "gpu-h100-sxm"); err != nil {
		t.Fatal(err)
	}
	before := f.get(t, id)
	f.clk.Advance(time.Hour)

	update := func(md *commonpb.ResourceMetadata, fabric string) error {
		md.Id = id
		_, err := f.svc.Update(t.Context(), UpdateGpuClusterCmd{Metadata: md, Spec: &computepb.GpuClusterSpec{InfinibandFabric: fabric}})
		return err
	}

	// Fabric change is rejected and leaves the cluster alone.
	err := update(&commonpb.ResourceMetadata{Name: "c1"}, "fabric-4")
	if !errs.IsKind(err, errs.KindBadRequest) || !slices.Contains(violationFields(err), "spec.infiniband_fabric") {
		t.Fatalf("fabric change: %v", err)
	}
	if !proto.Equal(before, f.get(t, id)) {
		t.Fatal("rejected update changed the cluster")
	}

	// Same values: no new version.
	if err := update(&commonpb.ResourceMetadata{Name: "c1"}, "fabric-3"); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if !proto.Equal(before, f.get(t, id)) {
		t.Fatalf("no-op update changed the cluster: %v", f.get(t, id))
	}

	// Empty fabric keeps the stored one; name and labels change, version bumps, status members stay.
	if err := update(&commonpb.ResourceMetadata{Name: "c2", Labels: map[string]string{"env": "dev"}}, ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := f.get(t, id)
	if got.GetMetadata().GetName() != "c2" || got.GetMetadata().GetLabels()["env"] != "dev" ||
		got.GetMetadata().GetResourceVersion() != before.GetMetadata().GetResourceVersion()+1 ||
		!got.GetMetadata().GetUpdatedAt().AsTime().Equal(f.clk.Now()) {
		t.Fatalf("metadata after update = %v", got.GetMetadata())
	}
	if got.GetSpec().GetInfinibandFabric() != "fabric-3" {
		t.Fatalf("fabric = %q, want fabric-3", got.GetSpec().GetInfinibandFabric())
	}
	if !proto.Equal(got.GetStatus(), before.GetStatus()) || !slices.Equal(got.GetStatus().GetInstances(), []string{"computeinstance-1"}) {
		t.Fatalf("status = %v, want unchanged with its member", got.GetStatus())
	}

	if _, err := f.svc.Update(t.Context(), UpdateGpuClusterCmd{Metadata: &commonpb.ResourceMetadata{}}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("update without id: %v", err)
	}
	if _, err := f.svc.Update(t.Context(), UpdateGpuClusterCmd{Metadata: &commonpb.ResourceMetadata{Id: "computegpucluster-missing"}}); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestGpuClusterDeleteRefusedWhileMembers(t *testing.T) {
	t.Parallel()
	f := newGpuFixture(t)
	id := f.create(t, "c1")
	path, err := f.members.Join(t.Context(), id, "computeinstance-1", testProject, "gpu-h100-sxm")
	if err != nil || len(path) != 3 {
		t.Fatalf("Join = %v, %v", path, err)
	}

	op, err := f.svc.Delete(t.Context(), id, nil)
	if !errs.IsKind(err, errs.KindBadState) || !strings.Contains(err.Error(), "computeinstance-1") {
		t.Fatalf("delete non-empty: op=%v err=%v, want BadState naming the instance", op, err)
	}
	if _, err := f.clusters.Get(t.Context(), id); err != nil {
		t.Fatalf("cluster gone after refused delete: %v", err)
	}

	if err := f.members.Leave(t.Context(), id, "computeinstance-1"); err != nil {
		t.Fatal(err)
	}
	if st := f.get(t, id).GetStatus(); len(st.GetInstances()) != 0 || st.GetTopology() != nil {
		t.Fatalf("status after Leave = %v", st)
	}
	op, err = f.svc.Delete(t.Context(), id, nil)
	if err != nil || op.Err != nil {
		t.Fatalf("delete empty: %v / %v", err, op)
	}
	if _, err := f.clusters.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("cluster after delete: %v", err)
	}
	if _, err := f.svc.Delete(t.Context(), id, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := f.svc.Delete(t.Context(), "", nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("delete empty id: %v", err)
	}
}

func TestGpuClusterJoinChecks(t *testing.T) {
	t.Parallel()
	f := newGpuFixture(t)
	id := f.create(t, "c1")
	tests := []struct {
		name                     string
		cluster, inst, proj, plt string
		wantKind                 errs.Kind
	}{
		{"missing cluster", "computegpucluster-nope", "i", testProject, "gpu-h100-sxm", errs.KindNotFound},
		{"other project", id, "i", otherProject, "gpu-h100-sxm", errs.KindBadRequest},
		{"wrong platform", id, "i", testProject, "gpu-h200-sxm", errs.KindBadRequest},
		{"no cluster id", "", "i", testProject, "gpu-h100-sxm", errs.KindBadRequest},
		{"no instance id", id, "", testProject, "gpu-h100-sxm", errs.KindBadRequest},
	}
	for _, tt := range tests {
		if _, err := f.members.Join(t.Context(), tt.cluster, tt.inst, tt.proj, tt.plt); errs.KindOf(err) != tt.wantKind {
			t.Errorf("%s: kind = %v, want %v (%v)", tt.name, errs.KindOf(err), tt.wantKind, err)
		}
	}
	if len(f.get(t, id).GetStatus().GetInstances()) != 0 {
		t.Fatal("a rejected Join added a member")
	}
}

func TestGpuClusterListAndFilter(t *testing.T) {
	t.Parallel()
	f := newGpuFixture(t)
	for _, n := range []string{"a", "b", "c"} {
		f.create(t, n)
	}
	var names []string
	token := ""
	for range 5 {
		p, err := f.svc.List(t.Context(), testProject, 2, token, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.Items {
			names = append(names, c.GetMetadata().GetName())
		}
		if token = p.NextPageToken; token == "" {
			break
		}
	}
	if !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Fatalf("names = %v", names)
	}
	if _, err := f.svc.List(t.Context(), testProject, 0, "", "name=a"); !errs.IsKind(err, errs.KindBadRequest) ||
		!slices.Contains(violationFields(err), "filter") {
		t.Fatalf("filter: %v", err)
	}
	if _, err := f.svc.List(t.Context(), "", 0, "", ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("empty parent: %v", err)
	}
}

func TestGpuClusterListOperationsByParent(t *testing.T) {
	t.Parallel()
	f := newGpuFixture(t)
	for _, n := range []string{"a", "b", "c"} {
		f.create(t, n)
	}
	id := f.create(t, "d")
	del, err := f.svc.Delete(t.Context(), id, nil) // newest, on a cluster that no longer exists
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(t.Context(), CreateGpuClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: otherProject, Name: "x"},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-5"},
	}); err != nil {
		t.Fatal(err)
	}

	f.idx.mu.Lock()
	want := slices.Clone(f.idx.ids[testProject])
	f.idx.mu.Unlock()
	slices.Reverse(want)
	if len(want) != 5 || want[0] != del.ID {
		t.Fatalf("indexed ids = %v", want)
	}
	var got []string
	token := ""
	for page := range 5 {
		p, err := f.svc.ListOperationsByParent(t.Context(), testProject, 2, token)
		if err != nil {
			t.Fatal(err)
		}
		if page == 0 && len(p.Items) != 2 {
			t.Fatalf("first page has %d items", len(p.Items))
		}
		for _, op := range p.Items {
			got = append(got, op.ID)
		}
		if token = p.NextPageToken; token == "" {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("operations = %v, want newest first %v", got, want)
	}
	if _, err := f.svc.ListOperationsByParent(t.Context(), "", 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("empty parent: %v", err)
	}
	if _, err := f.svc.ListOperationsByParent(t.Context(), testProject, 0, "garbage!"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestNewGpuClusterServiceRequiresDependencies(t *testing.T) {
	t.Parallel()
	if _, err := NewGpuClusterService(GpuClusterDeps{}); err == nil {
		t.Fatal("accepted empty dependencies")
	}
	if _, err := NewGpuClusterMembers(nil, FabricCatalog{}); err == nil {
		t.Fatal("NewGpuClusterMembers accepted a nil repository")
	}
}
