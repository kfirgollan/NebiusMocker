---
name: go-style
description: Go function and code style for NebiusMocker — function shape and signatures, naming, receivers, early returns, context and options parameters, zero values, comments, and modern Go 1.26 idioms. Use whenever writing or reviewing any Go code in this repository.
---

# Go style for NebiusMocker

Baseline: [Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments),
the [Google Go Style Guide](https://google.github.io/styleguide/go/) and the [Uber Go Style Guide](https://github.com/uber-go/guide).
Where they disagree, this file wins. Formatting is not a discussion: `gofumpt` + `gci` via `make fmt`.

The module targets **Go 1.26**. Use the modern standard library; do not hand-roll what it provides.

## Functions

- **Signature order**: `ctx context.Context` first, then required inputs, then `opts ...Option` last.
  Return `(value, error)`; error is always the last result.
- **Do one thing.** If a function needs a comment like `// step 2:` inside, extract step 2.
  Target ≤ 50 lines and ≤ 4 levels of nesting; the linters (`funlen`, `gocognit`, `nestif`) enforce the outer bound.
- **Early return, happy path at the left margin.** Handle the error/edge case and `return`; never `else` after a `return`.

  ```go
  disk, err := s.disks.Get(ctx, id)
  if err != nil {
      return nil, fmt.Errorf("get disk %s: %w", id, err)
  }
  if disk.GetStatus().GetState() != computepb.DiskStatus_READY {
      return nil, errs.BadState(id, "disk is not READY")
  }
  return s.attach(ctx, disk, inst)
  ```

- **No naked returns.** Named results are allowed only to document two same-typed results
  (`func split(cidr string) (network, broadcast netip.Addr, err error)`) or for a deferred error wrap.
- **No boolean flag parameters** (`create(x, true)`). Use two functions, an enum type, or an options struct.
- **More than ~4 parameters** → a params/command struct (`CreateInstanceCmd`).
- **Accept interfaces, return concrete types.** Return `*InstanceService`, not `InstanceServicer`.
- **Pure where possible.** Push I/O (store, peers, clock) to the edges; keep the decision code pure and table-testable.
- **No `init()`** except to register `go:embed`-free, side-effect-free constants. No package-level mutable state.
- **Generics** for genuinely type-parametric code (`resource.Handler[T proto.Message]`, `set.Set[T]`).
  Not for "might be useful later". If only one type instantiates it, write the concrete version.

## Receivers and types

- Pointer receivers for anything with state or that is large; be consistent per type (`recvcheck` enforces).
- Receiver name: one or two letters from the type (`s *InstanceService`, `r *DiskRepo`), never `this`/`self`.
- Make the **zero value useful** or make it unusable: unexported fields + a `New...` constructor.
- Don't embed types in exported structs to "inherit" methods; it leaks the embedded API. Compose with a named field.
- Enums: `type State int` with an `iota` block whose zero value is `StateUnspecified`; switch on them with
  `exhaustive` linting.

## Naming

- `MixedCaps`; initialisms keep case: `ID`, `URL`, `NID`, `CIDR`, `IAM`, `GRPC` (`subnetID`, `parseNID`).
- Getters have no `Get` prefix (`Owner()`), except where we implement a proto-named RPC (`Get`, `GetByName`).
- Name length scales with scope: `i` in a 3-line loop, `pendingOps` across a function, `ErrQuotaExceeded` package-wide.
- Error values `ErrX`, error types `XError`. Interfaces by role (`SubnetLookup`), `-er` only for single-method ones.

## Protobuf messages

- **Always use getters** (`req.GetMetadata().GetParentId()`), never field access on messages you received —
  getters are nil-safe. `protogetter` enforces it. Field writes on messages you construct are fine.
- Copy with `proto.Clone`, compare with `proto.Equal` / `protocmp.Transform()` in tests; never `*msg = *other`.
- Marshal deterministically when bytes are stored or compared (`proto.MarshalOptions{Deterministic: true}`).

## Context

- Every function that does I/O, blocks, or calls something that might takes `ctx` and honors cancellation.
- Never store a `context.Context` in a struct (`containedctx`), never pass `nil` (`context.TODO()` only in tests'
  scaffolding, and `context.Background()` only in `main` and `pkg/mocker`; `forbidigo` enforces).
- `context.WithoutCancel(ctx)` for work that must outlive the request (operation completion); keep its values.
- Context values only for request-scoped metadata (principal, request ID), behind typed accessor functions.

## Time, randomness, environment

- Never call `time.Now`, `time.Sleep`, `time.After`, `time.NewTimer` outside `pkg/clock` — inject `clock.Clock`.
  This is what makes lifecycles and token expiry testable. (`forbidigo`.)
- Never `os.Getenv` outside `pkg/config`. Never `math/rand` (v1); use `math/rand/v2` with an injected source where
  determinism matters (ID generation in `sequential`/`seeded` mode).

## Modern standard library (use these)

| Use | Instead of |
|---|---|
| `slices.Sort`, `slices.Contains`, `slices.IndexFunc`, `maps.Keys` + `slices.Sorted` | hand-written loops, `sort.Slice` |
| `min`, `max`, `clear` builtins | helper funcs |
| `for i := range n` | `for i := 0; i < n; i++` |
| `iter.Seq[T]` / `iter.Seq2[K,V]` for store iterators | callback-with-bool or channel iterators |
| `errors.Join`, `errors.AsType[E]` (1.26) | multi-error libs, `var target *E; errors.As(err, &target)` |
| `sync.WaitGroup.Go(f)` (1.25) | `wg.Add(1); go func(){ defer wg.Done() … }()` |
| `log/slog` with attrs | `log`, `fmt.Printf` |
| `net/netip` (`netip.Prefix`, `netip.Addr`) | `net.IP`, `net.IPNet` (IPAM code) |
| `new(expr)` (1.26), e.g. `Limit: new(uint64(100))` | `ptr()` helpers |
| `t.Context()`, `b.Loop()`, `testing/synctest` | `context.Background()` in tests, `for i := 0; i < b.N; i++` |
| `go fix ./...` modernizers | manual upgrades |

## Comments and docs

- Every exported identifier has a doc comment starting with its name, saying **what and why**, not how.
- Package comment in `doc.go` for every package in `pkg/` and every service area.
- Comments explaining Nebius behavior cite the source: `// Upstream: nebius/api README § Operations`.
- `TODO(username): …` with a link to an issue; no anonymous TODOs.

## Logging

- `*slog.Logger` injected, never the global default in library code.
- Static message + structured attrs: `log.InfoContext(ctx, "operation finished", "op_id", id, "duration", d)`.
  `sloglint` enforces consistent key style (snake_case) and attr-only usage.
- Never log secrets: fields annotated `sensitive`/`credentials` go through the redacting helpers in `pkg/edge`.
