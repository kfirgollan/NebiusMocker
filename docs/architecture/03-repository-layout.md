# 03 — Repository layout

## One Go module, public libraries

The repository is a single Go module, `github.com/kfirgollan/NebiusMocker`, with an importable `pkg/` tree.
Other projects can then embed NebiusMocker in their own Go tests, the way people use `testcontainers` or the
`localstack` Go client, without running a container:

```go
srv := mocker.New(mocker.WithServices("iam", "vpc", "compute"), mocker.WithAuthz(authz.Off))
addr := srv.StartT(t)   // random port, h2c, cleaned up with t.Cleanup
sdk, _ := gosdk.New(ctx,
    gosdk.WithResolvers(conn.NewBasicResolver("*", addr)),
    gosdk.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
    gosdk.WithCredentials(gosdk.IAMToken(srv.AdminToken())),
)
```

We use one module rather than a multi-module workspace because the shared libraries and the services change together
during early development. Splitting later is cheap if versioning needs diverge ([ADR-0003](decisions/0003-single-go-module.md)).

## Tree

```text
.
├── cmd/
│   ├── nebius-mocker/            # all-in-one binary (all modules linked; SERVICES selects what is enabled)
│   ├── nebius-mocker-iam/        # single-module binaries: thin main.go → mocker.Main(iam.Module)
│   ├── nebius-mocker-vpc/
│   ├── nebius-mocker-compute/
│   ├── nebius-mocker-mk8s/
│   ├── …                         # one per area in docs/services
│   └── tfnebiuslocal/            # Terraform wrapper (like LocalStack's tflocal)
│
├── pkg/                          # PUBLIC, importable libraries (semver once v1)
│   ├── mocker/                   # Server assembly: options, module registry, lifecycle, Main()
│   ├── edge/                     # gRPC listener(s), TLS/h2c, reflection, health, per-service listeners
│   ├── interceptors/             # request-id, logging, faults, authn, ratelimit, authz, validate, idempotency, resetmask
│   ├── resource/                 # generic resource framework (CRUD over metadata/spec/status messages)
│   │   ├── nid/                  #   ID generation (wraps gosdk/nid), deterministic mode
│   │   ├── resetmask/            #   Update semantics (wraps gosdk/proto/fieldmask PatchWithResetMask)
│   │   ├── behavior/             #   IMMUTABLE / OUTPUT_ONLY / INPUT_ONLY / NON_EMPTY_DEFAULT enforcement from annotations
│   │   └── paging/               #   page_token encoding, page_size limits, filters
│   ├── operations/               # operation engine, scheduler, OperationService (v1 and v1alpha1)
│   ├── lifecycle/                # declarative state-machine definitions and timing profiles
│   ├── store/                    # Store interface + memory and bolt backends, snapshots
│   ├── events/                   # in-process event bus
│   ├── auth/                     # token minting/verification, JWT exchange, static keys
│   ├── authz/                    # permission catalog, role → permission mapping, decision engine
│   ├── quota/                    # quota ledger and limit catalog
│   ├── faults/                   # fault-injection rule engine
│   ├── errs/                     # transport-free domain error kinds (NotFound, BadState, QuotaExceeded, …)
│   ├── serviceerror/             # api-side mapping: errs kinds → google.rpc.Status + nebius.common.v1.ServiceError
│   ├── clock/                    # the only package allowed to touch real time; Manual clock for tests
│   ├── config/                   # the only package allowed to read the environment
│   ├── peer/                     # cross-module lookup interface (in-process and remote gRPC implementations)
│   ├── admin/                    # admin HTTP API (health, state, reset, snapshots, faults, clock)
│   ├── seed/                     # declarative seed data loader (YAML)
│   └── testkit/                  # helpers for users' Go tests: StartT, SDK client factory, fixtures, assertions
│
├── services/                     # one package per Nebius API area; each exports `Module`
│   │                             #   every area: module.go + api/ (views) + domain/ (logic) + dal/ (data access)
│   ├── iam/                      #   tenants, projects, SAs, groups, permits, keys, token exchange
│   ├── vpc/
│   ├── compute/
│   ├── mk8s/
│   │   └── backend/{mock,k3s}/   #   pluggable cluster backends
│   ├── storage/
│   │   └── s3/                   #   optional S3 data plane (T3)
│   ├── registry/
│   ├── mysterybox/
│   ├── kms/
│   ├── dns/
│   ├── quotas/
│   ├── billing/
│   ├── capacity/
│   ├── msp/{postgresql,mlflow}/
│   ├── ai/
│   ├── applications/
│   ├── audit/
│   ├── logging/
│   ├── maintenance/
│   └── tunnel/
│
├── internal/                     # private helpers (codegen templates, build info)
├── tools/
│   ├── apigen/                   # reads nebius/api → service tables in docs, module skeletons, coverage report
│   └── tfcoverage/               # reads terraform-provider-nebius → RPCs used per TF resource
│
├── test/                         # the test harness (see 12-test-harness.md)
│   ├── contract/                 # gosdk-client conformance tests per service
│   ├── terraform/                # TF scenarios: configs + expectations, run with real terraform binary
│   ├── parity/                   # recorded real-Nebius interactions + replay assertions
│   └── e2e/                      # container topology tests (docker compose)
│
├── deploy/
│   ├── docker/                   # Dockerfile (multi-target: all-in-one + per service)
│   ├── compose/                  # example compose files for each topology
│   └── helm/                     # (later) chart for running in k8s CI
│
└── docs/
```

