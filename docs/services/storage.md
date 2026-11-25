# Object Storage

> Module: `services/storage` · Image: `nebius-mocker-storage` · `NEBIUS_MOCKER_SERVICES=storage`
> M1 tier: **T2** bucket, **T1** inventory/transfer · M3: **T3** S3 data plane · Endpoints: `cpl.storage`, `transfer.storage`


## Implementation status

| Piece | Status |
|---|---|
| `nebius.storage.v1.BucketService`: Get, GetByName, List (paged), ListWithFilter, Create, Update (reset mask, `resource_version`), Delete (soft, `purge_at`/`ttl`), Purge, Undelete | ✅ T2 |
| Bucket semantics: global S3 names, naming rules, `IMMUTABLE_NAME`, `versioning_policy` default + transition rules, `status.state/domain_name/region/bucket_type`, soft delete with `deleted_at`/`purge_at`, background purge sweeper | ✅ |
| `status.counters`: per storage class, current/noncurrent simple and multipart objects, multipart uploads and in-flight parts, computed from the object store on every read | ✅ |
| `spec.max_size_bytes`: writes that would exceed it are refused at the S3 front (`QuotaExceeded`) | ✅ |
| `spec.cors`: preflight `OPTIONS` and `Access-Control-*` headers at the S3 front | ✅ |
| `spec.bucket_policy`: group and anonymous rules, `storage.*` roles, path patterns, evaluated at the S3 front; `status.anonymous_access_enabled` | ✅ |
| `spec.lifecycle_configuration`: expiration (days/date), expired delete markers, noncurrent expiration (+ `newer_noncurrent_versions`), abort incomplete multipart uploads, with prefix/size/tag filters, applied on the mock clock | ✅ |
| `spec.default_storage_class`, `spec.force_storage_class`: `STANDARD` and `ENHANCED_THROUGHPUT` | ✅ |
| Lifecycle transitions (`transition`, `noncurrent_version_transition`), `last_access_filter`, `spec.last_access`, storage class `INTELLIGENT`, `spec.object_audit_logging` other than `NONE` | ❌ rejected with `INVALID_ARGUMENT` naming the field (see below) |
| Backing buckets in RustFS (created, versioned, force-deleted with all versions on purge) | ✅ |
| S3 data plane: S3 front on `:4580`, SigV4 with Nebius access keys (credential-scope region checked), anonymous requests for policy grants, IAM-authorized, relayed to RustFS | ✅ (`NEBIUS_MOCKER_OBJSTORE=rustfs`) |
| S3 bucket calls: CreateBucket (`aws s3 mb`), DeleteBucket (`aws s3 rb`), Get/Put/Delete BucketLifecycleConfiguration, Get/Put/Delete BucketCors, Get/PutBucketVersioning, GetBucketLocation, on the Nebius bucket records | ✅ |
| `nebius_storage_v1_bucket` resource + data source with the official provider (incl. cors, bucket_policy, lifecycle, max_size_bytes; counters through the data source) | ✅ e2e: `test/terraform/storage` (`make e2e`, `make e2e-storage`) |
| Objects with the unmodified `hashicorp/aws` provider (`aws_s3_object` + data source) against the S3 front | ✅ e2e: `test/terraform/storage` (`make e2e-storage`) |
| Go SDK: buckets with `gosdk`, objects with the AWS SDK for Go v2 | ✅ e2e: `test/storage` (`make e2e-storage`) |
| Documented CLI flows: `nebius storage bucket …`, the quick start (`nebius iam …` service account + access key, `aws configure set …`, `aws s3 mb/cp/ls/sync/mv/rm/rb`, `aws s3api put/get-bucket-lifecycle-configuration`) | ✅ e2e: `test/storage/cli_e2e_test.go` (`make e2e-storage`; pinned CLIs installed into the user cache, never system-wide) |
| Filesystem buckets (`spec.filesystem_bucket`) | ❌ rejected with `INVALID_ARGUMENT` |
| `InventoryService`, `TransferService` (v1, v1alpha1) | Not yet (UNIMPLEMENTED) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100 (hashicorp/aws 6.70.0, RustFS 1.0.6, nebius CLI 0.12.312, AWS CLI 2.42.3).

Covered by domain unit tests with fakes (`services/storage/domain`: policy, CORS, lifecycle executor, counters, storage
classes, S3 bucket calls), DAL tests (`services/storage/dal`), S3 front tests (`services/storage/s3`: routing, SigV4
accept/reject incl. region, XML conversion), `pkg/sigv4` and `pkg/objstore` (inventory conformance) tests, gosdk contract
tests on the in-memory object store (`test/contract/storage_bucket_test.go`), and the Docker-gated suites above
(`make e2e-storage` starts and removes RustFS).

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
(`rustfs/rustfs:1.0.6`, multi-arch), reached through the shared `pkg/objstore` port (`Store` plus `Inventory`:
versions, multipart uploads, tags). NebiusMocker provides the Nebius-shaped wrapping: the gRPC control plane,
authentication and authorization, and every bucket setting that RustFS cannot apply with Nebius semantics.

