# 09 — Quotas, limits and fault injection

## Quotas

Nebius exposes quotas as resources: `nebius.quotas.v1.QuotaAllowance` (`spec.limit`, `spec.region`,
`status.usage`, `status.usage_state`, …) under a tenant or project, served by `QuotaAllowanceService` at
`quota-dispatcher.billing-cpl`. The Terraform provider manages them through `nebius_quotas_v1_quota_allowance`.

### Usage and the gate

The quota names, units, owning areas and default limits are the catalog in `pkg/quota` (names as Nebius uses them, for
example `compute.disk.size.network-ssd`; defaults overridable with `NEBIUS_MOCKER_QUOTAS_LIMITS`). The contract between
areas is `pkg/peer/quota.go`:

- **Usage is counted, not stored.** Each area that charges quotas provides a `peer.QuotaUsageSource` under
  `peer.QuotaUsageKey(area)`, which counts its own store (`[]QuotaUsage{ContainerID, Region, Name, Amount}`). There is
  no ledger to drift: usage is right after a restart and for resources that predate an allowance. A project's usage also
  counts toward its tenant.
- **Creates pass through the gate.** An area wraps the store write of a create or resize in
  `peer.QuotaGate.Admit(ctx, QuotaClaim{ContainerID, Region, Amounts}, apply)`, usually inside its operation's `Prepare`.
  Under one process-wide lock the quotas module computes usage, checks each positive delta against the project's and
  the tenant's allowance (the value set on the `QuotaAllowance`, else the catalog default) and only then runs `apply`.
  Two concurrent creates therefore cannot both fit under one limit. A resource counts until its record is removed, so
  deletes release their share without bookkeeping.
- Exceeding an allowance → `RESOURCE_EXHAUSTED` with `QuotaFailure{violations[{quota, limit, requested, region,
  message}]}` (`errs.QuotaExceeded`, mapped in `pkg/serviceerror`).
- `QuotaAllowance.status.usage` and `usage_percentage` are computed from the same sources on every read, so Terraform
  data sources and the CLI see real numbers.
- With the quotas module disabled, `peer.AdmitQuota` runs `apply` directly and nothing is enforced.

Per-area details (which kinds charge which quotas) are in [services/quotas.md](../services/quotas.md#catalog).

## Static limits and validation

Separate from quotas, modules enforce documented API limits. `buf.validate` covers most of them; modules add the domain
rules, for example:

- Disk `size_*` must be a multiple of the block size, and not more than the type's maximum.
- A platform/preset pair must exist in `PlatformService` seed data.
- Subnet CIDRs must lie within the network's pools and must not overlap.
- mk8s `version` must be one of the supported versions in the seed catalog.
- Name formats and label key/value lengths.

These live in code next to the module, each with a unit test (see [12](12-test-harness.md)).

## Capacity simulation

GPU capacity is a real constraint in Nebius (`capacity` APIs, `NotEnoughResources` errors). The mocker models
**per-region, per-platform capacity pools** (default: unlimited). When a pool is set with
`POST /_mocker/capacity {"region":"eu-north1","platform":"gpu-h100-sxm","available":16}`:

- instance Create or Start beyond capacity fails the **operation** (not the RPC) with `NotEnoughResources`, as the real
  cloud does after scheduling;
- `capacity.v1` `CapacityBlockGroup`/`CapacityInterval` data reflect the pool, and `CapacityAllowance` reserves out of it.

## Rate limiting

Token buckets per (principal, service), off by default. `NEBIUS_MOCKER_RATE_LIMIT="compute=50/s,*=200/s"`.
Throttling returns `RESOURCE_EXHAUSTED` + `TooManyRequests{violation}` with `retry_type=CALL`, which exercises the SDK's
retry and backoff.

## Fault injection

LocalStack calls this its Chaos API. Rules are managed via `POST/GET/DELETE /_mocker/faults` or a YAML file:

```yaml
- match:
    method: "nebius.compute.v1.InstanceService/Create"   # glob allowed: "nebius.compute.*"
    user_agent: "terraform-provider-nebius/*"            # optional
    labels: { chaos: "true" }                            # optional: match request metadata.labels
  probability: 1.0                                       # 0..1
  times: 1                                               # then auto-removed
  effect:
    kind: rpc_error            # rpc_error | operation_error | latency | drop_operation | stuck_operation
    code: UNAVAILABLE
    service_error: { internal_error: {} }
    retry_type: CALL
```

| Effect | Behavior |
|---|---|
| `rpc_error` | Fail the RPC itself, before any state change |
| `operation_error` | Accept the RPC, then fail the operation at step *n*. Tests Terraform's handling of failed applies and tainted resources |
| `latency` | Add a delay before the handler runs, to test client timeouts (`timeout`, `per_retry_timeout`) |
| `stuck_operation` | The operation never completes, to test `terraform apply` timeouts |
| `drop_operation` | Return an operation ID that `OperationService.Get` later reports as `NOT_FOUND` (retention edge case) |
