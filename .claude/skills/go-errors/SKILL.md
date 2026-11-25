---
name: go-errors
description: Error handling for NebiusMocker Go code — wrapping with %w, sentinel vs typed errors, the pkg/errs domain error kinds, mapping to gRPC status and nebius ServiceError only in the api layer, errors.Is/AsType, panics, and errors in goroutines and operations. Use when writing code that creates, returns, wraps, inspects or logs errors, or when mapping errors to RPC responses.
---

# Error handling for NebiusMocker

## The model

```text
dal ──errs.NotFound(id)──▶ domain ──fmt.Errorf("…: %w")──▶ api ──serviceerror.FromDomain──▶ gRPC status
                                                                     + nebius.common.v1.ServiceError
```

- **`pkg/errs`** defines *what went wrong* as a small set of **kinds** that mirror the Nebius `ServiceError`
  details: `NotFound`, `AlreadyExists`, `BadRequest` (with field violations), `BadState`, `Conflict`,
  `OperationConflict`, `PermissionDenied`, `QuotaExceeded`, `TooManyRequests`, `NotEnoughResources`,
  `OutOfRange`, `Internal`. Constructors: `errs.NotFound(id)`, `errs.BadRequest(errs.Violation{Field, Msg})`, …
- **`domain` and `dal` return `pkg/errs` values (wrapped as needed). They never import `grpc/status` or
  `grpc/codes`.** depguard enforces this.
- **Only the `api` layer** (and interceptors) converts, using `serviceerror.FromDomain(ctx, err)`, which picks the
  gRPC code, attaches the `ServiceError` detail, sets `retry_type`, and adds request/trace IDs. Unknown errors become
  `INTERNAL` with `InternalError{request_id}` — and the original is logged, never sent to the client.
- Operations that fail asynchronously store the same mapping in `Operation.status`.

## Rules

1. **Handle every error exactly once.** Either return it (wrapped) *or* log it and degrade — never both.
   Logging-then-returning produces duplicate noise up the stack.
2. **Wrap with context using `%w`**: `fmt.Errorf("allocate address in subnet %s: %w", subnetID, err)`.
   - The context is *what this function was doing*, lowercase, no "failed to", no trailing punctuation.
   - Don't repeat what the callee already said; don't wrap when you add nothing (`return err` is fine then).
   - Use `%v` instead of `%w` only to deliberately hide an implementation error from callers' `errors.Is`.
3. **Inspect with `errors.Is` / `errors.AsType`**, never `==` on wrapped errors or string matching
   (`errorlint` enforces).

   ```go
   if errs.IsKind(err, errs.KindNotFound) { … }
   if qe, ok := errors.AsType[*errs.QuotaError](err); ok { log.Warn("quota", "quota", qe.Quota) }
   ```

4. **Sentinels** (`var ErrClosed = errors.New("store: closed")`) for conditions callers branch on and that carry
   no data. **Typed errors** (`*errs.QuotaError`) when callers need fields. Both are part of the package API — export
   only what callers must branch on.
5. **Validate at the boundary, trust inside.** `api` + protovalidate reject malformed input; `domain` enforces business
   invariants and returns `errs.BadRequest` with the **proto field path** (`spec.boot_disk.managed_disk.spec.size_bytes`)
   so Terraform shows the user which attribute is wrong.
6. **No `panic` for errors.** Panic only for programmer bugs that make continuing unsafe (impossible switch default,
   violated internal invariant), and in `Must…` helpers used at init with constant input. The gRPC server has a
   recovery interceptor that turns a panic into `INTERNAL` and logs the stack — it is a safety net, not a strategy.
7. **Errors in deferred calls** (`Close`, `Commit`) are checked; combine with `errors.Join` into the named result:

   ```go
   func (r *Repo) export(ctx context.Context, w io.Writer) (err error) {
       f, err := r.open()
       if err != nil {
           return fmt.Errorf("open snapshot: %w", err)
       }
       defer func() { err = errors.Join(err, f.Close()) }()
       …
   }
   ```

8. **Never ignore an error silently.** If ignoring is correct, write `_ = x.Close() // best-effort: read-only file`.
   `errcheck` enforces.
9. **`nil, nil` is not an answer.** A "not found" is `errs.NotFound`, not a nil pointer with a nil error (`nilnil`).
   Conversely don't return a non-nil value with a non-nil error unless documented (e.g. partial results).
10. **Messages are for humans, kinds are for code.** Client-visible messages must be stable and match Nebius wording
    where parity tests captured it; they live next to the error constructor, not scattered in handlers.

## Errors and concurrency

- Goroutines must return their error to an owner: `errgroup.Group` (first error cancels siblings) or a result
  channel. A goroutine that can fail and has nowhere to report is a bug.
- Use `context.Cause(ctx)` / `context.WithCancelCause` to carry *why* work was cancelled (shutdown vs. client
  disconnect vs. sibling failure).
- Collect independent failures (e.g. validating all fields) with `errors.Join` and report them together.

## Checklist for review

- [ ] No `status.Error` / `codes.` outside `api`, `pkg/serviceerror`, `pkg/interceptors`, `pkg/edge`.
- [ ] Every returned error is wrapped with useful context or deliberately passed through.
- [ ] No log-and-return.
- [ ] Field violations use proto field paths.
- [ ] Tests assert on kinds (`errs.IsKind`) in domain, and on gRPC code + `ServiceError` detail type in api/contract tests.
