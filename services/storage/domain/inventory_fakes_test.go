package domain_test

import (
	"context"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// invRepo is a slice-backed domain.InventoryRepository.
type invRepo struct {
	mu     sync.Mutex
	items  []*storagepb.Inventory
	states map[string]domain.InventoryState
}

func newInvRepo() *invRepo { return &invRepo{states: map[string]domain.InventoryState{}} }

func (f *invRepo) find(pred func(*storagepb.Inventory) bool, key string) (*storagepb.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.items {
		if pred(i) {
			return proto.CloneOf(i), nil
		}
	}
	return nil, errs.NotFound(key)
}

func (f *invRepo) Get(_ context.Context, id string) (*storagepb.Inventory, error) {
	return f.find(func(i *storagepb.Inventory) bool { return i.GetMetadata().GetId() == id }, id)
}

func (f *invRepo) GetByName(_ context.Context, bucketID, name string) (*storagepb.Inventory, error) {
	return f.find(func(i *storagepb.Inventory) bool {
		return i.GetMetadata().GetParentId() == bucketID && i.GetMetadata().GetName() == name
	}, name)
}

func (f *invRepo) List(ctx context.Context, bucketID string, _ int, _ string) (domain.InventoryPage, error) {
	all, _ := f.ListAll(ctx)
	all = slices.DeleteFunc(all, func(i *storagepb.Inventory) bool { return i.GetMetadata().GetParentId() != bucketID })
	return domain.InventoryPage{Items: all}, nil
}

func (f *invRepo) ListAll(context.Context) ([]*storagepb.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*storagepb.Inventory, 0, len(f.items))
	for _, i := range f.items {
		out = append(out, proto.CloneOf(i))
	}
	return out, nil
}

func (f *invRepo) Count(ctx context.Context, bucketID string) (int, error) {
	p, _ := f.List(ctx, bucketID, 0, "")
	return len(p.Items), nil
}

func (f *invRepo) Create(_ context.Context, inv *storagepb.Inventory, st domain.InventoryState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, proto.CloneOf(inv))
	f.states[inv.GetMetadata().GetId()] = st
	return nil
}

func (f *invRepo) Update(_ context.Context, inv *storagepb.Inventory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.items {
		if e.GetMetadata().GetId() == inv.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(inv)
			return nil
		}
	}
	return errs.NotFound(inv.GetMetadata().GetId())
}

func (f *invRepo) State(_ context.Context, id string) (domain.InventoryState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[id]
	if !ok {
		return domain.InventoryState{}, errs.NotFound(id)
	}
	return st, nil
}

func (f *invRepo) SetState(_ context.Context, id string, st domain.InventoryState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.states[id]; !ok {
		return errs.NotFound(id)
	}
	f.states[id] = st
	return nil
}

func (f *invRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.items)
	f.items = slices.DeleteFunc(f.items, func(i *storagepb.Inventory) bool { return i.GetMetadata().GetId() == id })
	delete(f.states, id)
	if len(f.items) == n {
		return errs.NotFound(id)
	}
	return nil
}

// invWriter is a domain.ReportWriter that records its calls and fails with err.
type invWriter struct {
	mu    sync.Mutex
	calls []domain.ReportSource
	err   error
}

func (w *invWriter) Write(_ context.Context, src domain.ReportSource) (domain.InventoryReport, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, src)
	if w.err != nil {
		return domain.InventoryReport{}, w.err
	}
	return domain.InventoryReport{ManifestKey: "manifest.json", Objects: 1}, nil
}

func (w *invWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}
