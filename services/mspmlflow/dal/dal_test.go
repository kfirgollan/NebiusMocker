package dal_test

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/dal"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

const (
	project  = "project-e00mock000000000default"
	project2 = "project-e00mock000000000other"
)

func cluster(id, parent, name string) *mlflowpb.Cluster {
	return &mlflowpb.Cluster{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, ResourceVersion: 1},
		Spec:     &mlflowpb.ClusterSpec{AdminUsername: "admin", ServiceAccountId: "serviceaccount-1", Size: "medium"},
		Status:   &mlflowpb.MlflowClusterStatus{Phase: mspv1alpha1pb.ClusterStatus_PHASE_RUNNING, State: mspv1alpha1pb.ClusterStatus_STATE_FINISHED},
	}
}

func TestClusterRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewClusterRepo(store.NewMemory())
	c := cluster("mlflow-1", project, "main")
	if err := r.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "mlflow-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(c, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	if byName, err := r.GetByName(ctx, project, "main"); err != nil || byName.GetMetadata().GetId() != "mlflow-1" {
		t.Errorf("GetByName = %v, %v, want mlflow-1", byName, err)
	}
	if _, err := r.GetByName(ctx, project2, "main"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetByName(other project) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Create(ctx, cluster("mlflow-2", project, "main")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(duplicate name) kind = %v, want AlreadyExists", errs.KindOf(err))
	}
	if err := r.Create(ctx, cluster("mlflow-3", project2, "main")); err != nil {
		t.Errorf("Create(same name, other project): %v", err)
	}
	if err := r.Create(ctx, cluster("mlflow-1", project, "other")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(duplicate ID) kind = %v, want AlreadyExists", errs.KindOf(err))
	}

	c.Status.Phase = mspv1alpha1pb.ClusterStatus_PHASE_STOPPING
	if err := r.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = r.Get(ctx, "mlflow-1")
	if got.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_STOPPING {
		t.Errorf("Update did not persist: %v", got.GetStatus())
	}
	if err := r.Update(ctx, cluster("mlflow-9", project, "ghost")); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update(missing) kind = %v, want NotFound", errs.KindOf(err))
	}

	if err := r.Delete(ctx, "mlflow-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "mlflow-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get after Delete kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "mlflow-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Delete(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	// Deleting frees the name.
	if err := r.Create(ctx, cluster("mlflow-4", project, "main")); err != nil {
		t.Errorf("Create(after delete): %v", err)
	}
}

func TestClusterRepoListPaginates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewClusterRepo(store.NewMemory())
	for i := range 5 {
		if err := r.Create(ctx, cluster(fmt.Sprintf("mlflow-%d", i), project, fmt.Sprintf("c%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Create(ctx, cluster("mlflow-x", project2, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	var names []string
	token := ""
	pages := 0
	for {
		page, err := r.List(ctx, project, 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, c := range page.Items {
			names = append(names, c.GetMetadata().GetName())
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	if diff := cmp.Diff([]string{"c0", "c1", "c2", "c3", "c4"}, names); diff != "" || pages != 3 {
		t.Errorf("listed %v in %d pages (-want +got):\n%s", names, pages, diff)
	}
	if _, err := r.List(ctx, project, 2, "!!"); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List(bad token) kind = %v, want BadRequest", errs.KindOf(err))
	}
}

func TestInternalsRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewInternalsRepo(store.NewMemory())
	if _, err := r.Get(ctx, "mlflow-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("Get(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	want := domain.Internals{NetworkID: "vpcnetwork-1", AccessKeyID: "accesskey-1", AWSAccessKeyID: "NMKX", Secret: "s"}
	if err := r.Put(ctx, "mlflow-1", domain.Internals{NetworkID: "vpcnetwork-1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(ctx, "mlflow-1", want); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Get(ctx, "mlflow-1"); err != nil || got != want {
		t.Fatalf("Get = %+v, %v, want %+v", got, err, want)
	}
	if err := r.Delete(ctx, "mlflow-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "mlflow-1"); err != nil {
		t.Errorf("Delete(missing): %v", err)
	}
	if _, err := r.Get(ctx, "mlflow-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get after Delete kind = %v, want NotFound", errs.KindOf(err))
	}
}
