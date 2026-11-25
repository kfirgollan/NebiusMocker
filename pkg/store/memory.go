package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

type idKey struct{ kind, id string }

// Memory is an in-memory Store. Writes inside Update are staged and applied
// atomically on success. It is safe for concurrent use.
type Memory struct {
	mu      sync.RWMutex
	records map[idKey]Record
	seq     uint64
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{records: map[idKey]Record{}}
}

// View runs fn with a read-only snapshot guarantee (writes return an error).
func (m *Memory) View(ctx context.Context, fn func(tx Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return fn(&memTx{m: m, readOnly: true})
}

// Update runs fn in a serializable transaction.
func (m *Memory) Update(ctx context.Context, fn func(tx Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tx := &memTx{m: m, puts: map[idKey]Record{}, dels: map[idKey]struct{}{}}
	if err := fn(tx); err != nil {
		return err
	}
	tx.commitLocked()
	return nil
}

type memTx struct {
	m        *Memory
	readOnly bool
	puts     map[idKey]Record
	dels     map[idKey]struct{}
	seq      uint64 // highest seq allocated in this tx
}

func (t *memTx) lookup(k idKey) (Record, bool) {
	if _, gone := t.dels[k]; gone {
		return Record{}, false
	}
	if r, ok := t.puts[k]; ok {
		return r, true
	}
	r, ok := t.m.records[k]
	return r, ok
}

func (t *memTx) Get(kind, id string) (Record, error) {
	r, ok := t.lookup(idKey{kind, id})
	if !ok {
		return Record{}, fmt.Errorf("%s %s: %w", kind, id, ErrNotFound)
	}
	return cloneRecord(r), nil
}

func (t *memTx) ByName(kind, parentID, name string) (Record, error) {
	for _, r := range t.visible(kind, parentID) {
		if r.Name == name {
			return cloneRecord(r), nil
		}
	}
	return Record{}, fmt.Errorf("%s %q in %s: %w", kind, name, parentID, ErrNotFound)
}

func (t *memTx) List(q ListQuery) ([]Record, error) {
	all := t.visible(q.Kind, q.ParentID)
	slices.SortFunc(all, func(a, b Record) int { return compareSeq(a.Seq, b.Seq) })
	out := make([]Record, 0, len(all))
	for _, r := range all {
		if r.Seq <= q.AfterSeq {
			continue
		}
		out = append(out, cloneRecord(r))
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (t *memTx) Put(rec Record) (Record, error) {
	if t.readOnly {
		return Record{}, errors.New("store: Put in read-only transaction")
	}
	if rec.Kind == "" || rec.ID == "" {
		return Record{}, errors.New("store: record kind and id are required")
	}
	k := idKey{rec.Kind, rec.ID}
	if prev, ok := t.lookup(k); ok {
		rec.Seq = prev.Seq
	} else {
		t.seq = max(t.seq, t.m.seq) + 1
		rec.Seq = t.seq
	}
	if rec.Name != "" {
		for _, other := range t.visible(rec.Kind, rec.ParentID) {
			if other.Name == rec.Name && other.ID != rec.ID {
				return Record{}, &NameTakenError{Kind: rec.Kind, ParentID: rec.ParentID, Name: rec.Name, ExistingID: other.ID}
			}
		}
	}
	delete(t.dels, k)
	t.puts[k] = cloneRecord(rec)
	return cloneRecord(rec), nil
}

func (t *memTx) Delete(kind, id string) error {
	if t.readOnly {
		return errors.New("store: Delete in read-only transaction")
	}
	k := idKey{kind, id}
	if _, ok := t.lookup(k); !ok {
		return fmt.Errorf("%s %s: %w", kind, id, ErrNotFound)
	}
	delete(t.puts, k)
	t.dels[k] = struct{}{}
	return nil
}

// visible returns the records of kind under parentID as seen by this tx.
func (t *memTx) visible(kind, parentID string) []Record {
	var out []Record
	for k, r := range t.m.records {
		if k.kind != kind || r.ParentID != parentID {
			continue
		}
		if _, gone := t.dels[k]; gone {
			continue
		}
		if _, staged := t.puts[k]; staged {
			continue
		}
		out = append(out, r)
	}
	for _, r := range t.puts {
		if r.Kind == kind && r.ParentID == parentID {
			out = append(out, r)
		}
	}
	return out
}

func (t *memTx) commitLocked() {
	for k := range t.dels {
		delete(t.m.records, k)
	}
	for k, r := range t.puts {
		t.m.records[k] = r
	}
	t.m.seq = max(t.m.seq, t.seq)
}

func cloneRecord(r Record) Record {
	r.Data = slices.Clone(r.Data)
	return r
}

func compareSeq(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
