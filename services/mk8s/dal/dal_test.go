package dal_test

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/dal"
)

const (
	project  = "project-e00mock000000000default"
	project2 = "project-e00mock000000000other"
)

func cluster(id, parent, name string) *mk8spb.Cluster {
	return &mk8spb.Cluster{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, ResourceVersion: 1},
		Spec:     &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{SubnetId: "vpcsubnet-1"}},
		Status:   &mk8spb.ClusterStatus{State: mk8spb.ClusterStatus_RUNNING},
	}
}

func group(id, clusterID, name string) *mk8spb.NodeGroup {
	return &mk8spb.NodeGroup{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: clusterID, Name: name, ResourceVersion: 1},
		Spec:     &mk8spb.NodeGroupSpec{Size: &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: 2}},
		Status:   &mk8spb.NodeGroupStatus{NodeCount: 2},
	}
}

func TestClusterRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewClusterRepo(store.NewMemory())
	c := cluster("mk8scluster-1", project, "main")
	if err := r.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "mk8scluster-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(c, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	if byName, err := r.GetByName(ctx, project, "main"); err != nil || byName.GetMetadata().GetId() != "mk8scluster-1" {
		t.Errorf("GetByName = %v, %v, want mk8scluster-1", byName, err)
	}
	if _, err := r.GetByName(ctx, project2, "main"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetByName(other project) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Create(ctx, cluster("mk8scluster-2", project, "main")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(taken name) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}
	if err := r.Create(ctx, cluster("mk8scluster-2", project2, "main")); err != nil {
		t.Errorf("Create(same name, other project): %v", err)
	}
	c.Status.State = mk8spb.ClusterStatus_DELETING
	if err := r.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := r.Get(ctx, "mk8scluster-1"); got.GetStatus().GetState() != mk8spb.ClusterStatus_DELETING {
		t.Errorf("state after update = %v, want DELETING", got.GetStatus().GetState())
	}
	if err := r.Update(ctx, cluster("mk8scluster-x", project, "x")); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "mk8scluster-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "mk8scluster-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "mk8scluster-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Delete(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestNodeGroupRepoListAndAll(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewNodeGroupRepo(store.NewMemory())
	for i := range 5 {
		if err := r.Create(ctx, group(fmt.Sprintf("mk8snodegroup-%d", i), "mk8scluster-1", fmt.Sprintf("g%d", i))); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if err := r.Create(ctx, group("mk8snodegroup-x", "mk8scluster-2", "g0")); err != nil {
		t.Fatalf("Create in other cluster: %v", err)
	}
	if err := r.Create(ctx, group("mk8snodegroup-y", "mk8scluster-1", "g0")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(taken name) kind = %v, want AlreadyExists", errs.KindOf(err))
	}
	var names []string
	token := ""
	for range 10 {
		page, err := r.List(ctx, "mk8scluster-1", 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, g := range page.Items {
			names = append(names, g.GetMetadata().GetName())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if diff := cmp.Diff([]string{"g0", "g1", "g2", "g3", "g4"}, names); diff != "" {
		t.Errorf("paged names (-want +got):\n%s", diff)
	}
	all, err := r.All(ctx, "mk8scluster-1")
	if err != nil || len(all) != 5 {
		t.Fatalf("All = %d items, %v; want 5", len(all), err)
	}
	if _, err := r.List(ctx, "mk8scluster-1", 2, "%%%"); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List(bad token) kind = %v, want BadRequest", errs.KindOf(err))
	}
}

func TestNetworkBindingRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewNetworkBindingRepo(store.NewMemory())
	if _, err := r.Get(ctx, "mk8scluster-1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get before Set: %v, want not_found", err)
	}
	if err := r.Set(ctx, "mk8scluster-1", "vpcnetwork-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := r.Get(ctx, "mk8scluster-1"); err != nil || got != "vpcnetwork-1" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := r.Delete(ctx, "mk8scluster-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(ctx, "mk8scluster-1"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}