```text
terraform / gosdk / nebius CLI ──gRPC :4566──▶ BucketService ──▶ dal (pkg/store: bucket metadata, global name index)
                                                   └──────────▶ pkg/objstore (RustFS): ensure / versioning / force-delete,
                                                                inventory for status.counters and lifecycle rules

aws CLI / AWS SDK / hashicorp/aws / browsers ──S3 HTTP :4580──▶ services/storage/s3 (S3 front)
   0. OPTIONS: CORS preflight from spec.cors (no authentication)
   1. SigV4 verify (pkg/sigv4): credential-scope region must be the bucket region; secret of the iam.v2 AccessKey
      (peer.AccessKeys, provided by IAM). Unsigned requests are anonymous.
   2. domain.S3Gate: bucket must be an ACTIVE Nebius bucket; allowed by an IAM permit of the key's service account,
      or by a spec.bucket_policy rule (group via peer.GroupMembers, or anonymous)
   3. bucket calls (mb/rb, versioning, lifecycle, CORS, location) → the Nebius bucket record
   4. object calls: storage class and max_size_bytes applied, relayed path-style to RustFS re-signed with the RustFS
      credentials (pkg/objstore/rustfs.Forwarder); responses get CORS headers and Nebius storage-class names
```

- **Nebius metadata is the source of truth.** A bucket exists for S3 only if it is an `ACTIVE` Nebius bucket; soft-deleted
  buckets answer `NoSuchBucket`. `ListBuckets` and `GetBucketLocation` are answered from Nebius metadata (the region is
  `eu-north1`). Users never see RustFS credentials.
- **S3 bucket calls** (as the Nebius quick start uses them): `CreateBucket` (`aws s3 mb`) creates a Nebius bucket with
  default settings in the project of the access key's service account (needs `storage.buckets.create` there; a
  `LocationConstraint` other than the region is `InvalidLocationConstraint`; a taken name is `BucketAlreadyExists`).
  `DeleteBucket` (`aws s3 rb`) is the default Nebius delete: soft delete with the default grace; non-empty buckets are
  `BucketNotEmpty`. `Put/Get/DeleteBucketLifecycleConfiguration`, `Put/Get/DeleteBucketCors` and
  `Put/GetBucketVersioning` read and write `spec.lifecycle_configuration`, `spec.cors` and `spec.versioning_policy` with
  the same validation as `BucketService.Update` (violations come back as `InvalidArgument` naming the field). These
  calls are synchronous (S3 has no operations); they bump `resource_version`. Other bucket sub-resource writes (AWS
  policies, ACLs, website, …) answer `NotImplemented`; Nebius bucket policies are set with the Nebius API.
- **Bucket names** are globally unique across all projects in the mocker instance (S3 DNS semantics), checked with a
  global name index. S3 rules: 3–63 characters of `a-z 0-9 . -`, alphanumeric at both ends, no `..`/`.-`/`-.`, not an IP
  address, no `xn--` prefix or `-s3alias` suffix. The prefix `nebius-mocker-` is reserved for internal buckets (e.g.
  compute image blobs) that share the RustFS instance. The name is immutable (`IMMUTABLE_NAME`) and stays reserved until purge.
  Nebius bucket names map 1:1 to RustFS bucket names.
- `status.domain_name` = `<name>.<NEBIUS_MOCKER_STORAGE_S3_DOMAIN>` (default `<name>.storage.eu-north1.mocker.localhost`);
  requests to that host are served virtual-hosted style, everything else path style.
- **Lifecycle of a bucket**: Create stores the record `CREATING`, creates the RustFS bucket (and versioning) in the
  operation's work step, then `ACTIVE`; a backend failure fails the operation and removes the record. Update sets
  `UPDATING` while the versioning change is applied. Operations are asynchronous with `NEBIUS_MOCKER_OP_DELAY`.
- **Versioning**: `versioning_policy` defaults to `DISABLED` (`NON_EMPTY_DEFAULT`); transitions disabled → enabled/suspended
  and enabled ↔ suspended are applied to RustFS; enabled/suspended → disabled is `INVALID_ARGUMENT`.
- **Soft delete**: `Delete` needs an empty bucket (`FAILED_PRECONDITION` otherwise) and moves it to `SCHEDULED_FOR_DELETION`
  with `deleted_at` and `purge_at` = now + `ttl`, or `purge_at`, or the default grace (7 days, `NEBIUS_MOCKER_STORAGE_PURGE_GRACE`).
  A zero `ttl` or a past `purge_at` purges at once. `Undelete` restores `ACTIVE`; `Purge` deletes immediately. A sweeper
  (every minute, mock clock) purges expired buckets. Purging force-deletes the RustFS bucket with all object versions.
