package store

import (
	"errors"
	"testing"
)

func put(t *testing.T, m *Memory, rec Record) {
	t.Helper()
	err := m.Update(t.Context(), func(tx Tx) error {
		_, err := tx.Put(rec)
		return err
	})
	if err != nil {
		t.Fatalf("put %s: %v", rec.ID, err)
	}
}

func TestMemoryNameUniquePerParent(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	put(t, m, Record{Kind: "group", ID: "g1", ParentID: "t1", Name: "admins"})
	put(t, m, Record{Kind: "group", ID: "g2", ParentID: "t2", Name: "admins"}) // other parent: allowed

	err := m.Update(t.Context(), func(tx Tx) error {
		_, err := tx.Put(Record{Kind: "group", ID: "g3", ParentID: "t1", Name: "admins"})
		return err
	})
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: err = %v, want ErrNameTaken", err)
	}
	nt, ok := errors.AsType[*NameTakenError](err)
	if !ok || nt.ExistingID != "g1" {
		t.Fatalf("NameTakenError = %+v, want ExistingID g1", nt)
	}
}

func TestMemoryRollbackOnError(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	boom := errors.New("boom")
	err := m.Update(t.Context(), func(tx Tx) error {
		if _, err := tx.Put(Record{Kind: "group", ID: "g1", ParentID: "t1"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	err = m.View(t.Context(), func(tx Tx) error {
		_, err := tx.Get("group", "g1")
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("record survived a failed transaction: %v", err)
	}
}

func TestMemoryListOrderAndCursor(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	for _, id := range []string{"c", "a", "b"} { // insertion order, not lexical
		put(t, m, Record{Kind: "group", ID: id, ParentID: "t1"})
	}
	var first, rest []Record
	err := m.View(t.Context(), func(tx Tx) error {
		var err error
		if first, err = tx.List(ListQuery{Kind: "group", ParentID: "t1", Limit: 1}); err != nil {
			return err
		}
		rest, err = tx.List(ListQuery{Kind: "group", ParentID: "t1", AfterSeq: first[0].Seq})
		return err
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := []string{first[0].ID}
	for _, r := range rest {
		got = append(got, r.ID)
	}
	if want := []string{"c", "a", "b"}; !equal(got, want) {
		t.Fatalf("list order = %v, want %v", got, want)
	}
}

func TestMemoryUpdateKeepsSeqAndRenames(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	put(t, m, Record{Kind: "group", ID: "g1", ParentID: "t1", Name: "old"})
	put(t, m, Record{Kind: "group", ID: "g1", ParentID: "t1", Name: "new"})
	put(t, m, Record{Kind: "group", ID: "g2", ParentID: "t1", Name: "old"}) // freed name is reusable
	err := m.View(t.Context(), func(tx Tx) error {
		r, err := tx.ByName("group", "t1", "new")
		if err != nil {
			return err
		}
		if r.ID != "g1" || r.Seq != 1 {
			t.Errorf("renamed record = %+v, want g1 with seq 1", r)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestMemoryReadOnlyView(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	err := m.View(t.Context(), func(tx Tx) error {
		_, err := tx.Put(Record{Kind: "group", ID: "g1"})
		return err
	})
	if err == nil {
		t.Fatal("Put inside View succeeded, want error")
	}
}

func TestMemoryListAnyKind(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	put(t, m, Record{Kind: "vpcnetwork", ID: "n1", ParentID: "p1"})
	put(t, m, Record{Kind: "bucket", ID: "b1", ParentID: "p1"})
	put(t, m, Record{Kind: "bucket", ID: "b2", ParentID: "p2"})
	var got []string
	err := m.Update(t.Context(), func(tx Tx) error {
		// Staged writes and deletes of the same transaction are visible.
		if _, err := tx.Put(Record{Kind: "secret", ID: "s1", ParentID: "p1"}); err != nil {
			return err
		}
		if err := tx.Delete("vpcnetwork", "n1"); err != nil {
			return err
		}
		recs, err := tx.List(ListQuery{ParentID: "p1", AnyKind: true})
		for _, r := range recs {
			got = append(got, r.Kind+"/"+r.ID)
		}
		return err
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := []string{"bucket/b1", "secret/s1"}; !equal(got, want) {
		t.Fatalf("AnyKind list = %v, want %v", got, want)
	}
	err = m.View(t.Context(), func(tx Tx) error {
		_, err := tx.List(ListQuery{AnyKind: true, AnyParent: true})
		return err
	})
	if err == nil {
		t.Fatal("AnyKind with AnyParent: want an error")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
