# Object Storage

> Module: `services/storage` · Image: `nebius-mocker-storage` · `NEBIUS_MOCKER_SERVICES=storage`
> M1 tier: **T2** bucket, **T1** inventory/transfer · M3: **T3** S3 data plane · Endpoints: `cpl.storage`, `transfer.storage`

## Overview

S3-compatible object storage. The gRPC API manages the **control plane**: buckets (versioning, lifecycle rules, CORS,
policies, storage classes, size limits, filesystem-bucket mode), **inventories** (scheduled object listings) and
**transfers** (copying from external S3-compatible sources into a bucket). Object reads and writes go through the
**S3 HTTP API**, which is not part of `nebius/api`.

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Bucket | `Bucket` | `storagebucket` | project | `CREATING, ACTIVE, UPDATING, SCHEDULED_FOR_DELETION` |
| Inventory | `Inventory` | `storagebucketinventory` | bucket | |
| Transfer | `Transfer` (v1, v1alpha1) | `storagetransfer` | project | running/stopped; iteration history |

## Emulation design

- **Bucket names** are globally unique (S3 DNS semantics) across all tenants in the mocker instance. The S3 naming rules
  (3–63 chars, lowercase, no `..`) are enforced in addition to `buf.validate`.
- `status.domain_name` = `<name>.storage.<region>.mocker.localhost:4566`, and `status.region` comes from the project.
  `status.counters` are zeroed in `mock` mode and live with the S3 data plane.
- **Soft delete**: `Delete` → `SCHEDULED_FOR_DELETION` with `deleted_at` / `purge_at` (default grace: 7 days of mock clock).
  `Undelete` restores the bucket. `Purge` deletes it immediately. The name stays reserved until purge. Deleting a
  non-empty bucket (data plane on) → `FAILED_PRECONDITION`, unless forced.
- `lifecycle_configuration`, `cors`, `bucket_policy` and `versioning_policy` are validated structurally and stored. With
  the data plane on (M3), versioning and lifecycle expiry are actually applied.
- **Transfers** (T1): stored, with `Stop`/`Resume` state transitions. `GetIterationHistory` returns synthetic
  iterations generated on the mock clock. No data is copied in M1.
- **S3 data plane (M3)**: an embedded S3 server on the edge port, using virtual-host style or path style
  (`/s3/<bucket>/…`), with SigV4 auth against `iam.v2.AccessKey` credentials. Nebius's documented S3 endpoint is
  `https://storage.<region>.nebius.cloud`. Clients point their endpoint at the mocker. This also enables the provider's
  "store Terraform state in Object Storage" guide.
- Permissions: `storage.buckets.*` (+ `purge`, `undelete`), `storage.inventories.*`, `storage.transfers.*` (+ `stop`, `resume`).
  Quotas: `storage.bucket.count` (tenant), `storage.size` (tenant, data plane only).

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_storage_v1_bucket` | `nebius.storage.v1.BucketService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_storage_v1_inventory` | `nebius.storage.v1.InventoryService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 |
| Resource | `nebius_storage_v1_transfer` | `nebius.storage.v1.TransferService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 |
| Resource | `nebius_storage_v1alpha1_transfer` | `nebius.storage.v1alpha1.TransferService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 |
| Data source | `nebius_storage_v1_bucket` | `nebius.storage.v1.BucketService` | Get, GetByName | T2 |
| Data source | `nebius_storage_v1_inventory` | `nebius.storage.v1.InventoryService` | Get, GetByName | T1 |
| Data source | `nebius_storage_v1_transfer` | `nebius.storage.v1.TransferService` | Get, GetByName | T1 |
| Data source | `nebius_storage_v1alpha1_transfer` | `nebius.storage.v1alpha1.TransferService` | Get, GetByName | T1 |

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_STORAGE_PURGE_GRACE` | `168h` | Soft-delete grace |
| `NEBIUS_MOCKER_STORAGE_S3` | `0` | Enable the S3 data plane (M3) |
| `NEBIUS_MOCKER_STORAGE_S3_BACKEND` | `embedded` | `embedded` \| `minio:<url>` |

## Open questions
- Whether bucket names are unique per region or globally.
- Default `versioning_policy` / `default_storage_class` values (these matter for empty-plan fidelity).
- The `status.domain_name` format.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.storage.v1.BucketService`

Endpoint: `cpl.storage.api.nebius.cloud:443` · Proto: [`nebius/storage/v1/bucket_service.proto`](https://github.com/nebius/api/blob/main/nebius/storage/v1/bucket_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetBucketRequest` | `Bucket` | ✅ |
| `GetByName` | `GetBucketByNameRequest` | `Bucket` | ✅ |
| `List` | `ListBucketsRequest` | `ListBucketsResponse` |  |
| `ListWithFilter` | `ListBucketsWithFilterRequest` | `ListBucketsWithFilterResponse` |  |
| `Create` | `CreateBucketRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateBucketRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteBucketRequest` | `common.v1.Operation` | ✅ |
| `Purge` | `PurgeBucketRequest` | `common.v1.Operation` |  |
| `Undelete` | `UndeleteBucketRequest` | `common.v1.Operation` |  |

#### `nebius.storage.v1.InventoryService`

Endpoint: `cpl.storage.api.nebius.cloud:443` · Proto: [`nebius/storage/v1/inventory_service.proto`](https://github.com/nebius/api/blob/main/nebius/storage/v1/inventory_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetInventoryRequest` | `Inventory` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Inventory` | ✅ |
| `List` | `ListInventoriesRequest` | `ListInventoriesResponse` |  |
| `Create` | `CreateInventoryRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateInventoryRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteInventoryRequest` | `common.v1.Operation` | ✅ |

#### `nebius.storage.v1.TransferService`

Endpoint: `transfer.storage.api.nebius.cloud:443` · Proto: [`nebius/storage/v1/transfer_service.proto`](https://github.com/nebius/api/blob/main/nebius/storage/v1/transfer_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetTransferRequest` | `Transfer` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Transfer` | ✅ |
| `List` | `ListTransfersRequest` | `ListTransfersResponse` |  |
| `Create` | `CreateTransferRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateTransferRequest` | `common.v1.Operation` | ✅ |
| `Stop` | `StopTransferRequest` | `common.v1.Operation` |  |
| `Resume` | `ResumeTransferRequest` | `common.v1.Operation` |  |
| `Delete` | `DeleteTransferRequest` | `common.v1.Operation` | ✅ |
| `GetIterationHistory` | `GetIterationHistoryRequest` | `GetIterationHistoryResponse` |  |

#### `nebius.storage.v1alpha1.TransferService` *(deprecated)*

Endpoint: `transfer.storage.api.nebius.cloud:443` · Proto: [`nebius/storage/v1alpha1/transfer_service.proto`](https://github.com/nebius/api/blob/main/nebius/storage/v1alpha1/transfer_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetTransferRequest` | `Transfer` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Transfer` | ✅ |
| `List` | `ListTransfersRequest` | `ListTransfersResponse` |  |
| `Create` | `CreateTransferRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateTransferRequest` | `common.v1.Operation` | ✅ |
| `Stop` | `StopTransferRequest` | `common.v1.Operation` |  |
| `Resume` | `ResumeTransferRequest` | `common.v1.Operation` |  |
| `Delete` | `DeleteTransferRequest` | `common.v1.Operation` | ✅ |
| `GetIterationHistory` | `GetIterationHistoryRequest` | `GetIterationHistoryResponse` |  |
