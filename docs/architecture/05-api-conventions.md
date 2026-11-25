# 05 — API conventions

These are the cross-cutting rules of the Nebius API. The `pkg/resource` framework implements them once, so every service
gets them for free. The sources are the upstream [`README`](https://github.com/nebius/api/blob/main/README.md),
`nebius/common/v1/*.proto`, `nebius/annotations.proto` and `gosdk`.

## Resource shape

Nearly every resource is:

```proto
message Xxx {
  common.v1.ResourceMetadata metadata = 1;
  XxxSpec   spec   = 2;                               // desired state, user-writable
  XxxStatus status = 3 [(field_behavior) = OUTPUT_ONLY]; // observed state, server-owned
}

message ResourceMetadata {
  string id = 1 [(field_behavior) = IDENTIFIER];
  string parent_id = 2 [(buf.validate.field).required = true];
  string name = 3;                 // ≤1024 bytes
  int64  resource_version = 4;     // optimistic concurrency
  google.protobuf.Timestamp created_at = 5; // OUTPUT_ONLY
  google.protobuf.Timestamp updated_at = 6; // OUTPUT_ONLY
  map<string,string> labels = 7;
}
```

The framework works on any message that has `metadata` and `spec` fields, using protoreflect. `status` is optional,
because some resources (`AccessPermit`) have an empty status.

### Framework rules

| Rule | Behavior |
|---|---|
| `id` | Server-generated NID (below). A client-supplied `id` on Create → `INVALID_ARGUMENT`. |
| `parent_id` | Must reference an existing resource of an allowed parent kind (from `(nid).parent_resource` annotations or the `Kind.Parents` declaration). A missing parent → `NOT_FOUND` + `ResourceNotFound{parent_id}`. |
| `name` | Unique per (kind, parent_id) unless the message has `resource_behavior = UNNAMED`. A collision → `ALREADY_EXISTS` + `ResourceAlreadyExists{existing id}`. `IMMUTABLE_NAME` forbids renames. |
| `resource_version` | Starts at 1 and increments on every spec or metadata change. If an Update carries a non-zero version that differs from the stored one → `ABORTED` + `ResourceConflict`. |
| `created_at` / `updated_at` | From the controllable mock clock. |
| `labels` | Stored as-is. Validated against key/value limits if the service defines them. |
| `status` | Never writable. Values sent by clients are discarded silently, as the real API does. |
| `MOVABLE` | Resource can change `parent_id` via Update. Otherwise a parent change → `INVALID_ARGUMENT`. |

## Field behaviors (`nebius/annotations.proto`)

| Annotation | Enforcement |
|---|---|
| `IDENTIFIER` | Server-assigned, used for lookups |
| `IMMUTABLE` | Settable on Create. An Update that changes it → `INVALID_ARGUMENT` + `BadRequest{field, "field is immutable"}` |
| `OUTPUT_ONLY` | Stripped from incoming requests |
| `INPUT_ONLY` | Accepted on write, never returned on read (for example, initial passwords) |
| `NON_EMPTY_DEFAULT` | If unset on Create, the server fills a default. Each module provides the default value |
| `MEANINGFUL_EMPTY_VALUE` | An empty value is distinct from unset (`Operation.status`) |
| `sensitive` / `credentials` | Values are redacted in logs and the request journal. Returned only by the RPCs designed to reveal them (`AccessKeyService.GetSecret`, `PayloadService.Get`) |

The annotation table is built at startup by walking the proto descriptors from `gosdk`. No per-field hand-coding is
needed.

## Nebius IDs (NIDs)

Format (from `gosdk/nid`): `^<type>-<routing code><unique>[--<reserved>]$`

- `<type>`: `[a-z][a-z0-9]{2,49}`, the resource-kind prefix. Seen in the protos: `project`, `tenant`, `serviceaccount`,
  `computeinstance`, `computedisk`, `vpcnetwork`, `vpcsubnet`, `mk8scluster`, `storagebucket`, `mbsec`, `kmssymkey`, …
  Each service doc lists its prefixes.
- `<routing code>`: three characters, `[a-z][a-z0-9]{2}`, that identify the region or routing domain (for example `e00`).
  The mocker maps regions to routing codes through config.
- `<unique>`: random lowercase alphanumerics.

Modes:

| Mode | Example | Use |
|---|---|---|
| `random` (default) | `computedisk-e00k3j9x2m1q8w7v4z` | Realistic |
| `sequential` | `computedisk-e00mock000000000001` | Deterministic tests and golden files |
| `seeded` | random from a fixed seed | Reproducible but realistic-looking |

The `parent_id` data source in the Terraform provider calls `GetByNameParentTypes`. The mocker must return the
parent-type metadata the same way, so that kind prefixes stay consistent.

## Standard methods

| RPC | Request pattern | Mock semantics |
|---|---|---|
| `Get` | `{id}` | `NOT_FOUND` if missing **or** the caller can't see it (the same as real IAM; existence is not leaked) |
| `GetByName` | `{parent_id, name}` (`common.v1.GetByNameRequest` or a service-specific one) | Exact name match within the parent |
| `List` | `{parent_id, page_size, page_token, filter?}` | Stable order (created_at, id). `page_size` default 100, max 1000 (configurable per service). Opaque, signed `page_token`. A bad token → `INVALID_ARGUMENT` |
| `Create` | `{metadata, spec}` → `Operation` | Validate → reserve quota → insert in initial state → start lifecycle |
| `Update` | `{metadata(id,…), spec}` + `X-ResetMask` → `Operation` | **Full replace with reset mask**, below |
| `Delete` | `{id}` → `Operation` | Check dependents → state `DELETING` → remove → release quota |
| `ListOperationsByParent` | `{parent_id}` | Operations whose resource has that parent |

### Update and `X-ResetMask`

The rules come straight from the upstream README:

- A field changes only if the request has a **non-default** value for it, **or** its path is in `X-ResetMask`.
- Lists: indices `0..min(len)-1` are patched element-wise; extra elements are added or removed. A list key alone in the
  mask clears the list.
- Maps: incoming keys are updated, missing keys removed, new keys added.
- Nested: masking `a.b` when `a` is absent resets the whole of `a`; sending `a: {}` with `a.b` resets only `b`.

Implementation: `gosdk/proto/fieldmask/protobuf.PatchWithResetMask(stored, incoming, mask)`. This is the same library the
SDK uses on the client side to compute masks, so both sides agree on semantics by construction. After the patch we run
immutability checks by diffing the old and new spec against the `IMMUTABLE` paths.

### Concurrency

The upstream README says: "Nebius AI Cloud does not support concurrent operations on the same resource." The mocker keeps
one **active-operation lock per resource**. A second mutating call while an operation is running returns
`FAILED_PRECONDITION` + `OperationConflict{conflicting_operation_id}`. With `NEBIUS_MOCKER_OP_CONFLICT=abort-previous`,
the earlier operation completes with `ABORTED` + `OperationAborted{aborted_by_operation_id}` instead. Upstream documents
both behaviors.

## Operations

`nebius.common.v1.Operation` is returned by every mutating RPC:

- `id`: NID with an operation prefix that encodes the owning module, so any endpoint can resolve it.
- `resource_id`: target resource. Filled from the first response, so `op.ResourceID()` works immediately (the provider
  depends on this right after Create).
- `status`: **null while running**. When finished it is a `google.rpc.Status`; `OK`, or an error with `ServiceError`
  details. `finished_at` is set at the same time.
- `request`, `request_headers`: echo of the original request and allow-listed headers.
- `progress_tracker` / `progress_data`: optional, filled by long lifecycles (mk8s, instances) for realism.

`OperationService` RPCs: `Get`, `List` (by `resource_id`). There is also a `v1alpha1` variant for the alpha services
(`mk8s/v1alpha1`, `msp`). Retention: finished operations are kept for `NEBIUS_MOCKER_OP_RETENTION` (default 7 days of mock time).

The real API can return an operation that is **already complete** (synchronous). The mocker does this for cheap changes
(labels-only updates, IAM bindings) and when the `instant` timing profile is active.

## Errors

The wire format is `google.rpc.Status{code, message, details[]}`, where details holds `nebius.common.v1.ServiceError`:

| `ServiceError.details` | gRPC code | Typical trigger |
|---|---|---|
| `bad_request{violations[field,message]}` | `INVALID_ARGUMENT` | protovalidate failure, immutable change, bad reference format |
| `bad_resource_state{resource_id,message}` | `FAILED_PRECONDITION` | Start on a RUNNING instance, delete a network with subnets |
| `resource_not_found{resource_id}` | `NOT_FOUND` | Missing or invisible resource |
| `resource_already_exists{resource_id}` | `ALREADY_EXISTS` | Name collision |
| `out_of_range{requested,limit}` | `OUT_OF_RANGE` | Disk size > platform max |
| `permission_denied{resource_id}` | `PERMISSION_DENIED` | Authz denial |
| `resource_conflict{resource_id,message}` | `ABORTED` | `resource_version` mismatch |
| `operation_aborted{…}` | `ABORTED` | Superseded operation |
| `operation_conflict{…}` | `FAILED_PRECONDITION` | Parallel operation on the same resource |
| `too_many_requests{violation}` | `RESOURCE_EXHAUSTED` | Rate limit |
| `quota_failure{violations[quota,limit,requested,region]}` | `RESOURCE_EXHAUSTED` | Quota exceeded |
| `not_enough_resources{violations[resource_type,…]}` | `RESOURCE_EXHAUSTED` / `UNAVAILABLE` | Simulated capacity shortage (GPU platforms) |
| `internal_error{request_id,trace_id}` | `INTERNAL` | Only from fault injection or real bugs |

Every `ServiceError` also carries `service` (for example `compute`), a stable `code` string
(for example `compute.disk.size_out_of_range`), and `retry_type` (`CALL`, `NOTHING`, `UNIT_OF_WORK`). `retry_type` drives
gosdk's retry decisions, so the mocker sets it correctly to exercise client retry logic.

## Preflight checks

Some services expose `PreflightCheck(context, metadata, spec) → diagnostics` (in this revision, `mk8s.v1.NodeGroupService`).
The provider calls it during `terraform plan`, and treats `UNIMPLEMENTED` as "no diagnostics"
(`terraform-provider-nebius/service/preflight.go`). The mocker implements it for node groups: it runs the full Create/Update
validation path in dry-run mode and returns errors as `PreflightCheckDiagnostic{severity=ERROR}` rather than failing the RPC.
For a changed `IMMUTABLE` path it returns `paths_require_recreate`, so Terraform plans a replacement.
