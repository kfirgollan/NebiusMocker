package dal_test

import (
	"strings"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
)

func container(id, parentID, name string) *iampb.Container {
	return &iampb.Container{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parentID, Name: name, ResourceVersion: 1},
		Spec:     &iampb.ContainerSpec{Region: "eu-north1"},
	}
}

func isVPC(kind string) bool { return kind == "vpcnetwork" }

func TestTenantRepoGetByNameAndPaging(t *testing.T) {
	t.Parallel()
	r := dal.NewTenantRepo(store.NewMemory())
	ctx := t.Context()
	for _, c := range []*iampb.Container{container("tenant-1", "", "one"), container("tenant-2", "", "two"), container("tenant-3", "", "three")} {
		if err := r.Ensure(ctx, c); err != nil {
			t.Fatalf("Ensure %s: %v", c.GetMetadata().GetId(), err)
		}
	}
	// Ensure is idempotent: a second call with a different name changes nothing.
	if err := r.Ensure(ctx, container("tenant-1", "", "renamed")); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	got, err := r.GetByName(ctx, "one")
	if err != nil || got.GetMetadata().GetId() != "tenant-1" {
		t.Fatalf("GetByName(one) = %v, %v", got, err)
	}
	if _, err := r.GetByName(ctx, "renamed"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByName(renamed) err = %v, want NotFound", err)
	}
	if _, err := r.Get(ctx, "tenant-9"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get unknown err = %v, want NotFound", err)
	}

	p1, err := r.List(ctx, 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := r.List(ctx, 2, p1.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" || p2.Items[0].GetMetadata().GetId() != "tenant-3" {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	if _, err := r.List(ctx, 2, "%%%"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token err = %v, want BadRequest", err)
	}
}

func TestProjectRepoCRUDAndNames(t *testing.T) {
	t.Parallel()
	r := dal.NewProjectRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, container("project-1", "tenant-1", "a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(ctx, container("project-2", "tenant-1", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name in tenant err = %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, container("project-1", "tenant-1", "other")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate id err = %v, want AlreadyExists", err)
	}
	// The same name is fine in another tenant.
	if err := r.Create(ctx, container("project-3", "tenant-2", "a")); err != nil {
		t.Fatalf("same name in other tenant: %v", err)
	}

	got, err := r.GetByName(ctx, "tenant-1", "a")
	if err != nil || got.GetMetadata().GetId() != "project-1" || got.GetSpec().GetRegion() != "eu-north1" {
		t.Fatalf("GetByName = %v, %v", got, err)
	}
	if _, err := r.GetByName(ctx, "tenant-2", "zzz"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByName unknown err = %v, want NotFound", err)
	}

	renamed := container("project-1", "tenant-1", "renamed")
	renamed.Metadata.ResourceVersion = 2
	if err := r.Update(ctx, renamed); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = r.Get(ctx, "project-1")
	if err != nil || got.GetMetadata().GetName() != "renamed" || got.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("Get after update = %v, %v", got, err)
	}
	if _, err := r.GetByName(ctx, "tenant-1", "a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("old name still resolves: err = %v", err)
	}
	if err := r.Update(ctx, container("project-9", "tenant-1", "x")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Update missing err = %v, want NotFound", err)
	}
	// Renaming onto a sibling's name is refused.
	if err := r.Create(ctx, container("project-4", "tenant-1", "b")); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, container("project-4", "tenant-1", "renamed")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("rename onto sibling err = %v, want AlreadyExists", err)
	}
}

