package dal

import (
	"slices"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

func TestNodeRepo(t *testing.T) {
	ctx := t.Context()
	r := NewNodeRepo(store.NewMemory())
	recs := []domain.NodeRecord{
		{InstanceID: "computeinstance-b", NodeGroupID: "ng-1", ClusterID: "c-1", Name: "n-b"},
		{InstanceID: "computeinstance-a", NodeGroupID: "ng-1", ClusterID: "c-1", Name: "n-a"},
		{InstanceID: "computeinstance-c", NodeGroupID: "ng-2", ClusterID: "c-2", Name: "n-b"},
	}
	for _, rec := range recs {
		if err := r.Put(ctx, rec); err != nil {
			t.Fatalf("Put(%s): %v", rec.InstanceID, err)
		}
	}

	got, ok, err := r.Owner(ctx, "computeinstance-a")
	if err != nil || !ok || got != recs[1] {
		t.Fatalf("Owner = %+v, %v, %v; want %+v", got, ok, err, recs[1])
	}
	if _, ok, err := r.Owner(ctx, "computeinstance-x"); err != nil || ok {
		t.Fatalf("Owner(unknown) = %v, %v; want not found", ok, err)
	}
	if got, ok, _ := r.ByName(ctx, "ng-2", "n-b"); !ok || got.InstanceID != "computeinstance-c" {
		t.Fatalf("ByName(ng-2, n-b) = %+v, %v", got, ok)
	}

	dup := domain.NodeRecord{InstanceID: "computeinstance-d", NodeGroupID: "ng-1", ClusterID: "c-1", Name: "n-a"}
	if err := r.Put(ctx, dup); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("Put(duplicate name) = %v, want AlreadyExists", err)
	}

	list, err := r.List(ctx, "ng-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(list, recs[:2]) {
		t.Fatalf("List(ng-1) = %+v, want creation order %+v", list, recs[:2])
	}

	if err := r.Delete(ctx, "computeinstance-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "computeinstance-b"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if list, _ := r.List(ctx, "ng-1"); len(list) != 1 || list[0].InstanceID != "computeinstance-a" {
		t.Fatalf("List after delete = %+v", list)
	}
}
