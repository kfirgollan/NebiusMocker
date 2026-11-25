package operations

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// idType is the NID type prefix of operation IDs.
const idType = "operation"

// ErrNotStarted is returned by Submit for delayed operations before Start.
var ErrNotStarted = errors.New("operations: engine not started")

// Engine creates and completes operations.
type Engine struct {
	repo  Repository
	clk   clock.Clock
	ids   IDGenerator
	delay time.Duration
	log   *slog.Logger

	// onFinish observes every finished operation (set before Start).
	onFinish func(ctx context.Context, op *Operation)

	mu      sync.Mutex
	active  map[string]string // resource ID → running operation ID
	started bool

	enqueue chan job
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

type job struct {
	due  time.Time
	op   *Operation
	work func(ctx context.Context) error
}

// NewEngine returns an Engine. A zero delay completes every operation inline,
// before Submit returns; a positive delay completes operations asynchronously.
func NewEngine(repo Repository, clk clock.Clock, ids IDGenerator, delay time.Duration, log *slog.Logger) (*Engine, error) {
	if repo == nil || clk == nil || ids == nil || log == nil {
		return nil, errors.New("operations: repository, clock, id generator and logger are required")
	}
	if delay < 0 {
		return nil, fmt.Errorf("operations: delay must not be negative, got %s", delay)
	}
	return &Engine{
		repo: repo, clk: clk, ids: ids, delay: delay, log: log,
		active:  map[string]string{},
		enqueue: make(chan job),
	}, nil
}

// Start launches the scheduler goroutine. It must be paired with Stop.
func (e *Engine) Start(ctx context.Context) {
	ctx, e.stop = context.WithCancel(ctx)
	e.mu.Lock()
	e.started = true
	e.mu.Unlock()
	e.wg.Go(func() { e.loop(ctx) })
}

// Stop stops the scheduler and waits for it to exit. Pending operations stay
// unfinished, as they would after a real control-plane restart.
func (e *Engine) Stop() {
	if e.stop == nil {
		return
	}
	e.stop()
	e.wg.Wait()
	e.mu.Lock()
	e.started = false
	e.mu.Unlock()
}

// Submit runs spec.Prepare under the resource lock and creates the operation.
func (e *Engine) Submit(ctx context.Context, spec Spec) (*Operation, error) {
	if err := e.lock(spec.ResourceID); err != nil {
		return nil, err
	}
	op, err := e.prepare(ctx, spec)
	if err != nil {
		e.unlock(spec.ResourceID)
		return nil, err
	}
	if e.delay == 0 {
		e.complete(ctx, op, spec.Work)
		return op.clone(), nil
	}
	return e.schedule(ctx, op, spec.Work)
}

// Get returns the operation with the given ID.
func (e *Engine) Get(ctx context.Context, id string) (*Operation, error) {
	op, err := e.repo.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get operation %s: %w", id, err)
	}
	return op, nil
}

// ListByResource returns the operations of a resource, newest first.
func (e *Engine) ListByResource(ctx context.Context, resourceID string) ([]*Operation, error) {
	ops, err := e.repo.ListByResource(ctx, resourceID)
	if err != nil {
		return nil, fmt.Errorf("list operations of %s: %w", resourceID, err)
	}
	return ops, nil
}

func (e *Engine) lock(resourceID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if opID, busy := e.active[resourceID]; busy {
		return errs.OperationConflict(opID, resourceID)
	}
	e.active[resourceID] = "" // reserved; filled once the operation exists
	return nil
}

func (e *Engine) unlock(resourceID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.active, resourceID)
}

func (e *Engine) prepare(ctx context.Context, spec Spec) (*Operation, error) {
	if spec.Prepare != nil {
		if err := spec.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	id, err := e.ids.New(idType)
	if err != nil {
		return nil, fmt.Errorf("new operation id: %w", err)
	}
	op := &Operation{
		ID: id, Description: spec.Description, CreatedAt: e.clk.Now(), CreatedBy: spec.CreatedBy,
		ResourceID: spec.ResourceID, Request: spec.Request,
	}
	e.mu.Lock()
	e.active[spec.ResourceID] = id
	e.mu.Unlock()
	return op, nil
}

func (e *Engine) schedule(ctx context.Context, op *Operation, work func(context.Context) error) (*Operation, error) {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if !started {
		e.unlock(op.ResourceID)
		return nil, ErrNotStarted
	}
	if err := e.repo.Put(ctx, op.clone()); err != nil {
		e.unlock(op.ResourceID)
		return nil, fmt.Errorf("store operation %s: %w", op.ID, err)
	}
	j := job{due: e.clk.Now().Add(e.delay), op: op.clone(), work: work}
	select {
	case e.enqueue <- j:
		return op.clone(), nil
	case <-ctx.Done():
		// The operation is stored but will never complete; fail it so pollers stop.
		e.finish(context.WithoutCancel(ctx), j.op, context.Cause(ctx))
		return nil, context.Cause(ctx)
	}
}

// complete runs the work and records the result. Work errors fail the operation,
// they are not returned to the caller of Submit.
func (e *Engine) complete(ctx context.Context, op *Operation, work func(context.Context) error) {
	var err error
	if work != nil {
		err = work(ctx)
	}
	e.finish(ctx, op, err)
}

func (e *Engine) finish(ctx context.Context, op *Operation, workErr error) {
	op.FinishedAt = e.clk.Now()
	op.Err = workErr
	if err := e.repo.Put(ctx, op.clone()); err != nil {
		e.log.ErrorContext(ctx, "store finished operation", "op_id", op.ID, "error", err)
	}
	if workErr != nil {
		e.log.WarnContext(ctx, "operation failed", "op_id", op.ID, "resource_id", op.ResourceID, "error", workErr)
	}
	e.unlock(op.ResourceID)
	if e.onFinish != nil {
		e.onFinish(ctx, op.clone())
	}
}

// OnFinish registers fn to observe every operation when it finishes, after
// its result is stored. fn runs on the goroutine that completed the
// operation (the caller of Submit when the delay is zero), so it must be
// fast and must not call Submit. Call it before Start; there is one observer.
func (e *Engine) OnFinish(fn func(ctx context.Context, op *Operation)) {
	e.onFinish = fn
}

// loop is the single scheduler goroutine: it owns the due-heap, so no lock is needed on it.
func (e *Engine) loop(ctx context.Context) {
	var due jobHeap
	for {
		var timer <-chan time.Time
		if len(due) > 0 {
			timer = e.clk.After(due[0].due.Sub(e.clk.Now()))
		}
		select {
		case j := <-e.enqueue:
			heap.Push(&due, j)
		case <-timer:
			e.runDue(ctx, &due)
		case <-ctx.Done():
			return
		}
	}
}

func (e *Engine) runDue(ctx context.Context, due *jobHeap) {
	now := e.clk.Now()
	for due.Len() > 0 && !(*due)[0].due.After(now) {
		j := heap.Pop(due).(job) //nolint:forcetypeassert // jobHeap only holds job values
		e.complete(ctx, j.op, j.work)
	}
}

type jobHeap []job

func (h *jobHeap) Len() int           { return len(*h) }
func (h *jobHeap) Less(i, j int) bool { return (*h)[i].due.Before((*h)[j].due) }
func (h *jobHeap) Swap(i, j int)      { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
func (h *jobHeap) Push(x any)         { *h = append(*h, x.(job)) } //nolint:forcetypeassert // heap.Interface contract
func (h *jobHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
