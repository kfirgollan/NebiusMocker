---
name: go-architecture
description: Layering, package layout and dependency rules for NebiusMocker Go code — api (views) / domain (logic) / dal (data access) separation, consumer-defined interfaces as test seams, constructor injection, and what each layer may import. Use when creating or moving a package, adding a service module, adding a dependency, deciding where a piece of logic belongs, or reviewing a change that crosses layers.
---

# Go architecture for NebiusMocker

Read `docs/architecture/03-repository-layout.md` for the repository tree. This skill is the **rulebook** for
where code goes and who may import what. The rules are enforced by `.golangci.yml` (depguard) and the
architecture test `test/arch` — see the `go-lint` skill. If a rule here blocks you, change the design, not the linter.

## The three layers inside every service module

```text
services/<area>/
├── module.go          # composition root for the area: builds dal → domain → api, implements mocker.Module
├── api/               # VIEWS. gRPC handlers. proto ⇄ domain conversion, error mapping. No decisions.
│   ├── zz_generated_handlers.go
│   └── instance_handler.go
├── domain/            # LOGIC. Validation, state machines, quotas, side effects. Pure Go + interfaces.
│   ├── instance.go
│   ├── ports.go       # interfaces this layer NEEDS (repositories, peers, clock) — defined here, by the consumer
│   └── errors.go      # domain error kinds (wrap pkg/errs)
└── dal/               # DATA ACCESS. The only code that touches pkg/store (and, through it, any database).
    └── instance_repo.go   # implements domain.InstanceRepository
```

Dependency direction is strictly **api → domain ← dal**. `domain` imports neither.

| Layer | Allowed | Forbidden |
|---|---|---|
| `api` | `domain`, gosdk proto + grpc types, `pkg/serviceerror`, `pkg/interceptors` helpers | `dal`, `pkg/store`, any DB driver, business rules |
| `domain` | stdlib, gosdk proto **messages as data**, `pkg/errs`, `pkg/clock`, `pkg/lifecycle`, `pkg/resource`, `pkg/quota`, `pkg/events` interfaces | `google.golang.org/grpc/...` (incl. `codes`, `status`, `metadata`), `pkg/serviceerror`, `pkg/store`, `dal`, `net/http`, `os.Getenv` |
| `dal` | `domain` (own area only), `pkg/store`, `pkg/errs` | grpc, `pkg/serviceerror`, `api`, other areas |
| `module.go` | all three of its own area, `pkg/mocker` | other areas |

Cross-area access (compute needs a vpc subnet) goes through `pkg/peer.Directory`, declared as a port in
`domain/ports.go`. **`services/A` never imports `services/B`.**

### What "no logic in api" means concretely

An api handler does exactly four things, in order:

```go
func (h *InstanceHandler) Create(ctx context.Context, req *computepb.CreateInstanceRequest) (*commonpb.Operation, error) {
    cmd, err := toCreateInstanceCmd(req)          // 1. convert proto → domain input (pure mapping)
    if err != nil {
        return nil, serviceerror.FromDomain(ctx, err)  //    conversion errors are BadRequest
    }
    op, err := h.svc.CreateInstance(ctx, cmd)     // 2. call exactly one domain method
    if err != nil {
        return nil, serviceerror.FromDomain(ctx, err)  // 3. map domain error → gRPC status + ServiceError
    }
    return toOperationProto(op), nil              // 4. convert domain → proto
}
```

Red flags in an api file: `if` on business state, loops over resources, calls to two domain methods, quota or
permission checks, time arithmetic, direct `Get`/`Put` on storage. Move them into `domain`.

Proto messages are the data model of this project (we emulate an API whose schema *is* the protos), so `domain`
may hold and return `*computepb.Instance`. What `domain` must not know is **transport**: status codes, metadata,
headers, reset-mask parsing. The interceptors and `api` layer translate those into plain domain inputs.

## Interfaces are seams, defined by the consumer

- Define an interface **in the package that uses it** (`domain/ports.go`), not next to the implementation.
  `dal` returns concrete types; `domain` accepts interfaces. ("Accept interfaces, return structs.")
- Keep them small — one to four methods, named for the role: `InstanceRepository`, `SubnetLookup`, `Clock`.
- Never export an interface "just in case". An interface earns its place when there are two implementations
  (real + fake counts) or when it breaks an import cycle.
- Add a compile-time check next to each implementation: `var _ domain.InstanceRepository = (*InstanceRepo)(nil)`.
- Domain unit tests use **in-package fakes** (`domain/fakes_test.go`) — map-backed, deterministic, a few lines each.
  Generated mocks (`go.uber.org/mock`) are allowed only for interfaces with more than four methods.

```go
// domain/ports.go
type InstanceRepository interface {
    Get(ctx context.Context, id string) (*computepb.Instance, error)
    Create(ctx context.Context, inst *computepb.Instance) error
    Update(ctx context.Context, inst *computepb.Instance, expectVersion int64) error
    Delete(ctx context.Context, id string) error
}

type SubnetLookup interface { // satisfied by a pkg/peer adapter; faked in tests
    Subnet(ctx context.Context, id string) (*vpcpb.Subnet, error)
}
```

## Construction and wiring

- Constructor injection only: `domain.NewInstanceService(repo, subnets, clock, quota) *InstanceService`.
  No package-level mutable state, no `init()` that registers things, no service locators, no DI frameworks.
- Required dependencies are constructor parameters; optional knobs are functional options (see `go-patterns`).
- Wiring happens in exactly two places: `services/<area>/module.go` (per area) and `pkg/mocker` (process).
- A constructor validates its inputs and returns `(*T, error)` if it can fail; never panics on bad config.

## Shared `pkg/` layers

| Package | Layer role |
|---|---|
| `pkg/store` | Storage engine. **The only importer of `go.etcd.io/bbolt` is `pkg/store/bolt`.** |
| `pkg/resource` | Generic domain framework; depends on a `Repository` port, never on `pkg/store` directly |
| `pkg/edge`, `pkg/interceptors`, `pkg/serviceerror` | Transport (api side) |
| `pkg/admin` | Transport (HTTP admin API); same "views only" rule |
| `pkg/errs`, `pkg/clock`, `pkg/lifecycle`, `pkg/quota`, `pkg/events` | Domain-side building blocks; no transport imports |
| `pkg/mocker`, `cmd/*` | Composition roots |
| `pkg/testkit` | Test wiring; may import anything in `pkg/` |

`pkg/*` never imports `services/*`. `internal/` is for helpers that must not become public API.

## Package design rules

- Name packages for what they provide (`quota`, `nid`), never `util`, `common`, `helpers`, `base`, `models`.
- One package = one responsibility you can state in a sentence. Split when a package needs a second sentence.
- Avoid stutter: `quota.Ledger`, not `quota.QuotaLedger`.
- Keep `main` packages tiny: parse config, call `mocker.Main`, exit with its code.
- Unexported by default. Exporting is a promise; `pkg/` is public API once we tag v1.

## When adding a new service area

1. Create `services/<area>/{module.go,api,domain,dal}` following an existing area.
2. Declare ports in `domain/ports.go`; implement them in `dal` or via `pkg/peer`.
3. Run `make lint` — depguard and `test/arch` pick up the new area automatically via globs.
4. Add the conformance fixtures (see `go-testing`).

More detail and worked examples: [references/layering-examples.md](references/layering-examples.md).
