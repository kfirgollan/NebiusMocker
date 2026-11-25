package dal

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

const project = "project-e00test0000000000001"

func reg(id, name string) *registrypb.Registry {
	return &registrypb.Registry{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: project, Name: name, ResourceVersion: 1},
		Spec:     &registrypb.RegistrySpec{Description: "d"},
		Status:   &registrypb.RegistryStatus{State: registrypb.RegistryStatus_ACTIVE},
	}
}

func TestRegistryRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewRegistryRepo(store.NewMemory())
	a, b := reg("registry-e00a", "a"), reg("registry-e00b", "b")
	for _, x := range []*registrypb.Registry{a, b} {
		if err := r.Create(ctx, x); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if err := r.Create(ctx, reg("registry-e00c", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Errorf("duplicate name: %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, a); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Errorf("duplicate id: %v, want AlreadyExists", err)
	}
	got, err := r.Get(ctx, "registry-e00a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(a, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get (-want +got):\n%s", diff)
	}
	byName, err := r.GetByName(ctx, project, "b")
	if err != nil || byName.GetMetadata().GetId() != "registry-e00b" {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	page, err := r.List(ctx, project, 1, "")
	if err != nil || len(page.Items) != 1 || page.NextPageToken == "" {
		t.Fatalf("List page 1 = %v, %v", page, err)
	}
	page2, err := r.List(ctx, project, 1, page.NextPageToken)
	if err != nil || len(page2.Items) != 1 || page2.Items[0].GetMetadata().GetId() != "registry-e00b" || page2.NextPageToken != "" {
		t.Fatalf("List page 2 = %v, %v", page2, err)
	}
	if _, err := r.List(ctx, project, 1, "@@"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("List bad token: %v", err)
	}
	a.Spec.Description = "changed"
	if err := r.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := r.Get(ctx, "registry-e00a"); got.GetSpec().GetDescription() != "changed" {
		t.Errorf("Update not stored: %v", got)
	}
	if err := r.Update(ctx, reg("registry-e00z", "z")); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Update missing: %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "registry-e00a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "registry-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
	if err := r.Delete(ctx, "registry-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Delete twice: %v", err)
	}
}

func TestArtifactIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	x := NewArtifactIndex(store.NewMemory())
	ref := domain.ArtifactRef{RegistryID: "registry-e00a", Repository: "team/web", Digest: "sha256:1"}
	if _, err := x.Lookup(ctx, ref); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Lookup before assign: %v", err)
	}
	if err := x.Assign(ctx, "artifact-e001", ref); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := x.Assign(ctx, "artifact-e002", ref); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Errorf("Assign same ref again: %v, want AlreadyExists", err)
	}
	if id, err := x.Lookup(ctx, ref); err != nil || id != "artifact-e001" {
		t.Errorf("Lookup = %q, %v", id, err)
	}
	if got, err := x.Get(ctx, "artifact-e001"); err != nil || got != ref {
		t.Errorf("Get = %+v, %v", got, err)
	}
	other := domain.ArtifactRef{RegistryID: "registry-e00b", Repository: "web", Digest: "sha256:1"}
	if err := x.Assign(ctx, "artifact-e003", other); err != nil {
		t.Fatalf("Assign other: %v", err)
	}
	if err := x.DeleteRegistry(ctx, "registry-e00a"); err != nil {
		t.Fatalf("DeleteRegistry: %v", err)
	}
	if _, err := x.Get(ctx, "artifact-e001"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after DeleteRegistry: %v", err)
	}
	if err := x.Delete(ctx, "artifact-e003"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := x.Delete(ctx, "artifact-e003"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Delete twice: %v", err)
	}
}
