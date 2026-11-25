package dal_test

import (
	"testing"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/ai/dal"
)

func endpoint(id, name string) *aipb.Endpoint {
	return &aipb.Endpoint{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "project-1", Name: name}}
}

func TestEndpointRepoRoundTripAndErrors(t *testing.T) {
	t.Parallel()
	r := dal.NewEndpointRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, endpoint("aiendpoint-1", "a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(ctx, endpoint("aiendpoint-2", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name err = %v, want AlreadyExists", err)
	}
	if err := r.Update(ctx, endpoint("aiendpoint-9", "z")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing err = %v, want NotFound", err)
	}
	g, err := r.GetByName(ctx, "project-1", "a")
	if err != nil || g.GetMetadata().GetId() != "aiendpoint-1" {
		t.Fatalf("GetByName = %v, %v", g, err)
	}
	if err := r.Delete(ctx, "aiendpoint-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "aiendpoint-1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete err = %v, want NotFound", err)
	}
}

func TestEndpointRepoPaging(t *testing.T) {
	t.Parallel()
	r := dal.NewEndpointRepo(store.NewMemory())
	for i, n := range []string{"a", "b", "c"} {
		if err := r.Create(t.Context(), endpoint("aiendpoint-"+string(rune('1'+i)), n)); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := r.List(t.Context(), "project-1", 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := r.List(t.Context(), "project-1", 2, p1.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" || p2.Items[0].GetMetadata().GetName() != "c" {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	if _, err := r.List(t.Context(), "project-1", 2, "%%%"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token err = %v, want BadRequest", err)
	}
}
