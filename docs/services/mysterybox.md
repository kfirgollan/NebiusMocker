# MysteryBox — Secrets manager

> Module: `services/mysterybox` · Image: `nebius-mocker-mysterybox` · `NEBIUS_MOCKER_SERVICES=mysterybox`
> M1 tier: **T2** · Endpoints: `cpl.mysterybox` (control plane), `dpl.mysterybox` (payload data plane)

## Overview

Secrets with immutable **versions**. Each version holds a payload of key → `string_value | binary_value` entries.
A secret has a primary version and is optionally encrypted with a KMS key. The control plane manages secrets and versions;
the data plane (`PayloadService`) reads payloads.

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Secret | `Secret` | `mbsec` | project | `ACTIVE, SCHEDULED_FOR_DELETION` |
| Secret version | `SecretVersion` | `mbsecver` | secret | active / scheduled for deletion |

## Emulation design

- `SecretSpec.secret_version` (immutable, input on Create) creates the first version inline. `primary_version_id` can be
  switched on Update. `SecretVersionSpec.set_primary=true` promotes a new version.
- Payload entries are **sensitive**. They are never logged or journaled, and are returned only by `PayloadService.Get`
  (all entries, or a given `version_id`) and `GetByKey`. These need `mysterybox.payload.get`, which `viewer` does **not**
  include.
- Payloads are stored encrypted at rest with an instance key. When `kms_key_id` is set, the module calls `peer` → kms
  `SymmetricCrypto.Encrypt`, so KMS permissions and key state matter (a disabled key → payload reads fail).
  `status.effective_kms_key_id` is filled in.
- Soft delete with `deleted_at`/`purge_at` and `Undelete`, for both secrets and versions, on the mock clock.
- Limits: entries per version, value size, and versions per secret, configurable with upstream-like defaults.
- It is also the delivery target for `iam.v2.AccessKey` secrets (`secret_delivery_mode=MYSTERYBOX`).
- Permissions: `mysterybox.secrets.*`, `mysterybox.secretVersions.*`, `mysterybox.payload.get`. Quotas: `mysterybox.secret.count` (project).

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_mysterybox_v1_secret` | `nebius.mysterybox.v1.SecretService` | Create, Update, Get, GetByName, Delete + OperationService.Get | T2 |
| Resource | `nebius_mysterybox_v1_secret_version` | `nebius.mysterybox.v1.SecretVersionService` | Create, Get, Delete + OperationService.Get | T2 |
| Data source | `nebius_mysterybox_v1_secret` | `nebius.mysterybox.v1.SecretService` | Get, GetByName | T2 |
| Data source | `nebius_mysterybox_v1_secret_payload_entry` | `nebius.mysterybox.v1.PayloadService` | GetByKey | T2 |
| Data source | `nebius_mysterybox_v1_secret_version` | `nebius.mysterybox.v1.SecretVersionService` | Get | T2 |
| Ephemeral | `nebius_mysterybox_v1_secret_payload` | `nebius.mysterybox.v1.PayloadService` | Get | T2 |
| Ephemeral | `nebius_mysterybox_v1_secret_payload_entry` | `nebius.mysterybox.v1.PayloadService` | GetByKey | T2 |

The `mysterybox_v1_secret_payload` and `_payload_entry` objects exist as data sources **and** as ephemeral resources.
The ephemeral variants keep payloads out of Terraform state. The mocker behaves the same either way.

## Open questions
- Size limits, and the default purge delay.
- Whether `Get` on a secret returns `secret_version` input fields (`INPUT_ONLY` semantics).

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.mysterybox.v1.PayloadService`

Endpoint: `dpl.mysterybox.api.nebius.cloud:443` · Proto: [`nebius/mysterybox/v1/payload_service.proto`](https://github.com/nebius/api/blob/main/nebius/mysterybox/v1/payload_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetPayloadRequest` | `SecretPayload` | ✅ |
| `GetByKey` | `GetPayloadByKeyRequest` | `SecretPayloadEntry` | ✅ |

#### `nebius.mysterybox.v1.SecretService`

Endpoint: `cpl.mysterybox.api.nebius.cloud:443` · Proto: [`nebius/mysterybox/v1/secret_service.proto`](https://github.com/nebius/api/blob/main/nebius/mysterybox/v1/secret_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateSecretRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateSecretRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetSecretRequest` | `Secret` | ✅ |
| `GetByName` | `GetSecretByNameRequest` | `Secret` | ✅ |
| `List` | `ListSecretsRequest` | `ListSecretsResponse` |  |
| `Delete` | `DeleteSecretRequest` | `common.v1.Operation` | ✅ |
| `Undelete` | `UndeleteSecretRequest` | `common.v1.Operation` |  |

#### `nebius.mysterybox.v1.SecretVersionService`

Endpoint: `cpl.mysterybox.api.nebius.cloud:443` · Proto: [`nebius/mysterybox/v1/secret_version_service.proto`](https://github.com/nebius/api/blob/main/nebius/mysterybox/v1/secret_version_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateSecretVersionRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetSecretVersionRequest` | `SecretVersion` | ✅ |
| `List` | `ListSecretVersionsRequest` | `ListSecretVersionsResponse` |  |
| `Delete` | `DeleteSecretVersionRequest` | `common.v1.Operation` | ✅ |
| `Undelete` | `UndeleteSecretVersionRequest` | `common.v1.Operation` |  |
