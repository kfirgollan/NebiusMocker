package dal_test

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/dal"
)

func bucket(id, parent, name string) *storagepb.Bucket {
	return &storagepb.Bucket{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Status:   &storagepb.BucketStatus{State: storagepb.BucketStatus_ACTIVE},
	}
}

func TestBucketRepoGlobalNames(t *testing.T) {
	t.Parallel()
	r := dal.NewBucketRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, bucket("b1", "project-a", "shared")); err != nil {
		t.Fatal(err)
	}
	// Names are global: another project cannot take it.
	if err := r.Create(ctx, bucket("b2", "project-b", "shared")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name in another project: %v, want AlreadyExists", err)
	}
	got, err := r.GetByBucketName(ctx, "shared")
	if err != nil || got.GetMetadata().GetId() != "b1" {
		t.Fatalf("GetByBucketName = %v, %v", got, err)
	}
	if got, err := r.GetByName(ctx, "project-a", "shared"); err != nil || got.GetMetadata().GetId() != "b1" {
		t.Fatalf("GetByName = %v, %v", got, err)
	}
	if _, err := r.GetByName(ctx, "project-b", "shared"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByName in other project: %v, want NotFound", err)
	}
	got.Status.State = storagepb.BucketStatus_SCHEDULED_FOR_DELETION
	if err := r.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, bucket("missing", "project-a", "x")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Update missing: %v, want NotFound", err)
	}
	// Delete releases the name.
	if err := r.Delete(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetByBucketName(ctx, "shared"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByBucketName after delete: %v, want NotFound", err)
	}
	if err := r.Create(ctx, bucket("b2", "project-b", "shared")); err != nil {
		t.Fatalf("reuse released name: %v", err)
	}
	if err := r.Delete(ctx, "b1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Delete twice: %v, want NotFound", err)
	}
}

func TestBucketRepoPaging(t *testing.T) {
	t.Parallel()
	r := dal.NewBucketRepo(store.NewMemory())
	for _, n := range []string{"aaa", "bbb", "ccc"} {
		if err := r.Create(t.Context(), bucket("id-"+n, "project-a", n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Create(t.Context(), bucket("id-other", "project-b", "ddd")); err != nil {
		t.Fatal(err)
	}
	p1, err := r.List(t.Context(), "project-a", 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := r.List(t.Context(), "project-a", 2, p1.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" || p2.Items[0].GetMetadata().GetName() != "ccc" {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	if _, err := r.List(t.Context(), "project-a", 2, "%%"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token: %v, want BadRequest", err)
	}
	all, err := r.ListAll(t.Context())
	if err != nil || len(all) != 4 {
		t.Fatalf("ListAll = %d items, %v; want 4", len(all), err)
	}
}
