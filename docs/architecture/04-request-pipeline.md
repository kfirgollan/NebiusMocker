# 04 — Request pipeline

## Transport

| Aspect | Real Nebius | NebiusMocker |
|---|---|---|
| Protocol | gRPC over HTTP/2 + TLS, port 443 | gRPC over **h2c (plaintext) by default**, optional TLS |
| Addressing | One host per service: `{service}.api.nebius.cloud:443` (see [services/README](../services/README.md#endpoint-map)) | **One edge port (`4566`) for everything.** Optional per-service ports, and optional host-based TLS virtual hosts for clients that cannot override resolution |
| HTTP endpoints | `https://auth.eu.nebius.com/oauth2/token/exchange` (RFC 8693) | Same path, served on the edge port via an HTTP/1.1 + h2c mux |

### How clients reach the mock

The Terraform provider and `gosdk` resolve a *service ID* (for example `nebius.compute.v1.DiskService`) to an address
through a chain of resolvers (`gosdk/conn/resolvers.go`). Three provider knobs are enough:

```hcl
provider "nebius" {
  # 1. Send every service to the mocker. "*" is a prefix wildcard (PrefixResolver with an empty prefix).
  resolvers = { "*" = "localhost:4566" }

  # 2. Plaintext gRPC for every address. Or use no_tls_verify with the mocker's self-signed TLS.
  address_options = { "*" = { insecure = true } }

  # 3. Any token the mocker issued, or a static dev token (see 06).
  token = "nmock-dev-admin"

  parent_id = "project-e00mock0000000000default"
}
```

Equivalent environment-only setup, for CI scripts that should not edit `.tf` files:

```bash
export NEBIUS_IAM_TOKEN=nmock-dev-admin
# resolvers_env expects ID=ADDRESS[;insecure], comma-separated (gosdk/conn/parse.go)
export NEBIUS_MOCKER_RESOLVERS='*=localhost:4566;insecure'
# provider { resolvers_env = "NEBIUS_MOCKER_RESOLVERS" }   ← one line, or injected by tfnebiuslocal
```

Alternatives we also support, for tools that only accept a domain:

- **`domain = "mocker.localhost:4566"`**: the convention resolver yields `compute.mocker.localhost:4566`,
  `cpl.iam.mocker.localhost:4566`, and so on. Because `*.localhost` resolves to loopback, every host reaches the edge port.
  The edge ignores `:authority` for routing, since gRPC method names are globally unique.
- **TLS with a generated CA**: the mocker can mint a local CA and a wildcard certificate for `*.api.nebius.cloud`. A client
  that trusts the CA and points DNS at the mocker (for example via docker `extra_hosts`) works with **zero** client
  configuration. This is opt-in (`NEBIUS_MOCKER_TLS=wildcard`) and documented as advanced.

## Routing

gRPC routes on the `:path` pseudo-header `/package.Service/Method`. Every enabled module registers its services on the
shared `grpc.Server`. Requests for services that are known but disabled get a mocker-registered stub returning
`UNIMPLEMENTED` with the message `service nebius.x.v1.FooService is not enabled in this NebiusMocker instance (NEBIUS_MOCKER_SERVICES)`.
That message is much easier to debug than gRPC's generic "unknown service".

`nebius.common.v1.OperationService` and `nebius.common.v1alpha1.OperationService` are registered **once**, by the operation
engine. They resolve an operation ID across all modules (see [08](08-resource-lifecycle.md)).

## Interceptor chain

Order matters. Each step can short-circuit with a gRPC status.

| # | Interceptor | Responsibility | Fails with |
|---|---|---|---|
| 1 | **request context** | Assign `X-Request-ID` and `X-Trace-ID` (returned in response headers and trailers, which `gosdk/serviceerror` reads); start a log span; capture `User-Agent` (the provider sends `terraform-provider-nebius/<ver> (terraform/<ver>)`) | — |
| 2 | **recorder** (optional) | Record request and response to a JSONL journal, for debugging and parity fixtures | — |
| 3 | **faults** | Match rules from [09](09-quotas-limits-faults.md) and inject latency or errors | any |
| 4 | **authn** | Parse `authorization: Bearer <token>`; resolve the token to a `Principal` (user account, service account, or anonymous). `TokenExchangeService` and health/reflection are exempt | `UNAUTHENTICATED` |
| 5 | **rate limit** | Token bucket per (principal, service), configurable | `RESOURCE_EXHAUSTED` + `TooManyRequests` |
| 6 | **authz** | Map method → permission (from module `Kinds`/explicit table), find the target resource's ancestry, evaluate | `PERMISSION_DENIED` + `PermissionDenied{resource_id}` |
| 7 | **validation** | `protovalidate` on the request using the `buf.validate` annotations already in the protos | `INVALID_ARGUMENT` + `BadRequest{violations}` |
| 8 | **idempotency** | For mutating RPCs that carry `X-Idempotency-Key`, return the stored response if (key, principal, method) was seen, and reject a different payload under the same key | `INVALID_ARGUMENT` on payload mismatch |
| 9 | **masks** | Parse `X-ResetMask` (Update) and `X-SelectMask` into the request context | `INVALID_ARGUMENT` |
| 10 | **handler** | Module code / resource framework | domain errors |
| 11 | **response** | Attach `X-Nebius-Warnings-Bin` trailer (serialized `nebius.common.v1.Warnings`) if a handler emitted warnings, such as deprecations | — |

## Headers

| Header | Direction | Semantics in the mocker |
|---|---|---|
| `authorization` | in | `Bearer <IAM token>`. See [06](06-iam-and-authorization.md). |
| `x-idempotency-key` | in | `[A-Za-z0-9-]+`, ignored on Get/List. Records are kept for 24h of mock clock by default. |
| `x-resetmask` | in | Nebius reset-mask syntax (`a, b.c, d.e.12, f.(j.h,i.j).k, l.*.m`). Parsed with `gosdk/proto/fieldmask/mask`. |
| `x-selectmask` | in | Field selection on read responses. The provider uses it in preflight diffing. We apply it with `fieldmaskprotobuf.FilterWithSelectMask`. |
| `user-agent` | in | Logged, and matchable in fault rules (for example, fail only Terraform). |
| `x-request-id`, `x-trace-id` | out | Always set. Included in `InternalError{request_id, trace_id}`. |
| `x-nebius-warnings-bin` | out (trailer) | Binary `nebius.common.v1.Warnings`. The provider shows these as Terraform warnings. |

## Streaming

None of the 488 RPCs in the pinned API revision is a streaming RPC, so the pipeline is unary-only for now.
We register stream interceptors anyway, as a defensive default.

## Errors

All errors are `google.rpc.Status` with zero or more `nebius.common.v1.ServiceError` entries in `details`
(see [05](05-api-conventions.md#errors)). The `pkg/serviceerror` helpers make it hard to return a bare status:

```go
return serviceerror.NotFound(id)                         // NOT_FOUND + ResourceNotFound{resource_id}
return serviceerror.AlreadyExists(existingID)            // ALREADY_EXISTS + ResourceAlreadyExists
return serviceerror.Quota(viol...)                       // RESOURCE_EXHAUSTED + QuotaFailure
return serviceerror.BadState(id, "instance is STOPPED")  // FAILED_PRECONDITION + BadResourceState
return serviceerror.OpConflict(runningOpID, id)          // ABORTED/FAILED_PRECONDITION + OperationConflict
```