- **Permissions**: `storage.buckets.{get,list,create,update,delete}` (`Purge` checks `delete`, `Undelete` checks `update`);
  S3 object reads `storage.objects.get`, listings (and ListParts) `storage.objects.list`, writes (put, copy, multipart,
  tagging) `storage.objects.create`, deletes `storage.objects.delete`; bucket-level S3 reads `storage.buckets.get`, S3
  bucket configuration writes `storage.buckets.update`, `aws s3 mb` `storage.buckets.create` on the project, `aws s3 rb`
  `storage.buckets.delete`. CopyObject/UploadPartCopy also need `get` on the source. The seeded
  `viewer`/`editor`/`admin` roles therefore apply to objects. `NEBIUS_MOCKER_AUTHZ` applies to the S3 front too (audit
  logs denials, enforce rejects with `AccessDenied`); anonymous requests are only ever allowed by a bucket policy.
- **SigV4** (`pkg/sigv4`): header and presigned-URL forms, ±15 min clock skew, presign expiry ≤ 7 days, and aws-chunked
  bodies (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD[-TRAILER]`, `STREAMING-UNSIGNED-PAYLOAD-TRAILER`; chunk signatures verified,
  trailing checksums dropped). Non-streaming payload hashes are relayed so RustFS re-checks the body; bodies the front
  consumes itself (bucket configuration XML) are checked against the signed hash. The credential-scope region must be
  `NEBIUS_MOCKER_STORAGE_REGION` (`eu-north1`): otherwise `400 AuthorizationHeaderMalformed` (presigned:
  `AuthorizationQueryParametersError`) with AWS's message "the region 'X' is wrong; expecting 'eu-north1'" and a
  `<Region>` element. Nebius does not document its answer, so this follows AWS S3.
- **Other modules** reach buckets through `pkg/peer`: storage provides `peer.BucketResolverKey` (bucket ID → object-store
  bucket name) for services that write into user buckets (audit exports) and read from them (compute image imports
  use the bucket name directly, since names map 1:1). IAM provides `peer.GroupMembersKey` for bucket policy group rules.
- **Docker-free mode** (`NEBIUS_MOCKER_OBJSTORE=memory`, the default): buckets work fully over gRPC (counters reflect the
  in-memory store, which is never written by users); the S3 front still authenticates and authorizes, serves the bucket
  calls, then answers `NotImplemented` for data operations.

### Bucket settings

Every `BucketSpec` field either has its Nebius effect or is rejected with `INVALID_ARGUMENT` naming the field. Nothing is
stored without effect.

- **`max_size_bytes`** (0 = unlimited) is checked by the S3 front before every write that adds bytes (`PutObject`,
  `CopyObject`, `UploadPart`, `UploadPartCopy`). The bucket's size is its current and noncurrent object versions plus
  uploaded multipart parts, computed from RustFS. A write that would exceed the limit is refused with
  `400 QuotaExceeded` naming the limit. An overwrite in a never-versioned bucket counts only the size difference.
  Concurrent writes are not serialized, so — as the Nebius API documents — the bucket can end slightly above the limit.
  Nebius does not document the error code; `QuotaExceeded` is the mocker's choice (see Open questions).
- **`cors`** is served by the S3 front, not RustFS (RustFS's own CORS headers are dropped). Validation: methods
  `GET PUT POST DELETE HEAD`, at most one `*` per origin and allowed header. Preflight `OPTIONS` (unauthenticated): the
  first rule that allows the origin (one wildcard, case-sensitive), the `Access-Control-Request-Method` and every
  `Access-Control-Request-Headers` entry (case-insensitive, wildcards) answers `200` with
  `Access-Control-Allow-Origin` (`*` for a `*` rule, else the origin plus `Access-Control-Allow-Credentials: true`),
  `-Allow-Methods` (the rule's methods), `-Allow-Headers` (the requested ones), `-Expose-Headers`, `-Max-Age`. No CORS
  configuration or no match is `403 AccessForbidden` ("CORSResponse: …"); a missing `Origin` or request method is
  `400`. Actual requests with an `Origin` get the same headers when a rule matches, and `Vary: Origin, …` always.
- **`bucket_policy`** is evaluated by the S3 gate: a request is allowed by an IAM permit **or** by any policy rule
  (rules are independent and additive). A rule applies when its subject matches (`group_id`: the requester's service
  account is a current member; `anonymous`: everyone, including unsigned requests), one of its roles includes the S3
  action, and one of its paths matches: object actions match the key (a full key, or a prefix ending in one `*`; `?`
  matches exactly one character; `*` alone is the whole bucket); listings match when the requested `prefix` lies under a
  wildcard path; other bucket actions match only `*`. Roles (the Nebius supported-actions table):
  `storage.object-viewer` (Get/Head/GetTagging), `storage.object-lister` (ListObjects[V2], ListObjectVersions),
  `storage.viewer` (both, plus ListMultipartUploads/ListParts, HeadBucket, Get bucket location/versioning/lifecycle/CORS),
  `storage.uploader` (Put/Copy/multipart/PutTagging plus the bucket reads), `storage.object-editor` (all object calls
  incl. deletes), `storage.editor` (all of the above plus Put bucket versioning/lifecycle/CORS). Validation: known
  roles only, a subject per rule, valid paths, and anonymous rules may only grant `storage.viewer`,
  `storage.object-viewer` or `storage.object-lister`. `status.anonymous_access_enabled` is true when any rule is
  anonymous. Bulk `DeleteObjects` is authorized **per key**: an IAM permit
  on the bucket allows every key; otherwise each key needs a policy rule whose path matches it. Only allowed keys are
  relayed to RustFS (with a recomputed `Content-MD5`); denied keys come back as `<Error><Code>AccessDenied</Code>`
  entries in the `DeleteResult`, as S3 reports per-object failures (`aws s3 rm --recursive`,
  `s3api delete-objects`).
- **`lifecycle_configuration`** is applied by the mocker, not RustFS (whose lifecycle runs on wall time): the storage
  module's minute sweeper (mock clock) evaluates every `ENABLED` rule of every active bucket against a snapshot of its
  versions and uploads. Days count from the object's `LastModified` (noncurrent days from when the next version
  superseded it; uploads from `Initiated`) and are rounded up to the next midnight UTC, as S3 does; `date` rules apply
  from that midnight. Actions: `expiration` (deletes the current version — a delete marker in versioned buckets),
  `expired_object_delete_marker` (removes a delete marker that is the only version left), `noncurrent_version_expiration`
  (keeping `newer_noncurrent_versions` newest), `abort_incomplete_multipart_upload`. Filters: `prefix`,
  `object_size_greater_than_bytes` / `_less_than_bytes` (strict), `tags` (all must match). Validation: unique `id`
  (≤ 255 chars), `status` set, at least one action, `expiration` has exactly one of `days`/`date`/`expired_object_delete_marker`,
  `date` at midnight UTC, tags not combined with delete-marker or abort actions.
  **Rejected by name**: `rules[i].transition`, `rules[i].noncurrent_version_transition` and `last_access_filter`.
  RustFS cannot change an object's storage class without rewriting it, which would reset `LastModified` and create a
  new version, breaking the expiration and versioning semantics above.
- **Storage classes**: `STANDARD` and `ENHANCED_THROUGHPUT`. RustFS knows only `STANDARD` and `REDUCED_REDUNDANCY`, so
  the front stores `ENHANCED_THROUGHPUT` objects as `REDUCED_REDUNDANCY` and translates both ways
  (`x-amz-storage-class` request and response headers, `<StorageClass>` in listings); users never see the backend name.
  Uploads (`PutObject`, `CopyObject`, `CreateMultipartUpload`) use the header's class, else `default_storage_class`
  (unset = `STANDARD`); `force_storage_class` ignores the header. An unknown class is `InvalidStorageClass`.
  **Rejected by name**: `INTELLIGENT` (access-based tiering needs last-access tracking, which is not emulated) as
  `default_storage_class` or upload class, and `spec.last_access`.
- **`status.counters`** are computed on every `Get`, `GetByName` and `List` from RustFS: one entry per storage class
  (`STANDARD` always present), with current and noncurrent simple/multipart object counts and bytes (multipart = the
  object's ETag has a part-count suffix), multipart uploads in flight with their uploaded parts. Delete markers count
  nowhere.
- **`object_audit_logging`**: only `NONE` (or unset) is accepted; `MUTATE_ONLY`/`ALL` are rejected by name, because S3
  object requests are not written to the audit log.

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
- Whether real Nebius allows deleting a non-empty bucket (the mocker refuses, `FAILED_PRECONDITION`); docs show
  `delete --ttl 0s` for non-empty buckets.
- The exact `status.domain_name` format, and whether `status.counters` lists a `STANDARD` entry for an empty bucket.
- The S3 error Nebius returns when `max_size_bytes` is exceeded (the mocker answers `400 QuotaExceeded`), and whether
  the size includes noncurrent versions and uploaded parts (the mocker counts both).
- The error Nebius returns for a wrong SigV4 credential-scope region (the mocker follows AWS:
  `AuthorizationHeaderMalformed`).
- Whether `aws s3 rb` soft-deletes like the Nebius API's default (the mocker does).

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
