# Capacity

> Module: `services/capacity` · Image: `nebius-mocker-capacity` · `NEBIUS_MOCKER_SERVICES=capacity`
> M1 tier: **T1** · M2: **T2** with capacity simulation · Endpoints: `capacity-blocks.billing-cpl`, `capacity-advisor.billing-cpl`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.capacity.v1.CapacityAllowanceService`: Create, Get, GetByParentAndCapacityBlockGroup (default allowance when none exists), List (one entry per block group of the tenant, paged), ListByCapacityBlockGroup (paged), Update (reset mask, `resource_version`), Delete | ✅ T2 |
| `nebius.capacity.v1.CapacityBlockGroupService`: Get, GetByResourceAffinity, List (paged), ListResources | ✅ T2 |
| `nebius.capacity.v1.CapacityIntervalService`: Get, List (paged) | ✅ T2 |
| `nebius.capacity.v1.ResourceAdviceService`: List (paged): reserved availability from the block groups | ✅ T1 (on-demand and preemptible: `UNKNOWN`, see below) |
| Seeded catalog of block groups and intervals (`NEBIUS_MOCKER_CAPACITY_BLOCK_GROUPS_FILE`), with limits, states and continuous intervals computed from the mock clock | ✅ |
| Reservations capability `peer.CapacityReservations` (`pkg/peer/capacity.go`): draws GPUs within the block group's limit and the project's allowance, and feeds `status.usage` and `ListResources` | ✅ consumed by compute instances (`spec.reservation_policy`, see [compute](compute.md#instances-implemented)) |
| Authorization: rules for every RPC (`capacity.allowances.*`, `capacity.blockGroups.*`, `capacity.intervals.*`, `capacity.resourceAdvice.list`); allowances checked on the project, block groups and advice on the tenant | ✅ |
| `nebius_capacity_v1_capacity_allowance` resource and data source, `nebius_capacity_v1_capacity_block_group` data source with the official provider | ✅ e2e: `test/terraform/capacity` |
| `nebius capacity capacity-block-group` / `capacity-interval` / `capacity-allowance` / `resource-advice` CLI flows from the docs | ✅ e2e: `test/cli/capacity` |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/capacity/{domain,dal,api}`), gosdk contract tests (`test/contract/capacity_*`),
the Terraform e2e suite and the CLI e2e suite (`make e2e`).

### Behavior notes