## The `Module` contract

Every service package exports a value implementing:

```go
package mocker

type Module interface {
    // Name is the area name used in NEBIUS_MOCKER_SERVICES, e.g. "compute".
    Name() string

    // Services lists the fully qualified gRPC services this module serves,
    // e.g. "nebius.compute.v1.DiskService". Used for routing, reflection and coverage reports.
    Services() []string

    // Kinds declares resource kinds: NID prefix, proto message, parent kinds,
    // lifecycle machine, permissions, quota bindings. The framework reads this to
    // build generic CRUD, authz and quota behavior.
    Kinds() []resource.Kind

    // Register wires gRPC handlers onto the server. Handlers are usually generated
    // adapters around resource.Handler[T] plus a few hand-written custom verbs.
    Register(s grpc.ServiceRegistrar, deps Deps) error

    // Seed returns default data (platforms, public images, regions, roles…).
    Seed() []seed.Item

    // Start/Stop for background work (e.g. mk8s k3s backend reconciliation).
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}

type Deps struct {
    Store      store.Store
    Ops        *operations.Engine
    Events     events.Bus
    Quota      quota.Ledger
    Authz      authz.Engine
    Peers      peer.Directory   // look up resources owned by other modules (local or remote)
    Clock      clock.Clock      // controllable; admin API can advance time
    IDs        nid.Generator
    Config     config.View
    Log        *slog.Logger
}
```

A typical service is mostly declarative. For example, `vpc.NetworkService`:

```go
var networkKind = resource.Kind{
    Prefix:      "vpcnetwork",
    Message:     (*vpcpb.Network)(nil),
    Parents:     []string{"project"},
    Lifecycle:   lifecycle.Simple("CREATING", "READY", "DELETING"),
    Permissions: authz.CRUD("vpc.networks"),
    Quotas:      []quota.Binding{{Name: "vpc.network.count", Per: quota.Project, Amount: quota.One}},
    Hooks: resource.Hooks[*vpcpb.Network]{
        BeforeCreate: validatePools,         // domain rule: pools must exist in same project
        BeforeDelete: refuseIfSubnetsExist,  // referential integrity
    },
}
```

## Code generation

We use code generation only where the upstream surface is large and mechanical:

| Generator | Input | Output | Committed? |
|---|---|---|---|
| `tools/apigen` | `nebius/api` protos (via `gosdk` descriptors) | `services/<area>/zz_generated_handlers.go`: adapters that route each standard RPC (`Get`, `GetByName`, `List`, `Create`, `Update`, `Delete`) to `resource.Handler[T]`, and `UNIMPLEMENTED` stubs for everything else | Yes |
| `tools/apigen docs` | same | API tables in `docs/services/*.md` | Yes |
| `tools/tfcoverage` | `terraform-provider-nebius/generated` | `docs/services/terraform-coverage.md` and the M1 test matrix | Yes |

**Message types and gRPC stubs are not generated here.** They are imported from `github.com/nebius/gosdk/proto/...`, which
already contains `*_grpc.pb.go` with `XxxServiceServer` interfaces and `RegisterXxxServiceServer` functions. Pinning
`gosdk` in `go.mod` pins the API version we emulate ([ADR-0002](decisions/0002-reuse-gosdk-protos.md)).

## Layers inside a service module

Every area follows the same three-layer split ([ADR-0008](decisions/0008-layered-service-modules.md)):

```text
services/<area>/
├── module.go   # composition root: builds dal → domain → api, implements mocker.Module
├── api/        # views: gRPC handlers, proto ⇄ domain conversion, error mapping. No business decisions.
├── domain/     # logic: validation, lifecycles, quotas, side effects. Declares its ports (interfaces) in ports.go.
└── dal/        # data access: the only code that touches pkg/store. Implements domain ports.
```

Dependency direction is `api → domain ← dal`. Domain and DAL are transport-free: they return `pkg/errs` kinds, and only
`api` maps them to gRPC status + `ServiceError` via `pkg/serviceerror`. Other adapters, such as mk8s cluster backends or
the S3 data plane, implement domain ports and follow the DAL rules. The full rulebook is the
[`go-architecture` skill](../../.claude/skills/go-architecture/SKILL.md).

## Dependency rules

These are enforced by `.golangci.yml` (depguard + forbidigo) and the `test/arch` architecture test. See the
[`go-lint` skill](../../.claude/skills/go-lint/SKILL.md).

- Databases and storage engines (`go.etcd.io/bbolt`, `database/sql`, drivers) are imported only under `pkg/store/**`.
- `pkg/store` is imported only by `services/*/dal`, `services/*/module.go` and the composition roots.
- `pkg/*` never imports `services/*`.
- `services/A` never imports `services/B`. Cross-area access goes through `peer.Directory`, so split deployments keep working.
- Only `cmd/*` and `pkg/mocker` know the full module list.
- Third-party dependencies need a short justification in the PR. Expected core set: `google.golang.org/grpc`,
  `github.com/nebius/gosdk`, `buf.build/go/protovalidate`, `go.etcd.io/bbolt`, `github.com/golang-jwt/jwt/v5`,
  `log/slog` (stdlib).
