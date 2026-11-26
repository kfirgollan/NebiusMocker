# MysteryBox — Secrets manager

> Module: `services/mysterybox` · Image: `nebius-mocker-mysterybox` · `NEBIUS_MOCKER_SERVICES=iam,mysterybox`
> M1 tier: **T2** · Endpoints: `cpl.mysterybox` (control plane), `dpl.mysterybox` (payload data plane)
> Product name: **SecretStash** (formerly MysteryBox); the API, CLI and Terraform keep `mysterybox`.

## Implementation status

| Piece | Status |
|---|---|
| `SecretService`: Create (inline first version), Get, GetByName, List (paged, `show_scheduled_for_deletion`), Update (reset mask, `resource_version`, primary switch), Delete (soft), Undelete (optional new name) | ✅ T2 |
| `SecretVersionService`: Create (`set_primary`), Get, List, Delete (soft), Undelete | ✅ T2 |
| `PayloadService` (`dpl.mysterybox`): Get, GetByKey, default = primary version | ✅ T2 |
| Payloads sealed at rest with AES-256-GCM under an instance key (`NEBIUS_MOCKER_MYSTERYBOX_KEY`, random by default), bound to the version ID | ✅ (KMS stand-in, see below) |
| Payloads never in logs, errors, `Get`/`List` responses or operation requests (`Operation.request` is redacted) | ✅ |
| Soft delete: `deleted_at`, `purge_at` = +30 days (configurable), purged lazily on the mock clock; a deleted secret releases its name | ✅ |
| Limits: entries per version, value size, key size, versions per secret | ✅ configurable |
| Authorization: `mysterybox.secrets.*`, `mysterybox.secretVersions.*`, `mysterybox.payload.read`; role `mysterybox.payload-viewer` | ✅ |
| In-process payload reads for other modules: `peer.SecretPayloadsKey` (`domain.Service.ResolvePayload`); system-owned secrets: `domain.Service.CreateSystemSecret` | ✅ |
| Terraform: `nebius_mysterybox_v1_secret`, `_secret_version` (resources and data sources), `_secret_payload_entry` (data source), `_secret_payload` and `_secret_payload_entry` (ephemeral) | ✅ e2e: `test/terraform/mysterybox` |
| CLI: `nebius mysterybox secret` / `secret-version` / `payload` flows from the docs | ✅ e2e: `test/cli/mysterybox` |
| Delivery target for IAM access keys with `secret_delivery_mode = MYSTERY_BOX` (`peer.SystemSecrets`) | ✅ contract: `test/contract/iam_accesskey_mysterybox_test.go`; e2e: `test/terraform/iam`, `test/cli/iam` (see `docs/services/iam.md`) |
| Quota `mysterybox.secret.count` enforced on Create (including system secrets for IAM access keys); secrets scheduled for deletion keep counting until purged, as in Nebius ([quotas.md](quotas.md)) | ✅ contract: `test/contract/quotas_areas_test.go` |
| KMS encryption with a customer key | Not yet |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/mysterybox/domain`, `dal`, `backend/aesgcm`), gosdk contract tests
(`test/contract/mysterybox_*`), the Terraform e2e suite (`test/terraform/mysterybox`) and the CLI e2e suite
(`test/cli/mysterybox`), all in `make check` / `make e2e`. The module needs `iam` (it seeds the project and the role
bindings).

### Behavior notes

- **Primary version.** The inline `spec.secret_version` becomes the primary. A version created later becomes primary
  only with `set_primary=true` (as the docs say for the CLI and SDKs), or when the secret has no primary yet. Setting
  `spec.primary_version_id` on Create is rejected; on Update it must name an active version of the same secret.
  Promoting a version bumps the secret's `resource_version`.
- **Input-only fields.** `Get` never returns `spec.secret_version` (it is input-only and holds the payload), and versions
  return only `spec.description`. `spec.secret_version` is immutable. Terraform resends it on every update, so Update
  accepts an identical copy and rejects any change (or adding one to a secret created without it) with
  `INVALID_ARGUMENT`. The check also holds after the inline version is purged, because a sealed copy of the inline spec
  is kept with the secret. An omitted payload (Terraform write-only `sensitive` payloads) compares only the
  description. `set_primary` is not compared: the first version is always primary.
- **Soft delete.** Deleting a secret keeps its versions; Undelete brings them back. Any version can be deleted,
  including the primary; payload reads that default to a deleted primary then fail with `NOT_FOUND`. Undeleting a
  version needs an active secret. Undelete needs the `delete` permission of the resource.
- **Encryption.** In this API revision a secret takes no KMS key, and reports only `status.effective_kms_key_id`. The
  mocker reports the fixed internal key `kmssymkey-e00mock000000mysterybox`, which is not a real KMS key. Encryption sits
  behind the domain port `PayloadSealer`, so a KMS-backed sealer can replace AES-GCM later.
- **Authorization.** Payload reads need `mysterybox.payload.read`, held by `admin` and `mysterybox.payload-viewer` but
  not by `viewer` or `editor` (the docs: "viewing payloads requires the `mysterybox.payload-viewer` role, which is a
  sub-role of `admin` but not `editor`"). The verb is not `get`, so a denied payload read is `PERMISSION_DENIED`, not
  the existence-hiding `NOT_FOUND` used for metadata reads.
- **Update without parent.** The CLI's patch mode sends `metadata.id` without `metadata.parent_id`; the request
  validator accepts that on `Update` RPCs (parents are immutable).

### Limits and configuration

| Variable | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_MYSTERYBOX_MAX_ENTRIES` | 100 | Payload entries per version |
| `NEBIUS_MOCKER_MYSTERYBOX_MAX_VALUE_BYTES` | 65536 | Size of one value (the API's own validation cap) |
| `NEBIUS_MOCKER_MYSTERYBOX_MAX_VERSIONS` | 100 | Versions per secret, including those scheduled for deletion |
| `NEBIUS_MOCKER_MYSTERYBOX_PURGE_DELAY` | `720h` | Time a deleted secret or version can be restored (docs: 30 days) |
| `NEBIUS_MOCKER_MYSTERYBOX_KEY` | random | Base64 AES-256 key payloads are sealed with |

Keys are limited to 256 bytes. Upstream publishes only the value cap, the 30-day restore window and the quota of
3000 secrets per region, so the entry, key and version limits are assumptions.

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
  (all entries, or a given `version_id`) and `GetByKey`. These need `mysterybox.payload.read`, which `viewer` and
  `editor` do **not** include.
- Payloads are stored encrypted at rest with an instance key. A later revision may call kms `SymmetricCrypto.Encrypt`
  through a `PayloadSealer` (KMS permissions and key state would then matter). `status.effective_kms_key_id` is filled in.
- Soft delete with `deleted_at`/`purge_at` and `Undelete`, for both secrets and versions, on the mock clock.
- Limits: entries per version, value size, and versions per secret, configurable with upstream-like defaults.
- It is also the delivery target for `iam.v2.AccessKey` secrets (`secret_delivery_mode=MYSTERY_BOX`). IAM creates
  the secret through the `peer.SystemSecrets` capability, which this module provides (`pkg/peer/systemsecrets.go`).
  The secret is ordinary: it is named after the access key ID, holds one entry `secret`, needs
  `mysterybox.payload.read` to read, and outlives the key (see `docs/services/iam.md`).
- Permissions: `mysterybox.secrets.*`, `mysterybox.secretVersions.*`, `mysterybox.payload.read`. Quotas: `mysterybox.secret.count` (project).

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

`mysterybox_v1_secret_payload_entry` exists as a data source **and** as an ephemeral resource;
`mysterybox_v1_secret_payload` is ephemeral only. The ephemeral variants keep payloads out of Terraform state. The
mocker behaves the same either way.

## Open questions
- Upstream entry, key and version limits (not published).
- Whether upstream counts versions scheduled for deletion toward the version limit (the mocker does).

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