func TestProjectRepoEnsureAndList(t *testing.T) {
	t.Parallel()
	r := dal.NewProjectRepo(store.NewMemory())
	ctx := t.Context()
	for i, n := range []string{"a", "b", "c"} {
		if err := r.Ensure(ctx, container("project-"+string(rune('1'+i)), "tenant-1", n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Ensure(ctx, container("project-9", "tenant-2", "foreign")); err != nil {
		t.Fatal(err)
	}
	// Ensure does not overwrite.
	if err := r.Ensure(ctx, container("project-1", "tenant-1", "changed")); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "project-1"); got.GetMetadata().GetName() != "a" {
		t.Fatalf("Ensure overwrote: name = %q", got.GetMetadata().GetName())
	}

	p1, err := r.List(ctx, "tenant-1", 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := r.List(ctx, "tenant-1", 2, p1.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" || p2.Items[0].GetMetadata().GetName() != "c" {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	other, err := r.List(ctx, "tenant-2", 10, "")
	if err != nil || len(other.Items) != 1 || other.Items[0].GetMetadata().GetId() != "project-9" {
		t.Fatalf("other tenant = %+v, %v", other, err)
	}
}

func TestProjectRepoDeleteIfEmpty(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	setup := func(t *testing.T) (*dal.ProjectRepo, store.Store) {
		t.Helper()
		st := store.NewMemory()
		r := dal.NewProjectRepo(st)
		if err := r.Create(ctx, container("project-1", "tenant-1", "a")); err != nil {
			t.Fatal(err)
		}
		return r, st
	}
	put := func(t *testing.T, st store.Store, rec store.Record) {
		t.Helper()
		if err := st.Update(ctx, func(tx store.Tx) error {
			_, err := tx.Put(rec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(r *dal.ProjectRepo) bool {
		_, err := r.Get(ctx, "project-1")
		return err == nil
	}

	t.Run("empty project is deleted", func(t *testing.T) {
		t.Parallel()
		r, _ := setup(t)
		if err := r.DeleteIfEmpty(ctx, "project-1", isVPC); err != nil {
			t.Fatalf("DeleteIfEmpty: %v", err)
		}
		if exists(r) {
			t.Error("project still exists")
		}
	})
	t.Run("blocked by a resource child naming it", func(t *testing.T) {
		t.Parallel()
		r, st := setup(t)
		put(t, st, store.Record{Kind: "vpcnetwork", ID: "vpcnetwork-e00child1", ParentID: "project-1", Name: "net"})
		err := r.DeleteIfEmpty(ctx, "project-1", isVPC)
		if !errs.IsKind(err, errs.KindBadState) {
			t.Fatalf("err = %v, want BadState", err)
		}
		if !strings.Contains(err.Error(), "vpcnetwork-e00child1") {
			t.Errorf("err = %q, want it to name the child", err)
		}
		if !exists(r) {
			t.Error("blocked delete removed the project")
		}
	})
	t.Run("not blocked by a non-resource kind", func(t *testing.T) {
		t.Parallel()
		r, st := setup(t)
		put(t, st, store.Record{Kind: "accesskeyindex", ID: "idx-1", ParentID: "project-1", Name: "i"})
		if err := r.DeleteIfEmpty(ctx, "project-1", isVPC); err != nil {
			t.Fatalf("DeleteIfEmpty: %v", err)
		}
		if exists(r) {
			t.Error("project still exists")
		}
	})
	t.Run("children of another parent do not block", func(t *testing.T) {
		t.Parallel()
		r, st := setup(t)
		put(t, st, store.Record{Kind: "vpcnetwork", ID: "vpcnetwork-e00child2", ParentID: "project-2", Name: "net"})
		if err := r.DeleteIfEmpty(ctx, "project-1", isVPC); err != nil {
			t.Fatalf("DeleteIfEmpty: %v", err)
		}
	})
	t.Run("dry run keeps the project", func(t *testing.T) {
		t.Parallel()
		r, st := setup(t)
		if err := r.CheckEmpty(ctx, "project-1", isVPC); err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if !exists(r) {
			t.Fatal("dry run deleted the project")
		}
		put(t, st, store.Record{Kind: "vpcnetwork", ID: "vpcnetwork-e00child1", ParentID: "project-1", Name: "net"})
		if err := r.CheckEmpty(ctx, "project-1", isVPC); !errs.IsKind(err, errs.KindBadState) {
			t.Fatalf("dry run on non-empty err = %v, want BadState", err)
		}
	})
	t.Run("names at most five children", func(t *testing.T) {
		t.Parallel()
		r, st := setup(t)
		for _, id := range []string{"v1", "v2", "v3", "v4", "v5", "v6", "v7"} {
			put(t, st, store.Record{Kind: "vpcnetwork", ID: id, ParentID: "project-1", Name: id})
		}
		err := r.DeleteIfEmpty(ctx, "project-1", isVPC)
		if !errs.IsKind(err, errs.KindBadState) || !strings.Contains(err.Error(), "and 2 more") || strings.Contains(err.Error(), "v6") {
			t.Fatalf("err = %v, want BadState naming 5 and 'and 2 more'", err)
		}
	})
	t.Run("unknown project", func(t *testing.T) {
		t.Parallel()
		r, _ := setup(t)
		if err := r.DeleteIfEmpty(ctx, "project-9", isVPC); !errs.IsKind(err, errs.KindNotFound) {
			t.Fatalf("err = %v, want NotFound", err)
		}
	})
}
