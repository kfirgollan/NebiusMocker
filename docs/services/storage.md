# Object Storage

> Module: `services/storage` · Image: `nebius-mocker-storage` · `NEBIUS_MOCKER_SERVICES=storage`
> M1 tier: **T2** bucket, **T1** inventory/transfer · M3: **T3** S3 data plane · Endpoints: `cpl.storage`, `transfer.storage`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.storage.v1.BucketService`: Get, GetByName, List (paged), ListWithFilter, Create, Update (reset mask, `resource_version`), Delete (soft, `purge_at`/`ttl`), Purge, Undelete | ✅ T2 |
| Bucket semantics: global S3 names, naming rules, `IMMUTABLE_NAME`, `versioning_policy` default + transition rules, `status.state/domain_name/region/bucket_type`, soft delete with `deleted_at`/`purge_at`, background purge sweeper | ✅ |
| Backing buckets in RustFS (created, versioned, force-deleted with all versions on purge) | ✅ |
| S3 data plane: S3 front on `:4580`, SigV4 with Nebius access keys, IAM-authorized, relayed to RustFS | ✅ (`NEBIUS_MOCKER_OBJSTORE=rustfs`) |
| `nebius_storage_v1_bucket` resource + data source with the official provider | ✅ e2e: `test/terraform/storage` (`make e2e`) |
| Objects with the unmodified `hashicorp/aws` provider (`aws_s3_object` + data source) against the S3 front | ✅ e2e: `test/terraform/storage` (`make e2e-storage`) |
| Go SDK: buckets with `gosdk`, objects with the AWS SDK for Go v2 | ✅ e2e: `test/storage` (`make e2e-storage`) |
| `lifecycle_configuration`, `cors`, `bucket_policy`, `max_size_bytes`, storage classes, audit logging, last access | Stored and returned, **not applied** to the data plane |
| Filesystem buckets (`spec.filesystem_bucket`) | ❌ rejected with `INVALID_ARGUMENT` |
| `InventoryService`, `TransferService` (v1, v1alpha1), `status.counters` | Not yet (UNIMPLEMENTED) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100 (hashicorp/aws 6.70.0, RustFS 1.0.6).

Covered by domain unit tests with fakes (`services/storage/domain`), DAL tests (`services/storage/dal`), S3 front
SigV4 accept/reject tests (`services/storage/s3`, `pkg/sigv4`), gosdk contract tests on the in-memory object store
(`test/contract/storage_bucket_test.go`), and the Docker-gated suites above (`make e2e-storage` starts and removes RustFS).

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

NebiusMocker implements no object storage. Object bytes live in [RustFS](https://github.com/rustfs/rustfs)
(`rustfs/rustfs:1.0.6`, multi-arch), reached through the shared `pkg/objstore` port. NebiusMocker provides only the
Nebius-shaped wrapping: the gRPC control plane, authentication and authorization, and glue.

```text
terraform / gosdk ──gRPC :4566──▶ BucketService ──▶ dal (pkg/store: bucket metadata, global name index)
                                        └──────────▶ pkg/objstore (RustFS S3 client): ensure / versioning / force-delete

aws CLI / AWS SDK / hashicorp/aws ──S3 HTTP :4580──▶ services/storage/s3 (S3 front)
   1. SigV4 verify (pkg/sigv4) with the secret of the iam.v2 AccessKey (peer.AccessKeys capability provided by IAM)
   2. domain.S3Gate: bucket must be an ACTIVE Nebius bucket; IAM permit check for the key's service account
   3. relay path-style to RustFS, re-signed with the RustFS credentials (pkg/objstore/rustfs.Forwarder)
