package dal_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/kms/dal"
)

const parent = "project-1"

func symKey(id, name string, state kmspb.KeyState) *kmspb.SymmetricKey {
	return &kmspb.SymmetricKey{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Status:   &kmspb.SymmetricKeyStatus{State: state},
	}
}

func asymKey(id, name string, state kmspb.KeyState) *kmspb.AsymmetricKey {
	return &kmspb.AsymmetricKey{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Status:   &kmspb.AsymmetricKeyStatus{State: state},
	}
}

func TestSymmetricRoundTrip(t *testing.T) {
	t.Parallel()
	r := dal.NewSymmetricKeyRepo(store.NewMemory())
	ctx := t.Context()
	k := symKey("k1", "a", kmspb.KeyState_ACTIVE)
	if err := r.Create(ctx, k, []byte("secret")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "k1")
	if err != nil || !proto.Equal(got, k) {
		t.Fatalf("Get = %v, %v", got, err)
	}
	byName, err := r.GetByName(ctx, parent, "a")
	if err != nil || byName.GetMetadata().GetId() != "k1" {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	m, err := r.Material(ctx, "k1")
	if err != nil || !bytes.Equal(m, []byte("secret")) {
		t.Fatalf("Material err = %v", err)
	}
	if err := r.Create(ctx, k, []byte("x")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("recreate err = %v, want AlreadyExists", err)
	}
}

func TestAsymmetricRoundTrip(t *testing.T) {
	t.Parallel()
	r := dal.NewAsymmetricKeyRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, asymKey("a1", "n", kmspb.KeyState_ACTIVE), []byte("m")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := r.GetByName(ctx, parent, "n"); err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if err := r.Create(ctx, asymKey("a2", "n", kmspb.KeyState_ACTIVE), []byte("m")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("dup err = %v, want AlreadyExists", err)
	}
}

func TestKindsAreIsolated(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	sym, asym := dal.NewSymmetricKeyRepo(st), dal.NewAsymmetricKeyRepo(st)
	if err := sym.Create(t.Context(), symKey("k1", "same", kmspb.KeyState_ACTIVE), []byte("m")); err != nil {
		t.Fatal(err)
	}
	if err := asym.Create(t.Context(), asymKey("k2", "same", kmspb.KeyState_ACTIVE), []byte("m")); err != nil {
		t.Fatalf("same name across kinds: %v", err)
	}
	if _, err := asym.Get(t.Context(), "k1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("cross-kind Get err = %v, want NotFound", err)
	}
}

func TestMissing(t *testing.T) {
	t.Parallel()
	r := dal.NewSymmetricKeyRepo(store.NewMemory())
	ctx := t.Context()
	tests := []struct {
		name string
		call func() error
	}{
		{"Get", func() error { _, err := r.Get(ctx, "nope"); return err }},
		{"GetByName", func() error { _, err := r.GetByName(ctx, parent, "nope"); return err }},
		{"Update", func() error {
			return r.Update(ctx, symKey("nope", "x", kmspb.KeyState_ACTIVE), symKey("nope", "x", kmspb.KeyState_ACTIVE))
		}},
		{"Material", func() error { _, err := r.Material(ctx, "nope"); return err }},
		{"UpdateWithMaterial", func() error {
			return r.UpdateWithMaterial(ctx, symKey("nope", "x", kmspb.KeyState_ACTIVE), []byte("m"), symKey("nope", "x", kmspb.KeyState_ACTIVE))
		}},
		{"Delete", func() error { return r.Delete(ctx, symKey("nope", "x", kmspb.KeyState_ACTIVE)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(); !errs.IsKind(err, errs.KindNotFound) {
				t.Fatalf("err = %v, want NotFound", err)
			}
		})
	}
}

func TestListPaging(t *testing.T) {
	t.Parallel()
	r := dal.NewSymmetricKeyRepo(store.NewMemory())
	ctx := t.Context()
	for _, id := range []string{"k1", "k2", "k3"} {
		if err := r.Create(ctx, symKey(id, id, kmspb.KeyState_ACTIVE), []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := r.List(ctx, parent, 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := r.List(ctx, parent, 2, p1.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" || p2.Items[0].GetMetadata().GetId() != "k3" {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	if _, err := r.List(ctx, parent, 2, "%%%"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token err = %v, want BadRequest", err)
	}
	other, err := r.List(ctx, "project-2", 10, "")
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("other parent = %+v, %v", other, err)
	}
}

func TestScheduledKeyFreesName(t *testing.T) {
	t.Parallel()
	r := dal.NewSymmetricKeyRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, symKey("k1", "a", kmspb.KeyState_ACTIVE), []byte("m")); err != nil {
		t.Fatal(err)
	}
	active := symKey("k1", "a", kmspb.KeyState_ACTIVE)
	scheduled := symKey("k1", "a", kmspb.KeyState_SCHEDULED_FOR_DELETION)
	if err := r.Update(ctx, scheduled, active); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := r.GetByName(ctx, parent, "a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByName scheduled err = %v, want NotFound", err)
	}
	if _, err := r.Get(ctx, "k1"); err != nil {
		t.Fatalf("Get scheduled: %v", err)
	}
	if err := r.Create(ctx, symKey("k2", "a", kmspb.KeyState_ACTIVE), []byte("m")); err != nil {
		t.Fatalf("reuse name: %v", err)
	}
	// Undelete while the name is taken must fail.
	if err := r.Update(ctx, active, scheduled); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("undelete err = %v, want AlreadyExists", err)
	}
	// Once the name is free again, undelete re-indexes it.
	if err := r.Delete(ctx, symKey("k2", "a", kmspb.KeyState_ACTIVE)); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, active, scheduled); err != nil {
		t.Fatalf("undelete: %v", err)
	}
	if k, err := r.GetByName(ctx, parent, "a"); err != nil || k.GetMetadata().GetId() != "k1" {
		t.Fatalf("GetByName after undelete = %v, %v", k, err)
	}
}

