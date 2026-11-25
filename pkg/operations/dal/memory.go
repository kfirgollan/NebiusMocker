// Package dal stores operations for pkg/operations.
package dal

import (
	"context"
	"slices"
	"sync"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// Memory is an in-memory operations.Repository.
type Memory struct {
	mu  sync.RWMutex
	ops map[string]*operations.Operation
}

var _ operations.Repository = (*Memory)(nil)

// NewMemory returns an empty repository.
func NewMemory() *Memory {
	return &Memory{ops: map[string]*operations.Operation{}}
}

// Put inserts or replaces op.
func (m *Memory) Put(_ context.Context, op *operations.Operation) error {
	c := *op
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops[op.ID] = &c
	return nil
}

// Get returns a copy of the operation, or errs.NotFound.
func (m *Memory) Get(_ context.Context, id string) (*operations.Operation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	op, ok := m.ops[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	c := *op
	return &c, nil
}

// ListByResource returns copies of the resource's operations, newest first.
func (m *Memory) ListByResource(_ context.Context, resourceID string) ([]*operations.Operation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*operations.Operation
	for _, op := range m.ops {
		if op.ResourceID == resourceID {
			c := *op
			out = append(out, &c)
		}
	}
	slices.SortFunc(out, func(a, b *operations.Operation) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return compareStrings(b.ID, a.ID)
	})
	return out, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