```

- **Nebius metadata is the source of truth.** A bucket exists for S3 only if it is an `ACTIVE` Nebius bucket; soft-deleted
  buckets answer `NoSuchBucket`. Bucket management over S3 (`CreateBucket`, `DeleteBucket`, `PutBucket*`) answers
  `NotImplemented`: use the Nebius API. `ListBuckets` and `GetBucketLocation` are answered from Nebius metadata (the
  region is `eu-north1`). Users never see RustFS credentials.
- **Bucket names** are globally unique across all projects in the mocker instance (S3 DNS semantics), checked with a
  global name index. S3 rules: 3–63 characters of `a-z 0-9 . -`, alphanumeric at both ends, no `..`/`.-`/`-.`, not an IP
  address, no `xn--` prefix or `-s3alias` suffix. The prefix `nebius-mocker-` is reserved for internal buckets (e.g.
  compute image blobs) that share the RustFS instance. The name is immutable (`IMMUTABLE_NAME`) and stays reserved until purge.
  Nebius bucket names map 1:1 to RustFS bucket names.
- `status.domain_name` = `<name>.<NEBIUS_MOCKER_STORAGE_S3_DOMAIN>` (default `<name>.storage.eu-north1.mocker.localhost`);
  requests to that host are served virtual-hosted style, everything else path style.
- **Lifecycle**: Create stores the record `CREATING`, creates the RustFS bucket (and versioning) in the operation's work
  step, then `ACTIVE`; a backend failure fails the operation and removes the record. Update sets `UPDATING` while the
  versioning change is applied. Operations are asynchronous with `NEBIUS_MOCKER_OP_DELAY`.
- **Versioning**: `versioning_policy` defaults to `DISABLED` (`NON_EMPTY_DEFAULT`); transitions disabled → enabled/suspended
  and enabled ↔ suspended are applied to RustFS; enabled/suspended → disabled is `INVALID_ARGUMENT`.
- **Soft delete**: `Delete` needs an empty bucket (`FAILED_PRECONDITION` otherwise) and moves it to `SCHEDULED_FOR_DELETION`
  with `deleted_at` and `purge_at` = now + `ttl`, or `purge_at`, or the default grace (7 days, `NEBIUS_MOCKER_STORAGE_PURGE_GRACE`).
  A zero `ttl` or a past `purge_at` purges at once. `Undelete` restores `ACTIVE`; `Purge` deletes immediately. A sweeper
  (every minute, mock clock) purges expired buckets. Purging force-deletes the RustFS bucket with all object versions.
- **Permissions**: `storage.buckets.{get,list,create,update,delete}` (`Purge` checks `delete`, `Undelete` checks `update`);
  S3 object reads `storage.objects.get`, listings `storage.objects.list`, writes (put, copy, multipart) `storage.objects.create`,
  deletes `storage.objects.delete`; bucket-level S3 reads `storage.buckets.get`. CopyObject also needs `get` on the source
  bucket. The seeded `viewer`/`editor`/`admin` roles therefore apply to objects. `NEBIUS_MOCKER_AUTHZ` applies to the
  S3 front too (audit logs denials, enforce rejects with `AccessDenied`).
- **SigV4** (`pkg/sigv4`): header and presigned-URL forms, ±15 min clock skew, presign expiry ≤ 7 days, and aws-chunked
  bodies (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD[-TRAILER]`, `STREAMING-UNSIGNED-PAYLOAD-TRAILER`; chunk signatures verified,
  trailing checksums dropped). Non-streaming payload hashes are relayed so RustFS re-checks the body. Any region in the
  credential scope is accepted. Anonymous access (bucket policies) is not supported.
- **Other modules** reach buckets through `pkg/peer`: storage provides `peer.BucketResolverKey` (bucket ID → object-store
  bucket name) for services that write into user buckets (audit exports) and read from them (compute image imports
  use the bucket name directly, since names map 1:1).
- **Docker-free mode** (`NEBIUS_MOCKER_OBJSTORE=memory`, the default): buckets work fully over gRPC; the S3 front still
  authenticates and authorizes, then answers `NotImplemented` for data operations.
- `lifecycle_configuration`, `cors`, `bucket_policy`, `max_size_bytes` and storage-class settings are validated by
  `buf.validate`, stored and returned (no Terraform drift) but not applied to RustFS yet.

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

Objects are managed the way Nebius documents for S3 tools: the official `hashicorp/aws` provider with the S3 endpoint,
region and an access key, here the mocker's S3 front:

```hcl
ephemeral "nebius_iam_v2_access_key_secret" "s3" {
  id = nebius_iam_v2_access_key.s3.id           # or the key's ID from another config
}

provider "aws" {
  region     = "eu-north1"
  access_key = ephemeral.nebius_iam_v2_access_key_secret.s3.aws_access_key_id
  secret_key = ephemeral.nebius_iam_v2_access_key_secret.s3.secret
  endpoints { s3 = "http://127.0.0.1:4580" }    # Nebius: https://storage.<region>.nebius.cloud
  s3_use_path_style           = true
  skip_credentials_validation = true
  skip_region_validation      = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
}

resource "aws_s3_object" "hello" {
  bucket  = nebius_storage_v1_bucket.data.name
  key     = "hello.txt"
  content = "hello"
}
```

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_OBJSTORE` | `memory` | Object store shared by modules: `memory` (no S3 data plane) or `rustfs` |
| `NEBIUS_MOCKER_OBJSTORE_ENDPOINT` | `http://127.0.0.1:9000` | RustFS S3 endpoint |
| `NEBIUS_MOCKER_OBJSTORE_ACCESS_KEY` / `_SECRET_KEY` | `rustfsadmin` / `rustfsadmin` | RustFS credentials (never exposed to users) |
| `NEBIUS_MOCKER_OBJSTORE_REGION` | `us-east-1` | Region the mocker signs RustFS requests for |
| `NEBIUS_MOCKER_STORAGE_S3_LISTEN` | `:4580` | S3 front address; `off` disables it |
| `NEBIUS_MOCKER_STORAGE_S3_DOMAIN` | `storage.eu-north1.mocker.localhost` | S3 host; `status.domain_name` = `<bucket>.<domain>` |
| `NEBIUS_MOCKER_STORAGE_REGION` | `eu-north1` | `status.region` and `GetBucketLocation` |
| `NEBIUS_MOCKER_STORAGE_PURGE_GRACE` | `168h` | Default soft-delete grace |

## Open questions
- Whether real Nebius allows deleting a non-empty bucket (the mocker refuses, `FAILED_PRECONDITION`).
- The exact `status.domain_name` format and `status.counters` contents.
- Whether real Nebius validates the SigV4 credential-scope region.

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