These follow [project limits](https://docs.nebius.com/overview/limits/project-limits),
[managing project limits](https://docs.nebius.com/overview/limits/manage-project-limits),
[capacity block groups](https://docs.nebius.com/overview/limits/capacity-block-groups),
[VM reservations](https://docs.nebius.com/compute/virtual-machines/reservations) and the
[capacity advisor](https://docs.nebius.com/compute/virtual-machines/capacity-advisor). Where they say nothing, the
mock's choice is marked *(mock)*.

- **Block groups are seeded.** In Nebius a manager creates them, so users can only read them. The built-in catalog
  (`services/capacity/domain/catalog.yaml`) has three groups in the seeded tenant:
  - `eu-north1` / `gpu-h100-sxm` / `fabric-2`: 16 GPUs now, plus 8 more from day +14 to +44;
  - `eu-north1` / `gpu-h200-sxm` / `fabric-7`: 32 GPUs;
  - `us-central1` / `gpu-b200-sxm` / `us-central1-b`: 64 GPUs from day +30, so `STATE_INACTIVE` now.

  IDs are derived from tenant + key, so they are stable across restarts. Groups in regions the mocker does not emulate
  are skipped.
- **Intervals** run from 00:00 UTC of their first day to 23:59 UTC of their last day (docs). Catalog days are relative
  to the UTC day the mocker starts, or absolute RFC 3339 times. Everything time-dependent is computed from the mock
  clock on every read:
  - `status.current_limit` is the sum of the active intervals;
  - `state` is `ACTIVE` with a non-zero limit, else `INACTIVE` *(mock: the transient `ALLOCATING` and `SHUTTING` are
    not modeled)*;
  - `next_change_at` / `next_change_to` come from the next interval boundary that changes the limit;
  - `current_continuous_interval` merges intervals that overlap or follow each other within a minute (23:59 → 00:00).
- **GetByResourceAffinity** matches tenant, region and `compute_v1.platform`, and `compute_v1.fabric` when set. If
  several groups match without a fabric, the call is `INVALID_ARGUMENT` and the message lists the fabrics *(mock)*.
  No match is `NOT_FOUND`.
- **Allowances (project limits).**
  - They are unnamed and unique per (project, block group): a second one is `ALREADY_EXISTS`.
  - The block group must belong to the project's tenant, else `NOT_FOUND`.
  - The limit is optional. Without one, the project may use the whole group. Docs: a limit above the group's size is
    allowed, and the effective limit is `min(limit, current_limit)`, so a higher limit takes effect when the group
    grows. Limits of different projects are independent and are not summed. `0` disables use of the group.
  - Delete removes the limit, so the effective limit becomes the group's size again.
  - Update changes the limit, labels or name, and may move the allowance to another group of the tenant: the provider
    updates `capacity_block_group_id` in place.
  - A `resource_version` mismatch is `FAILED_PRECONDITION`.
- **Default allowances.** `GetByParentAndCapacityBlockGroup` and `List` return a "non-created" allowance for groups
  without one, as the protos say: no ID, no limit, `STATE_ACTIVE` *(mock: the shape of a non-created allowance is not
  documented)*. `ListByCapacityBlockGroup` lists only created allowances.
- **Usage.**
  - `status.usage`, `usage_percentage` (e.g. `"37.5%"`) and `usage_state` of block groups and allowances come from the
    reservations held through `peer.CapacityReservations`. `ListResources` lists their consumers.
  - `unit` is `"GPU"`, `service` is `"compute"`.
- **Resource advice** returns one item per (block group, VM preset of its platform; presets in the catalog):
  - `reserved` counts whole VMs: `available` is free GPUs ÷ preset GPUs and `limit` is `current_limit` ÷ preset GPUs,
    with `FRESH` data. The level is `HIGH`, or `LIMIT_REACHED` when not one VM fits.
  - `on_demand` and `preemptible` report `DATA_STATE_UNKNOWN` / `AVAILABILITY_LEVEL_UNKNOWN` ("failed to retrieve
    data"), because the mocker simulates no physical capacity.

### Not yet

- mk8s node groups: a `template.reservation_policy` other than the default (`AUTO`, no IDs) is still rejected by name
  in mk8s. GPU nodes are compute instances under the default `AUTO`, so they draw from a matching block group like any
  other instance.
- Reservations are not re-checked when a block group's interval ends: instances keep the GPUs they hold.
- On-demand and preemptible availability in resource advice: needs a physical-capacity model.
- `ListByCapacityBlockGroup` does not list default allowances of projects that have none: the mocker has no
  tenant-to-projects lookup.
- `STATE_CONTAINER_DELETED` allowances: a project with an allowance cannot be deleted (it is not empty), so the state
  never occurs.
- An admin API to add block groups at runtime: change the catalog file and restart.

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

- Block groups and intervals are **seeded** from a catalog file, because users cannot create them in the real cloud.
  The domain keeps them in memory (no DAL). `GetByResourceAffinity` matches on (tenant, region, platform, fabric).
- An allowance is unique per (parent, block group), which is why the provider uses `GetByParentAndCapacityBlockGroup` for
  lookup. Its limit is not bounded at write time: the effective limit is `min(limit, current_limit)`, and it is enforced
  where capacity is consumed (`peer.CapacityReservations.Reserve`), as the project-limit docs describe.
- Compute instances draw GPUs through `peer.CapacityReservations` on create and start and release them on delete
  (held while stopped; see compute.md): `Reserve`
  checks the block group's current limit and the project's effective limit, and an exhausted one is
  `RESOURCE_EXHAUSTED`. Reservations are stored per consumer and drive `status.usage` and `ListResources`.
- `ResourceAdviceService` (T0 → M4) returns advice computed from the seeded pools.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_CAPACITY_BLOCK_GROUPS_FILE` | built-in catalog | YAML or JSON file that replaces the catalog of block groups, intervals and GPU presets (same shape as `services/capacity/domain/catalog.yaml`) |

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
