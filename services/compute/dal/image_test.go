package dal

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

const parent = "project-e00mock000000000default"

func image(id, name string) *computepb.Image {
	return &computepb.Image{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, Labels: map[string]string{"k": "v"}},
		Spec:     &computepb.ImageSpec{ImageFamily: "fam", Version: id},
		Status:   &computepb.ImageStatus{State: computepb.ImageStatus_READY, StorageSizeBytes: 42},
	}
}

func TestImageRepoRoundTripAndUniqueness(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewImageRepo(store.NewMemory())
	a := image("computeimage-e00a", "a")
	if err := r.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, "computeimage-e00a")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(a, got, protocmp.Transform()); diff != "" {
		t.Fatalf("Get differs (-want +got):\n%s", diff)
	}
	byName, err := r.GetByName(ctx, parent, "a")
	if err != nil || byName.GetMetadata().GetId() != "computeimage-e00a" {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	if err := r.Create(ctx, a); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate ID: %v", err)
	}
	if err := r.Create(ctx, image("computeimage-e00b", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := r.Update(ctx, image("computeimage-e00zz", "zz")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if err := r.Delete(ctx, "computeimage-e00a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "computeimage-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if err := r.Delete(ctx, "computeimage-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestImageRepoPaging(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewImageRepo(store.NewMemory())
	ids := []string{"computeimage-e00c", "computeimage-e00a", "computeimage-e00b"}
	for _, id := range ids {
		if err := r.Create(ctx, image(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	token := ""
	for {
		page, err := r.List(ctx, parent, 2, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, img := range page.Items {
			got = append(got, img.GetMetadata().GetId())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if diff := cmp.Diff(ids, got); diff != "" {
		t.Fatalf("paged List not in creation order (-want +got):\n%s", diff)
	}
	all, err := r.All(ctx, parent)
	if err != nil || len(all) != 3 {
		t.Fatalf("All = %d, %v", len(all), err)
	}
	if _, err := r.List(ctx, parent, 2, "%%%"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token: %v", err)
	}
	if other, _ := r.All(ctx, "project-e00other"); len(other) != 0 {
		t.Fatalf("other parent has %d images", len(other))
	}
}

func TestOpIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	idx := NewOpIndex(store.NewMemory())
	for _, op := range []string{"operation-e00b", "operation-e00a"} {
		if err := idx.Add(ctx, parent, op); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Add(ctx, "project-e00other", "operation-e00c"); err != nil {
		t.Fatal(err)
	}
	got, err := idx.IDs(ctx, parent)
	if err != nil || !cmp.Equal(got, []string{"operation-e00b", "operation-e00a"}) {
		t.Fatalf("IDs = %v, %v", got, err)
	}
}
