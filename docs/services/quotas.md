# Quotas

> Module: `services/quotas` · `NEBIUS_MOCKER_SERVICES=quotas`
> Tier: **T2** with live usage and enforcement · Endpoint: `quota-dispatcher.billing-cpl.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.quotas.v1.QuotaAllowanceService`: List (paged), Get, GetByName, Create (upsert), Update (reset mask, `resource_version`, `spec.region` immutable), Delete (resets to the default) | ✅ T2 |
| Live `status`: `usage`, `usage_percentage`, `usage_state`, `unit`, `service`, `service_description`, `description`, `state` | ✅ |
| Enforcement: creates and resizes that would exceed a project's or its tenant's allowance fail with `RESOURCE_EXHAUSTED` + `QuotaFailure` | ✅ (areas below) |
| Only admins change quotas (editors and viewers read them) | ✅ |
| Default limits per quota, overridable with `NEBIUS_MOCKER_QUOTAS_LIMITS` | ✅ |
| `nebius_quotas_v1_quota_allowance` resource and data source (by ID and `by_name_in_region`) with the official Terraform provider | ✅ e2e: `test/terraform/quotas` |
| CLI: `nebius quotas quota-allowance list / get-by-name / get / create / update / delete` | ✅ e2e: `test/cli/quotas` |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Tests: catalog and config unit tests (`pkg/quota`, `pkg/config`), domain tests with fakes including a 50-goroutine race
on one limit (`services/quotas/domain`), DAL tests on the memory store, gosdk contract tests for every RPC, authz per
role, enforcement and release, parallel creates and one create per wired area (`test/contract/quotas_*`), Terraform
CRUD plus lowered allowances that refuse a network and a Managed PostgreSQL cluster (`make e2e`), and the CLI flows
from the docs (`make e2e`).

## Overview

A `QuotaAllowance` (`UNNAMED`; `metadata.name` is the quota name, parent a tenant or project) holds a limit
(`spec.limit`) for one region (`spec.region`). Every tenant and project has one allowance per catalog quota: a project
in its own region, a tenant in every emulated region (`NEBIUS_MOCKER_REGIONS`).

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Quota allowance | `QuotaAllowance` | `quotaallowance` | tenant / project | `STATE_ACTIVE` (the others are never reported) |

## Emulation design

- **Usage is counted, never stored.** Each area provides a usage source (`peer.QuotaUsageKey(area)`) that counts its own
  store. `status.usage` sums them on every read, so it is right after a restart and for resources created before an
  allowance was lowered. A project's resources also count toward its tenant.
- **The gate.** Areas wrap the store write of a create or resize in `peer.QuotaGate.Admit` (`pkg/peer/quota.go`). Under
  one process-wide lock it computes usage, checks every positive delta against the project's allowance and the
  tenant's, then runs the write. Two parallel creates cannot both slip under a limit. A resource counts until its record
  is removed, so deletes release their share with no extra bookkeeping. Without the quotas module (`NEBIUS_MOCKER_SERVICES`
  without `quotas`) nothing is enforced.
- **The error.** `RESOURCE_EXHAUSTED` with a `nebius.common.v1.ServiceError` whose `quota_failure` has one violation per
  exhausted allowance: `quota` (the name), `limit`, `requested` (the delta), `region` and a message such as
  `quota vpc.network.count of project-… in eu-north1 exceeded: limit 3 count, used 3, requested 1`. gosdk decodes it as
  `*serviceerror.QuotaFailure`. It is returned by the create RPC itself (the check runs in `Prepare`), except where an
  area writes the record in the operation's work step (Object Storage transfers, mk8s nodes), where the operation fails.
  The Nebius docs don't show the exact error text, so the message is ours; the code and detail type are Nebius's.
- **Create is an upsert**, as the proto says: it returns the existing allowance (same ID) with its limit replaced. A
  project allowance may omit `spec.region` (the project's region; the empty value is echoed back so Terraform plans
  stay empty); a tenant allowance must name one. Unknown quota names and regions are `INVALID_ARGUMENT`.
- **Delete resets to the default**, and the allowance remains ("it does not remove the quota entry"). `spec.limit` on
  read is always the effective limit.
- **Changes apply at once.** In Nebius, a change is a request that support reviews. Setting a limit below current usage
  is allowed; existing resources stay and new consumption fails.
- **Permissions:** `quotas.quotaAllowances.{get,list}` for viewers and editors; `create`, `update` and `delete` only
  for admins ("Only users in groups with the `admin` role can change quotas").
- `usage_percentage` is usage/limit with two decimals (`"0.12"`, above `1.00` when over the limit); a zero limit in use
  reads `"1.00"`. `usage_state` is `USED`/`NOT_USED`, or `UNKNOWN` when the owning area is not enabled.

## Catalog

