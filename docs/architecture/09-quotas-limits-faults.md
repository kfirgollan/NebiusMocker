# 09 — Quotas, limits and fault injection

## Quotas

Nebius exposes quotas as resources: `nebius.quotas.v1.QuotaAllowance` (`spec.limit`, `spec.region`,
`status.usage`, `status.usage_state`, …) under a tenant or project, served by `QuotaAllowanceService` at
`quota-dispatcher.billing-cpl`. The Terraform provider manages them through `nebius_quotas_v1_quota_allowance`.

### Ledger

`pkg/quota` keeps **usage counters** keyed by `(scope_id, quota_name, region)` and **limits** coming from:

1. `QuotaAllowance` resources, if present for that scope and name;
2. otherwise the catalog default (`quotas/catalog.yaml`), for example:

   ```yaml
   - name: compute.instance.count        # count of instances
     unit: count
     default: { tenant: 100 }
   - name: compute.disk.size             # sum of disk sizes
     unit: bytes
     default: { tenant: 100Ti }
   - name: compute.gpu.h100              # GPUs by platform
     unit: count
     default: { tenant: 16 }
   - name: vpc.network.count
     unit: count
     default: { project: 10 }
   - name: mk8s.cluster.count
     unit: count
     default: { tenant: 10 }
   ```

The catalog uses **our own quota names**, chosen to mirror what `QuotaAllowance.status.service`/`description` show in the
real cloud. Parity tests reconcile the names over time.

### Accounting

- Modules bind quotas to kinds declaratively: `Binding{Name, Per: Tenant|Project, Amount: func(msg) int64}`.
  For example, an instance on `gpu-h100-sxm` with preset `8gpu-…` costs 8 of `compute.gpu.h100`.
- On Create/Update the framework computes the delta and **reserves** it in the same transaction as the write.
  Exceeding the limit → `RESOURCE_EXHAUSTED` with `QuotaFailure{violations[{quota, limit, requested, region}]}`.
- On Delete, the reservation is released when the resource reaches `Gone`.
- `QuotaAllowance.status.usage` and `usage_percentage` are computed live from the ledger. Terraform data sources and the
  CLI therefore see real numbers.

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
