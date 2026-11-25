package dal_test

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
)

func group(id, name string) *iampb.Group {
	return &iampb.Group{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "tenant-1", Name: name}}
}

func TestGroupRepoRoundTripAndErrors(t *testing.T) {
	t.Parallel()
	r := dal.NewGroupRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, group("g1", "a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(ctx, group("g2", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name err = %v, want AlreadyExists", err)
	}
	if err := r.Update(ctx, group("g9", "z")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing err = %v, want NotFound", err)
	}
	g, err := r.GetByName(ctx, "tenant-1", "a")
	if err != nil || g.GetMetadata().GetId() != "g1" {
		t.Fatalf("GetByName = %v, %v", g, err)
	}
	if err := r.Delete(ctx, "g1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "g1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete err = %v, want NotFound", err)
	}
}

func TestGroupRepoPaging(t *testing.T) {
	t.Parallel()
	r := dal.NewGroupRepo(store.NewMemory())
	for i, n := range []string{"a", "b", "c"} {
		if err := r.Create(t.Context(), group("g"+string(rune('1'+i)), n)); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := r.List(t.Context(), "tenant-1", 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := r.List(t.Context(), "tenant-1", 2, p1.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" || p2.Items[0].GetMetadata().GetName() != "c" {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	if _, err := r.List(t.Context(), "tenant-1", 2, "%%%"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token err = %v, want BadRequest", err)
	}
}
