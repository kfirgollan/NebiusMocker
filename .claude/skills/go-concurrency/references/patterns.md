# Concurrency patterns — reference implementations

Each pattern: when to use it, the canonical shape, and the trap it avoids. All examples honor `ctx`, are bounded,
and have a single owner that waits.

## 1. Pipeline (stages connected by channels)

Use for streaming transforms where stages have different costs (snapshot export: iterate → marshal → compress → write).

```go
func pipeline(ctx context.Context, src iter.Seq[*store.Record], w io.Writer) error {
    g, ctx := errgroup.WithContext(ctx)
    recs := make(chan *store.Record)
    blobs := make(chan []byte)

    g.Go(func() error { // stage 1: produce
        defer close(recs)
        for r := range src {
            select {
            case recs <- r:
            case <-ctx.Done():
                return context.Cause(ctx)
            }
        }
        return nil
    })
    g.Go(func() error { // stage 2: transform
        defer close(blobs)
        for r := range recs {
            b, err := marshal(r)
            if err != nil {
                return fmt.Errorf("marshal %s: %w", r.ID, err)
            }
            select {
            case blobs <- b:
            case <-ctx.Done():
                return context.Cause(ctx)
            }
        }
        return nil
    })
    g.Go(func() error { // stage 3: sink
        for b := range blobs {
            if _, err := w.Write(b); err != nil {
                return fmt.Errorf("write: %w", err)
            }
        }
        return nil
    })
    return g.Wait()
}
```

Trap avoided: a failing sink would otherwise block stage 2 forever on `blobs <- b`. The `ctx.Done()` arms
plus errgroup cancellation unblock every stage.

## 2. Fan-out / fan-in with a fixed worker pool

Use when work items arrive as a stream (not a known slice) and per-item cost is similar.

```go
func process(ctx context.Context, in <-chan Item, workers int, fn func(context.Context, Item) (Result, error)) (<-chan Result, func() error) {
    out := make(chan Result)
    g, ctx := errgroup.WithContext(ctx)
    for range workers {
        g.Go(func() error {
            for {
                select {
                case it, ok := <-in:
                    if !ok {
                        return nil
                    }
                    r, err := fn(ctx, it)
                    if err != nil {
                        return err
                    }
                    select {
                    case out <- r:
                    case <-ctx.Done():
                        return context.Cause(ctx)
                    }
                case <-ctx.Done():
                    return context.Cause(ctx)
                }
            }
        })
    }
    wait := sync.OnceValue(func() error { err := g.Wait(); close(out); return err })
    go func() { _ = wait() }() // closes out when workers finish; caller reads out, then calls wait()
    return out, wait
}
```

For a **known slice**, prefer the simpler `errgroup` + `SetLimit` + index-write form in SKILL.md.
Fan-in ordering: results arrive in completion order; if order matters, carry the index and reorder, or write by index.

## 3. Bounded parallelism with a semaphore (no pool)

```go
sem := make(chan struct{}, limit)
for _, x := range xs {
    select {
    case sem <- struct{}{}:
    case <-ctx.Done():
        return context.Cause(ctx)
    }
    wg.Go(func() {
        defer func() { <-sem }()
        handle(ctx, x)
    })
}
wg.Wait()
```

`errgroup.SetLimit` is the same thing with error propagation — prefer it.

## 4. Pub/sub event bus (pkg/events)

Requirements: publishers never block on slow subscribers; per-subscriber ordering preserved; overflow is visible.

```go
type subscriber struct {
    ch      chan Event // bounded, e.g. 256
    dropped atomic.Int64
}

func (b *Bus) Publish(e Event) {
    b.mu.RLock()
    defer b.mu.RUnlock()
    for _, s := range b.subs {
        select {
        case s.ch <- e:
        default:
            s.dropped.Add(1) // never block the publisher; surfaced in /_mocker/health
        }
    }
}
```

Each subscriber's goroutine is owned by the Bus and drained on `Stop`. Cross-module **correctness** side effects
(cascade deletes, IP release) must not rely on best-effort delivery: do them synchronously in the domain transaction,
and use the bus only for observers (journal, metrics, ListOperationsByParent index).

## 5. Single-goroutine scheduler (pkg/operations)

```go
for {
    next, ok := s.heap.Peek()
    var timer <-chan time.Time
    if ok {
        timer = s.clock.After(next.due.Sub(s.clock.Now()))
    }
    select {
    case <-timer:
        s.runDue(ctx)            // pops and executes all due steps, each in its own store tx
    case j := <-s.enqueue:      // new step from an RPC handler
        s.heap.Push(j)
    case <-ctx.Done():
        return
    }
}
```

One goroutine owns the heap: no locks on it. Handlers communicate by sending on `enqueue`. With `clock.Manual`,
tests advance time and assert state deterministically.

## 6. singleflight (dedupe concurrent identical work)

Use for remote peer lookups and authz decision fetches in split mode, where many requests ask for the same key.

```go
v, err, _ := s.group.Do(key, func() (any, error) { return s.fetch(ctx, key) })
```

Caveat: the first caller's `ctx` governs the shared call; use `DoChan` and select on your own ctx if callers have
different deadlines.

## 7. Rate limiting

`golang.org/x/time/rate.Limiter` per (principal, service) key in a mutex-guarded map with idle eviction. Use
`limiter.Allow()` in interceptors (fail fast with `TooManyRequests`), not `Wait` (which would queue requests).
Inject a clock-compatible limiter for tests.

## 8. Timeouts and retries

- Per-call timeout: `ctx, cancel := context.WithTimeout(ctx, d); defer cancel()` — always `defer cancel()`.
- Retries: bounded attempts, exponential backoff with jitter, retry only errors whose `retry_type` is `CALL`, stop on
  `ctx.Done()`. Delays through `clock.Clock`.

## Anti-patterns

| Anti-pattern | Fix |
|---|---|
| `go func() { … }()` in a handler with no wait | errgroup / owned worker |
| `wg.Add(1)` inside the goroutine | `wg.Go` |
| `time.Sleep` to wait for a goroutine | channel, `synctest.Wait`, or condition polling with deadline |
| Closing a channel from the receiver | sender closes; receiver uses ctx |
| Holding a mutex while calling out (store, peer, callback, channel send) | copy data, unlock, then call |
| `select {}` / `for {}` busy loops | block on channels/ctx |
| Unbounded `make(chan T, 100000)` "to be safe" | 0/1 buffer + backpressure, or bounded + drop with metric |
| Goroutine per operation/timer | the single scheduler goroutine |
| Capturing loop variables by pointer to mutate shared slices without index ownership | write by index, or mutex |
