---
name: go-concurrency
description: Goroutine and concurrency rules for NebiusMocker — goroutine ownership and lifetimes, context cancellation, errgroup and WaitGroup.Go, fan-out/fan-in, pipelines, worker pools, bounded parallelism, channels vs mutexes, leak prevention with goleak and testing/synctest, and the race detector. Use whenever code starts a goroutine, uses channels, sync primitives, timers, or shares state between goroutines.
---

# Concurrency for NebiusMocker

Concurrency is a tool for **latency or throughput you have measured a need for**. An emulator is mostly
request/response over an in-memory store: most code should be plain sequential functions called concurrently
by the gRPC server. Reach for goroutines only in the places listed in "Where we do use goroutines".

## Ten rules

1. **Every goroutine has an owner, a way to stop, and a way to report.** The owner is a struct with
   `Start(ctx)`/`Stop(ctx)` (or a function that waits before returning). If you cannot say who waits for
   a goroutine, don't start it.
2. **Never fire-and-forget** `go f()` in request paths. Use `errgroup.Group`, `sync.WaitGroup.Go`, or hand the work
   to an owned worker (the operation scheduler).
3. **Pass `ctx`; select on `ctx.Done()`** in every blocking send, receive, or wait inside a goroutine.
   A send without a `ctx.Done()` arm is a leak waiting for a slow consumer.
4. **The sender closes the channel**, exactly once, and only when no more sends can happen. Receivers never close.
5. **Prefer `errgroup`** for "run N things, fail fast, wait for all":
   `g, ctx := errgroup.WithContext(ctx); g.SetLimit(n)` gives cancellation + bounded parallelism + first error.
6. **`sync.WaitGroup.Go(f)`** (Go 1.25) when tasks cannot fail. Never `wg.Add` inside the goroutine
   (`go vet`'s `waitgroup` analyzer flags it).
7. **Share memory by communicating — but a mutex is fine.** Protect a map or struct with `sync.Mutex`/`RWMutex`
   held for the shortest time, never across I/O, callbacks, or channel operations. Use channels to transfer
   *ownership* or signal events, not as a lock.
8. **Bound everything**: buffered channel sizes, worker counts, queue lengths, retries. Unbounded = OOM under load.
   Channel buffers are 0 or 1 unless you can justify the number in a comment.
9. **Recover at goroutine roots you own** (a panic in a goroutine kills the process): long-lived workers wrap their
   loop with a recover that logs and reports to the owner. Don't recover inside library code.
10. **Test with `-race` and `goleak`, time with `synctest`.** `make test` runs `go test -race`; every package that
    starts goroutines has `TestMain` with `goleak.VerifyTestMain(m)`.

## Where we do use goroutines

| Component | Pattern | Owner |
|---|---|---|
| gRPC / HTTP servers | one per listener | `pkg/edge` server, stopped on shutdown |
| Operation scheduler | **single goroutine** + min-heap + `clock.Clock` timers | `pkg/operations.Engine` |
| Event bus fan-out to subscribers | per-subscriber bounded queue + goroutine | `pkg/events.Bus` |
| Remote peer lookups (split mode) | `errgroup` fan-out with `SetLimit` | the calling domain method |
| Snapshot export | pipeline: list → marshal → write | `pkg/store` snapshot function |
| mk8s k3s backend | reconciler loop | `services/mk8s` module `Start/Stop` |

The single-goroutine scheduler is deliberate: lifecycle steps then never race each other, and ordering is
deterministic under the manual clock. Don't "speed it up" with a goroutine per operation.

## Shape of an owned worker

```go
type Worker struct {
    log   *slog.Logger
    jobs  chan job
    wg    sync.WaitGroup
    stop  context.CancelFunc
}

func (w *Worker) Start(ctx context.Context) {
    ctx, w.stop = context.WithCancel(ctx)
    w.wg.Go(func() { w.loop(ctx) })
}

func (w *Worker) Stop(ctx context.Context) error {
    w.stop()
    done := make(chan struct{})
    go func() { w.wg.Wait(); close(done) }()
    select {
    case <-done:
        return nil
    case <-ctx.Done():
        return fmt.Errorf("worker stop: %w", context.Cause(ctx))
    }
}

func (w *Worker) Submit(ctx context.Context, j job) error {
    select {
    case w.jobs <- j:
        return nil
    case <-ctx.Done():
        return context.Cause(ctx)
    }
}
```

## Fan-out / fan-in (bounded), the default form

```go
// lookupAll resolves ids concurrently, at most limit in flight, preserving input order.
func lookupAll(ctx context.Context, ids []string, limit int, get func(context.Context, string) (*Res, error)) ([]*Res, error) {
    out := make([]*Res, len(ids)) // each goroutine writes its own index: no mutex needed
    g, ctx := errgroup.WithContext(ctx)
    g.SetLimit(limit)
    for i, id := range ids {
        g.Go(func() error {
            r, err := get(ctx, id)
            if err != nil {
                return fmt.Errorf("lookup %s: %w", id, err)
            }
            out[i] = r
            return nil
        })
    }
    if err := g.Wait(); err != nil {
        return nil, err
    }
    return out, nil
}
```

Pipelines, worker pools, pub/sub, `singleflight`, semaphores, rate limiting and timer patterns are in
[references/patterns.md](references/patterns.md).

## Choosing a primitive

| Need | Use |
|---|---|
| Run N tasks, first error cancels rest | `errgroup.WithContext` (+ `SetLimit`) |
| Run N tasks that can't fail, wait | `sync.WaitGroup.Go` |
| Protect a map/struct | `sync.Mutex` (or `RWMutex` if reads ≫ writes and measured) |
| Counter / flag | `atomic.Int64`, `atomic.Bool`, `atomic.Pointer[T]` |
| One-time lazy init | `sync.OnceValue` / `sync.OnceValues` |
| Deduplicate concurrent identical calls | `golang.org/x/sync/singleflight` |
| Limit concurrency without a pool | buffered `chan struct{}` semaphore or `x/sync/semaphore` |
| Signal "done" to many | `close(ch)` or `ctx` cancellation |
| Cache shared across goroutines | mutex + map; `sync.Map` only for write-once/read-many keys |
| Delays/timeouts | `clock.Clock` (`After`, `NewTimer`) — never `time.*` directly |

## Testing concurrent code

- `testing/synctest` (Go 1.25): `synctest.Test(t, func(t *testing.T) { … })` runs the test in a bubble with a fake
  clock that jumps when all goroutines block; `synctest.Wait()` waits until they're blocked. Use it for timeout,
  retry and scheduler tests instead of sleeps.
- `go.uber.org/goleak` in `TestMain` for every package with goroutines.
- Never `time.Sleep` in a test to "let goroutines run". Wait on a channel, `synctest.Wait()`, or poll a condition
  with a deadline.
- Run `go test -race -count=1` before pushing; races are bugs even if "benign".
