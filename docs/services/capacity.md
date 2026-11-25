# Capacity

> Module: `services/capacity` · Image: `nebius-mocker-capacity` · `NEBIUS_MOCKER_SERVICES=capacity`
> M1 tier: **T1** · M2: **T2** with capacity simulation · Endpoints: `capacity-blocks.billing-cpl`, `capacity-advisor.billing-cpl`

## Overview

Reserved GPU capacity: **capacity block groups** (a reservation of a resource type in a region; read-only to users, with
`status.current_limit`, `usage`, `next_change_at`, intervals), **capacity intervals** (time windows of a block group),
**capacity allowances** (a user's share of a block group for a project, with `spec.limit`), and the **resource advice**
service (suggestions for where capacity exists).

| Resource kind | Message | NID prefix | Parent |
|---|---|---|---|
| Capacity block group | `CapacityBlockGroup` | `capacityblockgroup` | tenant (seeded) |
| Capacity interval | `CapacityInterval` | `capacityinterval` | block group (seeded) |
| Capacity allowance | `CapacityAllowance` | `capacityallowance` | project |

## Emulation design

- Block groups and intervals are **seeded** (admin API / seed file), because users cannot create them in the real cloud.
  `GetByResourceAffinity` matches on (region, platform, …) affinity.
- An allowance is unique per (parent, block group), which is why the provider uses `GetByParentAndCapacityBlockGroup` for
  lookup. Its limit is bounded by the group's `current_limit` minus other allowances.
- **M2**: block groups feed the capacity pools from [architecture/09](../architecture/09-quotas-limits-faults.md#capacity-simulation).
  Compute instances with `reservation_policy` draw from the matching allowance, `status.usage` is live, and an exhausted
  allowance → `NotEnoughResources`.
- `ResourceAdviceService` (T0 → M4) returns advice computed from the seeded pools.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_capacity_v1_capacity_allowance` | `nebius.capacity.v1.CapacityAllowanceService` | Get, GetByParentAndCapacityBlockGroup, Create, Update, Delete + OperationService.Get | T1 |
| Data source | `nebius_capacity_v1_capacity_allowance` | `nebius.capacity.v1.CapacityAllowanceService` | Get, GetByParentAndCapacityBlockGroup | T1 |
| Data source | `nebius_capacity_v1_capacity_block_group` | `nebius.capacity.v1.CapacityBlockGroupService` | Get, GetByResourceAffinity | T1 |

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.capacity.v1.CapacityAllowanceService`

Endpoint: `capacity-blocks.billing-cpl.api.nebius.cloud:443` · Proto: [`nebius/capacity/v1/capacity_allowance_service.proto`](https://github.com/nebius/api/blob/main/nebius/capacity/v1/capacity_allowance_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `List` | `ListCapacityAllowancesRequest` | `ListCapacityAllowancesResponse` |  |
| `ListByCapacityBlockGroup` | `ListCapacityAllowancesByCapacityBlockGroupRequest` | `ListCapacityAllowancesResponse` |  |
| `Get` | `GetCapacityAllowanceRequest` | `CapacityAllowance` | ✅ |
| `GetByParentAndCapacityBlockGroup` | `GetCapacityAllowanceByParentAndCapacityBlockGroupRequest` | `CapacityAllowance` | ✅ |
| `Create` | `CreateCapacityAllowanceRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateCapacityAllowanceRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteCapacityAllowanceRequest` | `common.v1.Operation` | ✅ |

#### `nebius.capacity.v1.CapacityBlockGroupService`

Endpoint: `capacity-blocks.billing-cpl.api.nebius.cloud:443` · Proto: [`nebius/capacity/v1/capacity_block_group_service.proto`](https://github.com/nebius/api/blob/main/nebius/capacity/v1/capacity_block_group_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetCapacityBlockGroupRequest` | `CapacityBlockGroup` | ✅ |
| `GetByResourceAffinity` | `GetCapacityBlockGroupByResourceAffinityRequest` | `CapacityBlockGroup` | ✅ |
| `List` | `ListCapacityBlockGroupsRequest` | `ListCapacityBlockGroupsResponse` |  |
| `ListResources` | `ListCapacityBlockGroupResourcesRequest` | `ListCapacityBlockGroupResourcesResponse` |  |

#### `nebius.capacity.v1.CapacityIntervalService`

Endpoint: `capacity-blocks.billing-cpl.api.nebius.cloud:443` · Proto: [`nebius/capacity/v1/capacity_interval_service.proto`](https://github.com/nebius/api/blob/main/nebius/capacity/v1/capacity_interval_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetCapacityIntervalRequest` | `CapacityInterval` |  |
| `List` | `ListCapacityIntervalsRequest` | `ListCapacityIntervalsResponse` |  |

#### `nebius.capacity.v1.ResourceAdviceService`

Endpoint: `capacity-advisor.billing-cpl.api.nebius.cloud:443` · Proto: [`nebius/capacity/v1/resource_advice_service.proto`](https://github.com/nebius/api/blob/main/nebius/capacity/v1/resource_advice_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `List` | `ListResourceAdviceRequest` | `ListResourceAdviceResponse` |  |
