package domain

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// fakeRepo is an in-memory Repository[T]: names are unique per parent and
// listings come back in creation order, paged with the last returned sequence
// number as the token.
type fakeRepo[T Resource] struct {
	mu    sync.Mutex
	seq   int
	items []fakeItem[T]
}

type fakeItem[T Resource] struct {
	seq int
	v   T
}

func newFakeRepo[T Resource]() *fakeRepo[T] { return &fakeRepo[T]{} }

func clone[T Resource](v T) T {
	c, _ := proto.Clone(v).(T)
	return c
}

func (f *fakeRepo[T]) find(id string) int {
	return slices.IndexFunc(f.items, func(it fakeItem[T]) bool { return it.v.GetMetadata().GetId() == id })
}

func (f *fakeRepo[T]) nameTaken(parentID, name, exceptID string) (string, bool) {
	for _, it := range f.items {
		md := it.v.GetMetadata()
		if md.GetParentId() == parentID && md.GetName() == name && md.GetId() != exceptID {
			return md.GetId(), true
		}
	}
	return "", false
}

func (f *fakeRepo[T]) Get(_ context.Context, id string) (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(id)
	if i < 0 {
		var zero T
		return zero, errs.NotFound(id)
	}
	return clone(f.items[i].v), nil
}

func (f *fakeRepo[T]) GetByName(_ context.Context, parentID, name string) (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.items {
		md := it.v.GetMetadata()
		if md.GetParentId() == parentID && md.GetName() == name {
			return clone(it.v), nil
		}
	}
	var zero T
	return zero, errs.NotFound(name)
}

func (f *fakeRepo[T]) page(match func(T) bool, pageSize int, pageToken string) (Page[T], error) {
	after := 0
	if pageToken != "" {
		n, err := strconv.Atoi(pageToken)
		if err != nil {
			return Page[T]{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed"})
		}
		after = n
	}
	var (
		out  Page[T]
		last int
	)
	for _, it := range f.items {
		if it.seq <= after || !match(it.v) {
			continue
		}
		if pageSize > 0 && len(out.Items) == pageSize {
			out.NextPageToken = strconv.Itoa(last)
			return out, nil
		}
		out.Items = append(out.Items, clone(it.v))
		last = it.seq
	}
	return out, nil
}

func (f *fakeRepo[T]) List(_ context.Context, parentID string, pageSize int, pageToken string) (Page[T], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.page(func(v T) bool { return v.GetMetadata().GetParentId() == parentID }, pageSize, pageToken)
}

func (f *fakeRepo[T]) ListWhere(_ context.Context, match func(T) bool, pageSize int, pageToken string) (Page[T], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.page(match, pageSize, pageToken)
}

func (f *fakeRepo[T]) All(_ context.Context, match func(T) bool) ([]T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.page(match, 0, "")
	return p.Items, err
}

func (f *fakeRepo[T]) Create(_ context.Context, r T) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := r.GetMetadata()
	if f.find(md.GetId()) >= 0 {
		return errs.AlreadyExists(md.GetId(), "id taken")
	}
	if existing, taken := f.nameTaken(md.GetParentId(), md.GetName(), ""); taken {
		return errs.AlreadyExists(existing, "name taken")
	}
	f.seq++
	f.items = append(f.items, fakeItem[T]{seq: f.seq, v: clone(r)})
	return nil
}

func (f *fakeRepo[T]) Update(_ context.Context, r T) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := r.GetMetadata()
	i := f.find(md.GetId())
	if i < 0 {
		return errs.NotFound(md.GetId())
	}
	if existing, taken := f.nameTaken(md.GetParentId(), md.GetName(), md.GetId()); taken {
		return errs.AlreadyExists(existing, "name taken")
	}
	f.items[i].v = clone(r)
	return nil
}

func (f *fakeRepo[T]) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// inlineOps runs Prepare then Work synchronously, like a zero-delay engine.
// A Prepare error is returned from Submit; a Work error ends up in Operation.Err.
type inlineOps struct {
	mu sync.Mutex
	n  int
}

func (o *inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	o.mu.Lock()
	o.n++
	id := fmt.Sprintf("operation-e00test%04d", o.n)
	o.mu.Unlock()
	op := &operations.Operation{ID: id, Description: s.Description, ResourceID: s.ResourceID, Request: s.Request}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

// seqIDs hands out "<kind>-e00test<counter>" IDs.
type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (s *seqIDs) New(typ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("%s-e00test%04d", typ, s.n), nil
}

// fakeHierarchy knows a fixed set of IDs, all roots.
type fakeHierarchy struct{ known map[string]bool }

func (h fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if !h.known[id] {
		return "", errs.NotFound(id)
	}
	return "", nil
}
