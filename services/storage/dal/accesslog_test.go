package dal_test

import (
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/dal"
)

func TestAccessLogRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewAccessLogRepo(store.NewMemory())
	t1 := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	t2 := t1.Add(time.Hour)

	if _, ok, err := r.LastAccess(ctx, "b1", "k"); err != nil || ok {
		t.Fatalf("LastAccess before any read = ok %v, %v", ok, err)
	}
	for _, rec := range []struct {
		bucket, key string
		at          time.Time
	}{{"b1", "k", t1}, {"b1", "dir/a bü", t1}, {"b2", "k", t1}, {"b1", "k", t2}} {
		if err := r.Touch(ctx, rec.bucket, rec.key, rec.at); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok, err := r.LastAccess(ctx, "b1", "k"); err != nil || !ok || !got.Equal(t2) {
		t.Fatalf("LastAccess(b1,k) = %v %v %v, want %v", got, ok, err, t2)
	}
	if got, ok, err := r.LastAccess(ctx, "b1", "dir/a bü"); err != nil || !ok || !got.Equal(t1) {
		t.Fatalf("LastAccess(unicode key) = %v %v %v, want %v", got, ok, err, t1)
	}

	// Forget drops one bucket's records only.
	if err := r.Forget(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.LastAccess(ctx, "b1", "k"); ok {
		t.Fatal("b1 still has records after Forget")
	}
	if _, ok, _ := r.LastAccess(ctx, "b1", "dir/a bü"); ok {
		t.Fatal("b1 still has the unicode key after Forget")
	}
	if got, ok, err := r.LastAccess(ctx, "b2", "k"); err != nil || !ok || !got.Equal(t1) {
		t.Fatalf("Forget(b1) touched b2: %v %v %v", got, ok, err)
	}
}
