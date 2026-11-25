---
name: go-patterns
description: Data structures and design patterns for NebiusMocker Go code — slices/maps semantics and aliasing, sets, heaps, iterators, generic containers, functional options, constructors, strategy/backends, middleware/decorator (interceptors), registry, state machines and table-driven dispatch. Use when choosing a data structure, designing a type's API, adding a pluggable backend, writing an interceptor, or implementing a lifecycle.
---

# Data structures and design patterns

Go favors a few patterns done plainly. If a pattern needs a diagram to explain, it is probably the wrong one here.
For concurrency patterns (fan-out, pipelines, pools) see the `go-concurrency` skill.

## Data structures

### Slices
- `make([]T, 0, n)` when the final size is known; `slices.Grow` before bulk appends.
- **Aliasing**: `b := a[:2]` shares the backing array; `append(b, x)` may overwrite `a[2]`. When returning a
  sub-slice that callers may append to, use a full slice expression `a[:2:2]` or `slices.Clone`.
- Never return internal slices from a struct; return `slices.Clone(s.items)` or an `iter.Seq`.
- Removing while iterating: `slices.DeleteFunc`. Dedup: `slices.Sort` + `slices.Compact`.
- `nil` and empty slices are equivalent for callers; don't distinguish them in APIs (proto repeated fields don't).

### Maps
- Iteration order is random — **never** let it leak into output, IDs, or test expectations. Sort keys:
  `for _, k := range slices.Sorted(maps.Keys(m))`. This matters for deterministic List ordering and snapshots.
- Set: `map[K]struct{}`; use a tiny generic `set.Set[K]` in `internal/set` rather than repeating the idiom.
- Maps are not safe for concurrent writes: a mutex per owning struct.
- Pre-size with `make(map[K]V, n)` when n is known; `clear(m)` to reuse.

### Other structures we need
| Need | Structure |
|---|---|
| Operation scheduler due-queue | `container/heap` over a slice of `(due, opID, step)` |
| Secondary indexes in the memory store | `map[indexKey]map[id]struct{}`; ordered scans via a sorted slice or `github.com/google/btree` (generic `BTreeG`) |
| CIDR / IP allocation | `net/netip` + an interval bitmap per subnet; never string arithmetic on IPs |
| Paged List results | stable sort by `(created_at, id)`; opaque cursor = last `(created_at, id)` signed |
| Bounded FIFO / journal | ring buffer (fixed slice + head index) |
| Streaming store reads | `iter.Seq2[*Record, error]` — no channels for iteration |

### Value vs pointer
- Small immutable structs (≤ ~4 words: IDs, keys, `netip.Prefix`) by value. Anything with a mutex, large, or
  mutated by methods by pointer. Proto messages are always `*T`.
- Don't take the address of a range variable to store it; build the value and append.

## Design patterns

### Constructor + functional options
Required dependencies are positional; optional knobs are options. Validate in the constructor.

```go
type Option func(*config)

func WithTiming(p lifecycle.Profile) Option { return func(c *config) { c.timing = p } }
func WithRetention(d time.Duration) Option  { return func(c *config) { c.retention = d } }

func NewEngine(st store.Store, clk clock.Clock, opts ...Option) (*Engine, error) {
    cfg := config{timing: lifecycle.Fast, retention: 7 * 24 * time.Hour}
    for _, o := range opts {
        o(&cfg)
    }
    if cfg.retention <= 0 {
        return nil, fmt.Errorf("operations: retention must be positive, got %s", cfg.retention)
    }
    return &Engine{st: st, clk: clk, cfg: cfg}, nil
}
```

Use a plain config struct instead when the type is configured from YAML/env (`pkg/config` → struct → constructor).

### Strategy / pluggable backend
Backends (mk8s `mock|k3s|external`, storage `memory|bolt`, S3 `embedded|minio`) are an interface in the
consumer's package plus one implementation per sub-package, chosen once in the composition root.

```go
// services/mk8s/domain/ports.go
type ClusterBackend interface {
    Provision(ctx context.Context, c *mk8spb.Cluster) (Endpoints, error)
    Destroy(ctx context.Context, clusterID string) error
}
```

No `switch backendName` anywhere except the composition root.

### Decorator / middleware
gRPC interceptors and repository decorators (caching, metrics) wrap an interface and preserve its contract.
Order is explicit in one place (`pkg/edge` chain). A decorator must not change semantics silently
(a caching repo must invalidate on writes it sees).

### Registry
Modules register with the server through an explicit list in `pkg/mocker` / `cmd/*` — **not** via `init()` side
effects. A registry is a struct you construct, not a package-level map.

### State machine (table-driven)
Lifecycles are **data**, interpreted by `pkg/lifecycle`: states, transitions, guards, durations, hooks. Adding a
verb means adding a table row, not a `switch` in a handler.

```go
var transitions = map[verb][]State{ // allowed current states per verb
    verbStart: {StateStopped},
    verbStop:  {StateRunning},
}
```

Make illegal states unrepresentable where cheap: typed enums, constructors that validate, unexported fields.

### Table-driven dispatch over long `switch`
For mapping kinds (domain error kind → gRPC code + detail builder; NID prefix → module), use a map or slice of
structs. Pair with `exhaustive` so new enum values can't be forgotten.

### Adapter
`pkg/peer` adapters satisfy domain ports (`SubnetLookup`) using either an in-process call or a gRPC client. The
domain never knows which.

## Patterns we avoid

- Deep embedding hierarchies, "base" structs, abstract factories, getters/setters for every field.
- Global singletons and `init()` registration.
- Generic repository interfaces with 15 methods "for all entities" — each domain port lists only what it uses.
- Channels as iterators; reflection where generics or code generation work; `interface{}`/`any` in domain APIs.
