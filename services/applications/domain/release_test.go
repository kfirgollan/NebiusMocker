package domain_test

import (
	"strings"
	"testing"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/applications/domain"
)

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		edit  func(*applicationsv1alpha1pb.K8SReleaseSpec)
		kind  errs.Kind
		field string
	}{
		{"ok", func(*applicationsv1alpha1pb.K8SReleaseSpec) {}, errs.KindUnknown, ""},
		{"unknown product", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ProductSlug = "evil/thing" }, errs.KindBadRequest, "spec.product_slug"},
		{"uppercase namespace", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Namespace = "Prod" }, errs.KindBadRequest, "spec.namespace"},
		{"namespace dash end", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Namespace = "prod-" }, errs.KindBadRequest, "spec.namespace"},
		{"namespace too long", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Namespace = strings.Repeat("a", 64) }, errs.KindBadRequest, "spec.namespace"},
		{"namespace 63", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Namespace = strings.Repeat("a", 63) }, errs.KindUnknown, ""},
		{"underscore app name", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ApplicationName = "my_app" }, errs.KindBadRequest, "spec.application_name"},
		{"bad yaml", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Values = "a: [b" }, errs.KindBadRequest, "spec.values"},
		{"empty values", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Values = "" }, errs.KindUnknown, ""},
		{"empty set key", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Set = map[string]string{"": "x"} }, errs.KindBadRequest, "spec.set"},
		{"missing cluster", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ClusterId = missingID }, errs.KindNotFound, ""},
		{"foreign cluster", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ClusterId = foreignID }, errs.KindBadRequest, "spec.cluster_id"},
		{"cluster not running", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ClusterId = creatingID }, errs.KindBadState, ""},
		{"not a cluster id", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ClusterId = "vpcnetwork-x" }, errs.KindBadRequest, "spec.cluster_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			sp := spec()
			tt.edit(sp)
			_, err := e.create(t.Context(), sp)
			wantKind(t, "create", err, tt.kind)
			if tt.field != "" && !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error %q does not name %s", err, tt.field)
			}
			if tt.name == "cluster not running" && !strings.Contains(err.Error(), "is not RUNNING") {
				t.Errorf("error %q lacks %q", err, "is not RUNNING")
			}
			if tt.kind != errs.KindUnknown && len(e.releases.items) != 0 {
				t.Errorf("a rejected create stored %d releases", len(e.releases.items))
			}
		})
	}
}

func TestCreateParent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for name, parent := range map[string]string{"empty": "", "not a project": "folder-1"} {
		_, err := e.svc.Create(t.Context(), domain.CreateCmd{Metadata: &commonpb.ResourceMetadata{ParentId: parent}, Spec: spec()})
		wantKind(t, name, err, errs.KindBadRequest)
	}
	_, err := e.svc.Create(t.Context(), domain.CreateCmd{Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00nope"}, Spec: spec()})
	wantKind(t, "unknown project", err, errs.KindNotFound)
}

func TestLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newRelease(t, spec())
	if got := e.state(t, id); got != applicationsv1alpha1pb.K8SReleaseStatus_CREATED {
		t.Fatalf("state after Prepare = %v, want CREATED", got)
	}
	e.ops.finish()
	if got := e.state(t, id); got != applicationsv1alpha1pb.K8SReleaseStatus_DEPLOYED {
		t.Fatalf("state after Work = %v, want DEPLOYED", got)
	}

	up := spec()
	up.Values = "replicas: 3\n"
	_, err := e.svc.Update(t.Context(), domain.UpdateCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "renamed", Labels: map[string]string{"a": "b"}}, Spec: up,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := e.state(t, id); got != applicationsv1alpha1pb.K8SReleaseStatus_INSTALLING {
		t.Fatalf("state after update Prepare = %v, want INSTALLING", got)
	}
	e.ops.finish()
	r, err := e.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.GetStatus().GetState() != applicationsv1alpha1pb.K8SReleaseStatus_DEPLOYED ||
		r.GetMetadata().GetName() != "renamed" || r.GetMetadata().GetLabels()["a"] != "b" || r.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("after update = %v", r)
	}
	if stored := e.releases.items[0].GetSpec().GetValues(); stored != "replicas: 3\n" {
		t.Errorf("stored values = %q, want the updated ones kept for later updates", stored)
	}

	if _, err := e.svc.Delete(t.Context(), id, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = e.svc.Get(t.Context(), id)
	wantKind(t, "get after delete", err, errs.KindNotFound)
}

func TestUnnamedReleasesDoNotCollide(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a, b := e.newRelease(t, spec()), e.newRelease(t, spec())
	if a == b {
		t.Fatalf("two releases got the same ID %s", a)
	}
}

func TestUpdateImmutable(t *testing.T) {
	t.Parallel()
	for field, edit := range map[string]func(*applicationsv1alpha1pb.K8SReleaseSpec){
		"spec.cluster_id":       func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ClusterId = creatingID },
		"spec.product_slug":     func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ProductSlug = "nebius/kuberay" },
		"spec.namespace":        func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Namespace = "other" },
		"spec.application_name": func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ApplicationName = "other" },
	} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			id := e.newRelease(t, spec())
			sp := spec()
			edit(sp)
			_, err := e.svc.Update(t.Context(), domain.UpdateCmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: sp})
			wantKind(t, "update", err, errs.KindBadRequest)
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not name %s", err, field)
			}
		})
	}
}

