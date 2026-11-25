---
name: go-testing
description: Testing rules for NebiusMocker Go code — what to test at each layer (domain unit tests with fakes, dal tests on the memory store, api conversion tests, contract kit, Terraform scenarios), table-driven tests, fakes vs mocks, protocmp comparisons, golden files, synctest for time, goleak, -race, and fuzzing. Use when writing or reviewing tests, adding a resource kind, or fixing a bug (write the failing test first).
---

# Testing for NebiusMocker

The harness design is in `docs/architecture/12-test-harness.md`. This skill covers how to write the Go tests.

## What to test where

| Layer | Test type | Dependencies | Speed |
|---|---|---|---|
| `domain` | Unit, table-driven | In-package **fakes** for every port; `clock.Manual` | µs |
| `dal` | Integration with the real store | `store.NewMemory()` (and bolt in a `t.TempDir()` for one smoke test per repo) | ms |
| `api` | Conversion + error mapping | Fake domain service; assert proto output, gRPC code, `ServiceError` detail type | µs |
| `pkg/*` | Unit + property/fuzz where parsing | none | µs–ms |
| whole module | Contract kit (`test/contract`) via gosdk over a real in-process gRPC server | `pkg/testkit.StartT` | ms |
| tool level | Terraform scenarios (`test/terraform`) | real `terraform` + provider | s |

Bug fix = a failing test at the **lowest** layer that reproduces it, then the fix.

## Table-driven tests

```go
func TestValidateDiskSize(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name      string
        sizeBytes int64
        block     int64
        wantKind  errs.Kind // zero = no error
    }{
        {name: "multiple of block", sizeBytes: 8192, block: 4096},
        {name: "not a multiple", sizeBytes: 5000, block: 4096, wantKind: errs.KindBadRequest},
        {name: "zero", sizeBytes: 0, block: 4096, wantKind: errs.KindBadRequest},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            err := validateDiskSize(tt.sizeBytes, tt.block)
            if got := errs.KindOf(err); got != tt.wantKind {
                t.Fatalf("validateDiskSize(%d, %d) kind = %v, want %v (err: %v)", tt.sizeBytes, tt.block, got, tt.wantKind, err)
            }
        })
    }
}
```

- `t.Parallel()` in tests and subtests unless they share state (`tparallel`/`paralleltest` style is enforced by review).
- Failure messages: `Func(args) = got, want want`. Use `t.Fatalf` when continuing is meaningless, `t.Errorf` otherwise.
- Helpers call `t.Helper()` (`thelper`). Use `t.Context()` (cancelled when the test ends), `t.TempDir()`, `t.Cleanup()`.

## Assertions and comparisons

- Stdlib `testing` + `github.com/google/go-cmp/cmp`. For protos: `cmp.Diff(want, got, protocmp.Transform())`;
  ignore server-generated fields with `protocmp.IgnoreFields(&commonpb.ResourceMetadata{}, "id", "created_at", …)`.
- Never compare protos with `reflect.DeepEqual` or `==`.
- Errors: domain tests assert `errs.KindOf(err)`; api/contract tests assert `status.Code(err)` and the
  `ServiceError` detail **type**. Don't assert full message strings except where parity requires exact wording.

## Fakes over mocks

```go
type fakeDiskRepo struct {
    mu    sync.Mutex
    disks map[string]*computepb.Disk
}

func (f *fakeDiskRepo) Get(_ context.Context, id string) (*computepb.Disk, error) {
    f.mu.Lock()
    defer f.mu.Unlock()
    d, ok := f.disks[id]
    if !ok {
        return nil, errs.NotFound(id)
    }
    return proto.CloneOf(d), nil
}
```

- Fakes live in `*_test.go` (or `internal/fakes` if shared within an area), behave like the real thing, and are
  simple enough to be obviously correct.
- Assert on **resulting state and outputs**, not on call sequences. Interaction-style mocks (`go.uber.org/mock`) only
  for ports with more than four methods or when the interaction *is* the behavior (e.g. "peer called once").
- Every `dal` implementation and its fake can share a **port conformance test** (`domaintest.RunInstanceRepoSuite(t, newRepo)`)
  so fakes can't drift from reality.

## Time and concurrency

- Inject `clock.Clock`; in tests use `clock.NewManual(t0)` and `Advance(d)` for lifecycle/expiry logic.
- For code that uses timers internally or spawns goroutines, use `testing/synctest`:
  `synctest.Test(t, func(t *testing.T) { …; synctest.Wait(); … })`. No `time.Sleep` in tests, ever.
- `TestMain(m) { goleak.VerifyTestMain(m) }` in any package that starts goroutines.
- `make test` = `go test -race -shuffle=on -count=1 ./...`.

## Golden files

Generated docs tables, error mapping tables and snapshot formats use golden files under `testdata/`, updated
with `go test ./... -update`. Golden output must be deterministic (sorted keys, sequential IDs, manual clock).

## Fuzzing

Fuzz every parser of untrusted input: reset-mask syntax, NIDs, page tokens, CIDR inputs, token strings.
`func FuzzParseResetMask(f *testing.F)` with seeds from the upstream README examples. The seed corpus runs as normal
tests in `make test`; run real fuzzing locally when you touch a parser (`go test -fuzz=. -fuzztime=30s ./pkg/resource/resetmask`)
and commit any failing inputs it finds under `testdata/fuzz/`.

## Adding a resource kind — test checklist

- [ ] Domain rules: table tests with fakes, one row per rule and per error kind.
- [ ] DAL: repo test against `store.NewMemory()` + the shared port suite.
- [ ] API: conversion round-trip and error mapping.
- [ ] Contract kit fixture (`test/contract/fixtures/<kind>.go`) — the coverage test fails without it.
- [ ] Terraform scenario or example coverage if the provider exposes it.