func TestMaterialLifecycle(t *testing.T) {
	t.Parallel()
	r := dal.NewSymmetricKeyRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, symKey("k1", "a", kmspb.KeyState_ACTIVE), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	created := symKey("k1", "a", kmspb.KeyState_ACTIVE)
	rotated := symKey("k1", "a", kmspb.KeyState_ACTIVE)
	rotated.Metadata.ResourceVersion = 1
	if err := r.UpdateWithMaterial(ctx, rotated, []byte("v2"), created); err != nil {
		t.Fatalf("UpdateWithMaterial: %v", err)
	}
	if m, err := r.Material(ctx, "k1"); err != nil || string(m) != "v2" {
		t.Fatalf("Material err = %v", err)
	}
	if k, err := r.Get(ctx, "k1"); err != nil || k.GetMetadata().GetResourceVersion() != 1 {
		t.Fatalf("key after UpdateWithMaterial = %v, %v", k, err)
	}
	if err := r.Delete(ctx, rotated); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Material(ctx, "k1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Material after delete err = %v, want NotFound", err)
	}
	if _, err := r.Get(ctx, "k1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete err = %v, want NotFound", err)
	}
	// Material records must not leak into listings.
	if err := r.Create(ctx, symKey("k2", "b", kmspb.KeyState_ACTIVE), []byte("m")); err != nil {
		t.Fatal(err)
	}
	if p, err := r.List(ctx, parent, 10, ""); err != nil || len(p.Items) != 1 {
		t.Fatalf("List = %+v, %v", p, err)
	}
}

// TestWritesCompareAndSwap: every write of an existing key fails with
// Conflict, changing nothing, unless the stored key still equals prev.
func TestWritesCompareAndSwap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	stale := symKey("k1", "a", kmspb.KeyState_ACTIVE)
	stale.Metadata.Labels = map[string]string{"old": "1"}
	next := symKey("k1", "a", kmspb.KeyState_SCHEDULED_FOR_DELETION)
	writes := map[string]func(r *dal.KeyRepo[*kmspb.SymmetricKey]) error{
		"Update": func(r *dal.KeyRepo[*kmspb.SymmetricKey]) error { return r.Update(ctx, next, stale) },
		"UpdateWithMaterial": func(r *dal.KeyRepo[*kmspb.SymmetricKey]) error {
			return r.UpdateWithMaterial(ctx, next, []byte("v2"), stale)
		},
		"Delete": func(r *dal.KeyRepo[*kmspb.SymmetricKey]) error { return r.Delete(ctx, stale) },
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := dal.NewSymmetricKeyRepo(store.NewMemory())
			stored := symKey("k1", "a", kmspb.KeyState_ACTIVE) // differs from stale only in labels
			if err := r.Create(ctx, stored, []byte("v1")); err != nil {
				t.Fatal(err)
			}
			if err := write(r); !errs.IsKind(err, errs.KindConflict) {
				t.Fatalf("err = %v, want Conflict", err)
			}
			k, err := r.Get(ctx, "k1")
			if err != nil || !proto.Equal(k, stored) {
				t.Fatalf("key after conflict = %v, %v; want unchanged", k, err)
			}
			if m, err := r.Material(ctx, "k1"); err != nil || string(m) != "v1" {
				t.Fatalf("material after conflict = %q, %v; want v1", m, err)
			}
		})
	}
}
