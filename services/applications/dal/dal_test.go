package dal_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/applications/dal"
)

const (
	project  = "project-e00mock000000000default"
	project2 = "project-e00mock000000000other"
)

func release(id, parent, name string) *applicationsv1alpha1pb.K8SRelease {
	return &applicationsv1alpha1pb.K8SRelease{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, ResourceVersion: 1},
		Spec: &applicationsv1alpha1pb.K8SReleaseSpec{
			ClusterId: "mk8scluster-1", ProductSlug: "nebius/mlflow", Namespace: "ns", ApplicationName: "app",
			Values: "a: b\n", Set: map[string]string{"k": "v"},
		},
		Status: &applicationsv1alpha1pb.K8SReleaseStatus{State: applicationsv1alpha1pb.K8SReleaseStatus_CREATED},
	}
}

func TestReleaseRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewReleaseRepo(store.NewMemory())
	rel := release("applicationrelease-1", project, "")
	if err := r.Create(ctx, rel); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "applicationrelease-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(rel, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	if err := r.Create(ctx, rel); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("duplicate Create kind = %v, want AlreadyExists", errs.KindOf(err))
	}

	rel.Status.State = applicationsv1alpha1pb.K8SReleaseStatus_DEPLOYED
	if err := r.Update(ctx, rel); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = r.Get(ctx, "applicationrelease-1")
	if got.GetStatus().GetState() != applicationsv1alpha1pb.K8SReleaseStatus_DEPLOYED {
		t.Errorf("Update not stored: %v", got)
	}
	if err := r.Update(ctx, release("applicationrelease-9", project, "")); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update missing kind = %v, want NotFound", errs.KindOf(err))
	}

	if err := r.Delete(ctx, "applicationrelease-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "applicationrelease-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get after Delete kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "applicationrelease-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("second Delete kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestReleasesAreUnnamed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewReleaseRepo(store.NewMemory())
	for _, id := range []string{"applicationrelease-1", "applicationrelease-2"} {
		if err := r.Create(ctx, release(id, project, "same-name")); err != nil {
			t.Fatalf("Create %s with a repeated name: %v", id, err)
		}
	}
}

func TestListAndAll(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewReleaseRepo(store.NewMemory())
	for _, c := range []struct{ id, parent string }{
		{"applicationrelease-1", project}, {"applicationrelease-2", project2}, {"applicationrelease-3", project},
	} {
		if err := r.Create(ctx, release(c.id, c.parent, "")); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(rs []*applicationsv1alpha1pb.K8SRelease) []string {
		var out []string
		for _, x := range rs {
			out = append(out, x.GetMetadata().GetId())
		}
		return out
	}
	in, err := r.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"applicationrelease-1", "applicationrelease-3"}, ids(in)); diff != "" {
		t.Errorf("List (-want +got):\n%s", diff)
	}
	all, err := r.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"applicationrelease-1", "applicationrelease-2", "applicationrelease-3"}, ids(all)); diff != "" {
		t.Errorf("All (-want +got):\n%s", diff)
	}
}