Names are the identifiers Nebius uses in `metadata.name`. The Nebius docs list quotas by display name only (for example
"Regular VMs"), plus the example `compute.disk.size.network-ssd`. The other names come from Nebius's own tooling
(github.com/nebius/nebius-physical-ai), except those marked *inferred*, which follow the same pattern. The Managed
PostgreSQL and MLflow docs list four quotas each by display name only ("Clusters", "Total vCPUs across clusters", "Total
RAM across clusters", "Network SSD disks total size across clusters"; defaults 10, 80, 320 GiB, 2 TiB); their names are
inferred from the API packages (`nebius.msp.postgresql`, `nebius.msp.mlflow`). Defaults are far
above the documented default tier (2 VMs, 8 GPUs, …), so local work is unlimited until you lower an allowance:
counts 1000, `compute.instance.non-gpu.vcpu` 100000, GPUs 4096, sizes 1 PiB. Override them with
`NEBIUS_MOCKER_QUOTAS_LIMITS="compute.instance.count=2,compute.disk.size.network-ssd=4Ti"` (Ki…Pi suffixes).

| Area | Quota | Unit | Charged by |
|---|---|---|---|
| compute | `compute.instance.count` | count | regular instances, including mk8s nodes |
| compute | `compute.instance.preemptible.count` | count | preemptible instances |
| compute | `compute.instance.non-gpu.vcpu` | count | vCPUs of non-GPU instances |
| compute | `compute.instance.gpu.<family>` (`b200`, `b300`, `h100`, `h200`, `l40s`, `rtx6000`; `gb200`, `gb300` *inferred*; any other platform family too) | count | GPUs of regular GPU instances without a reservation (`gpu-h100-sxm` → `h100`) |
| compute | `compute.gpucluster.count` | count | GPU clusters |
| compute | `compute.disk.count`, `compute.disk.size.<type>` (`network-ssd`, `network-ssd-non-replicated`, `network-ssd-io-m3`, `network-hdd`) | count, byte | disks, including managed boot disks; resizes |
| compute | `compute.disk.snapshot.count`, `compute.disk.snapshot.size` | count, byte | disk snapshots |
| compute | `compute.filesystem.count`, `compute.filesystem.size.<type>` (`network-ssd`; `network-hdd`, `weka`, `vast` *inferred*) | count, byte | shared filesystems; resizes |
| vpc | `vpc.network.count`, `vpc.subnet.count`, `vpc.pool.count`, `vpc.routetable.count`, `vpc.route.count` | count | user-created resources (see [vpc.md](vpc.md) for defaults) |
| vpc | `vpc.allocation.count`, `vpc.ipv4-address.public.count` | count | allocations, including the ones NICs create; public IPv4 ones |
| vpc | `vpc.securitygroup.count`, `vpc.securityrule.count` (*inferred*) | count | security groups and rules |
| dns | `dns.zone.count` | count | zones |
| storage | `storage.bucket.count`, `storage.transfer.count` | count | buckets (all types), transfers (v1 and v1alpha1) |
| mk8s | `mk8s.cluster.count` | count | clusters; node groups are checked against the compute quotas of their nodes |
| registry | `registry.count` | count | registries |
| mysterybox | `mysterybox.secret.count` | count | secrets, including ones scheduled for deletion (as in Nebius) |
| kms | `kms.symmetrickey.count`, `kms.asymmetrickey.count` (*inferred*) | count | keys, including ones scheduled for deletion (as in Nebius) |
| msp-postgresql | `msp.postgresql.cluster.count`, `msp.postgresql.vcpu` (*inferred*) | count | clusters (Create and Restore), whatever their state, stopped included; hosts × preset vCPUs, with the growth of an Update |
| msp-postgresql | `msp.postgresql.memory`, `msp.postgresql.disk.size` (*inferred*) | byte | hosts × preset RAM; hosts × `disk.size_gibibytes` |
| msp-mlflow | `msp.mlflow.cluster.count`, `msp.mlflow.vcpu` (*inferred*) | count | clusters, stopped included; the vCPUs of their `size` |
| msp-mlflow | `msp.mlflow.memory` (*inferred*) | byte | the RAM of their `size` |

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_quotas_v1_quota_allowance` | `nebius.quotas.v1.QuotaAllowanceService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Data source | `nebius_quotas_v1_quota_allowance` | `nebius.quotas.v1.QuotaAllowanceService` | Get, GetByName | T2 |

`terraform destroy` resets the limit to the default. Changing `region` (or `parent_id`) replaces the resource, which,
since Create is an upsert, keeps the same allowance ID.

## Not yet

- Size quotas of Object Storage (`storage.bucket.size.standard`, `.enhanced-throughput`, `.intelligent`): enforcing them
  needs a check on every S3 upload. Images (`compute.image.count`, `compute.image.size`): the public API cannot create
  images. The MLflow disk quota ("Network SSD disks total size across clusters"): the API exposes no disk size for an
  MLflow cluster.
- Quotas the Nebius docs list for areas the mocker doesn't serve yet, or whose names aren't documented: IAM (projects,
  service accounts), AI endpoints, Soperator.
  These names aren't in the catalog, so they can't be set and silently ignored. (Tunnels have no quota allowances: their
  documented limits are traffic and connection rates, which "can't be changed".)
- The `STATE_PROVISIONING` review flow and automatic quota reduction (the mocker applies changes at once).
- `aitenant`, `tractotenant` and `aiproject` parents.

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
