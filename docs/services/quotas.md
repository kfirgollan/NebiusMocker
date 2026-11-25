# Quotas

> Module: `services/quotas` · Image: `nebius-mocker-quotas` · `NEBIUS_MOCKER_SERVICES=quotas`
> M1 tier: **T1** with live usage · Endpoint: `quota-dispatcher.billing-cpl.api.nebius.cloud`

## Overview

`QuotaAllowance` resources (`UNNAMED`, parent a tenant or project) expose the limit (`spec.limit`, `spec.region`) and
the live usage (`status.usage`, `usage_percentage`, `usage_state`, `unit`, `service`, `description`).
This area is the public view of the quota ledger described in
[architecture/09](../architecture/09-quotas-limits-faults.md#quotas).

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Quota allowance | `QuotaAllowance` | `quotaallowance` | tenant / project | `STATE_PROVISIONING, STATE_ACTIVE, STATE_FROZEN, STATE_DELETED` |

## Emulation design

- Every catalog quota appears as an allowance in `List`, even if no user resource exists, because that is how the real
  service presents defaults. `GetByName(parent, name)` looks up by quota name.
- `Create` / `Update` set an explicit limit. In the real cloud this is a "request quota increase" flow. In the mocker it
  applies immediately, unless `NEBIUS_MOCKER_QUOTAS_APPROVAL=manual`, in which case the allowance stays in `STATE_PROVISIONING`
  until it is approved via the admin API.
- Setting a limit below current usage is allowed (as in real clouds). New consumption then fails with `QuotaFailure`.
- `status` is computed from the ledger on read, so it is always consistent.
- Lowering a limit is not `Delete`. `Delete` reverts to the catalog default.
- Permissions: `quotas.quotaAllowances.*`.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_quotas_v1_quota_allowance` | `nebius.quotas.v1.QuotaAllowanceService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 |
| Data source | `nebius_quotas_v1_quota_allowance` | `nebius.quotas.v1.QuotaAllowanceService` | Get, GetByName | T1 |

## Open questions
- Real quota names and units for each service.
- Whether `Create` of an allowance that already exists is `ALREADY_EXISTS` or an upsert.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.quotas.v1.QuotaAllowanceService`

Endpoint: `quota-dispatcher.billing-cpl.api.nebius.cloud:443` · Proto: [`nebius/quotas/v1/quota_allowance_service.proto`](https://github.com/nebius/api/blob/main/nebius/quotas/v1/quota_allowance_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `List` | `ListQuotaAllowancesRequest` | `ListQuotaAllowancesResponse` |  |
| `Get` | `GetQuotaAllowanceRequest` | `QuotaAllowance` | ✅ |
| `GetByName` | `GetByNameRequest` | `QuotaAllowance` | ✅ |
| `Create` | `CreateQuotaAllowanceRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateQuotaAllowanceRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteQuotaAllowanceRequest` | `common.v1.Operation` | ✅ |