func TestUpdateRejects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newRelease(t, spec())
	bad := spec()
	bad.Values = "a: [b"
	_, err := e.svc.Update(t.Context(), domain.UpdateCmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: bad})
	wantKind(t, "bad yaml", err, errs.KindBadRequest)
	_, err = e.svc.Update(t.Context(), domain.UpdateCmd{Metadata: &commonpb.ResourceMetadata{Id: id, ResourceVersion: 9}, Spec: spec()})
	wantKind(t, "stale version", err, errs.KindBadState)
	_, err = e.svc.Update(t.Context(), domain.UpdateCmd{Metadata: &commonpb.ResourceMetadata{Id: "applicationrelease-x"}, Spec: spec()})
	wantKind(t, "missing", err, errs.KindNotFound)
}

func TestInputOnlyFieldsAreHidden(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newRelease(t, spec())
	r, err := e.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.GetSpec().GetValues() != "" || len(r.GetSpec().GetSet()) != 0 {
		t.Errorf("Get returned input-only fields: %v", r.GetSpec())
	}
	if r.GetSpec().GetNamespace() != "mlflow" {
		t.Errorf("Get lost namespace: %v", r.GetSpec())
	}
	page, err := e.svc.List(t.Context(), domain.ListCmd{ParentID: projectID, ClusterID: runningID})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("List = %v, %v", page, err)
	}
	if v := page.Items[0].GetSpec(); v.GetValues() != "" || len(v.GetSet()) != 0 {
		t.Errorf("List returned input-only fields: %v", v)
	}
	// What the store keeps is untouched by the redaction.
	if stored := e.releases.items[0].GetSpec(); stored.GetValues() == "" || len(stored.GetSet()) == 0 {
		t.Errorf("store lost the input-only fields: %v", stored)
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for range 3 {
		e.newRelease(t, spec())
	}
	e.releases.items = append(e.releases.items, &applicationsv1alpha1pb.K8SRelease{
		Metadata: &commonpb.ResourceMetadata{Id: "applicationrelease-other", ParentId: projectID},
		Spec:     &applicationsv1alpha1pb.K8SReleaseSpec{ClusterId: creatingID},
	})

	var ids []string
	token := ""
	for {
		page, err := e.svc.List(t.Context(), domain.ListCmd{ParentID: projectID, ClusterID: runningID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Items {
			ids = append(ids, r.GetMetadata().GetId())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if len(ids) != 3 {
		t.Fatalf("listed %v, want the 3 releases of the cluster", ids)
	}

	for name, cmd := range map[string]domain.ListCmd{
		"filter":       {ParentID: projectID, ClusterID: runningID, Filter: "x"},
		"bad token":    {ParentID: projectID, ClusterID: runningID, PageToken: "!"},
		"page size":    {ParentID: projectID, ClusterID: runningID, PageSize: -1},
		"foreign":      {ParentID: projectID, ClusterID: foreignID},
		"no cluster":   {ParentID: projectID},
		"no parent":    {ClusterID: runningID},
		"cluster kind": {ParentID: projectID, ClusterID: "vpcnetwork-x"},
	} {
		_, err := e.svc.List(t.Context(), cmd)
		wantKind(t, name, err, errs.KindBadRequest)
	}
	_, err := e.svc.List(t.Context(), domain.ListCmd{ParentID: projectID, ClusterID: missingID})
	wantKind(t, "missing cluster", err, errs.KindNotFound)

	_, err = e.svc.List(t.Context(), domain.ListCmd{ParentID: projectID, ClusterID: runningID, Filter: "x"})
	if !strings.Contains(err.Error(), "filtering is not implemented in NebiusMocker yet") {
		t.Errorf("filter error = %v", err)
	}
}

func TestDeleteClusterReleases(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	keep := spec()
	keep.ClusterId = foreignID
	a, b := e.newRelease(t, spec()), e.newRelease(t, spec())
	kept, err := e.svc.Create(t.Context(), domain.CreateCmd{Metadata: &commonpb.ResourceMetadata{ParentId: otherProjectID}, Spec: keep})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteClusterReleases(t.Context(), runningID); err != nil {
		t.Fatalf("DeleteClusterReleases: %v", err)
	}
	for _, id := range []string{a, b} {
		_, err := e.svc.Get(t.Context(), id)
		wantKind(t, "release "+id, err, errs.KindNotFound)
	}
	if _, err := e.svc.Get(t.Context(), kept.ResourceID); err != nil {
		t.Errorf("another cluster's release was removed: %v", err)
	}
	if err := e.svc.DeleteClusterReleases(t.Context(), creatingID); err != nil {
		t.Errorf("cluster without releases: %v", err)
	}
}

func TestWorkAfterDeleteIsHarmless(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newRelease(t, spec())
	if _, err := e.svc.Delete(t.Context(), id, &applicationsv1alpha1pb.DeleteK8SReleaseRequest{Id: id}); err != nil {
		t.Fatal(err)
	}
	e.ops.finish() // the create's Work finds nothing and must not fail or resurrect
	if len(e.releases.items) != 0 {
		t.Fatalf("release resurrected: %v", e.releases.items)
	}
}

func TestCreateRejectsServerFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.svc.Create(t.Context(), domain.CreateCmd{Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Id: "x"}, Spec: spec()})
	wantKind(t, "metadata.id", err, errs.KindBadRequest)
}
