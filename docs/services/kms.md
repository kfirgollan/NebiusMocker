# KMS — Key Management Service

> Module: `services/kms` · Image: `nebius-mocker-kms` · `NEBIUS_MOCKER_SERVICES=kms`
> M1 tier: **T1** (crypto functional with real algorithms) · Endpoints: `cpl.kms` (keys), `dpl.kms` (crypto)

## Implementation status

| Piece | Status |
|---|---|
| `nebius.kms.v1.SymmetricKeyService`: Create, Get (`show_scheduled_for_deletion`), GetByName, List (paged, `show_scheduled_for_deletion`), Update (reset mask, `resource_version`), Rotate, Delete, UpdateDeletionDelay, Undelete | ✅ T3 |
| `nebius.kms.v1.AsymmetricKeyService`: the same, without Rotate | ✅ T3 |
| `nebius.kms.v1.SymmetricCryptoService`: Encrypt, Decrypt, GenerateDataKey (`skip_plaintext`) with AES-256-GCM, AAD honored | ✅ T3 |
| `nebius.kms.v1.AsymmetricCryptoService`: SignHash (ECDSA P-256/P-384, ASN.1 DER), GetPublicKey (PKIX PEM), Decrypt (RSA-4096 OAEP SHA-256) | ✅ T3 |
| Delayed deletion on the mock clock: 30-day default, `UpdateDeletionDelay` 1–30 days, crypto rejected while scheduled, Undelete (optionally renamed), purge when the delay ends | ✅ |
| Authorization: rules for every RPC; crypto verbs granted to `editor` and `admin`, not `viewer` (see below) | ✅ |
| `nebius_kms_v1_symmetric_key`, `nebius_kms_v1_asymmetric_key` resources and data sources with the official provider | ✅ e2e: `test/terraform/kms` |
| `nebius kms` CLI flows from the docs (quickstarts, encrypt/decrypt with AAD, rotate, data keys, delete/undelete, openssl verification) | ✅ e2e: `test/kms` |
| Automatic rotation on `rotation_period` | Not yet (the period is stored and validated only; rotate manually) |
| `PENDING_IMPORT` / key import, quotas (3000 keys per tenant) | Not yet |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/kms/{domain,dal,api}`), gosdk contract tests (`test/contract/kms_*`), the Terraform e2e
suite and the CLI e2e suite (`make e2e`).

### Behavior notes

- **Algorithms.** Symmetric keys and data keys accept `AES_256` only. `AES_128` is rejected, because upstream marks it
  deprecated ("it is impossible to create new keys with this algorithm"). Asymmetric keys accept the three algorithms
  in the proto. ECDSA keys sign, and RSA keys decrypt. Using the wrong one is `INVALID_ARGUMENT`.
- **Limits.** Plaintext is at most 64 KiB. A key allows at most 100 manual rotations; the 101st is
  `FAILED_PRECONDITION`. `rotation_period` defaults to 90 days and must be 1–3650 days.
- **Symmetric ciphertext format** (opaque to clients): `0x01` · key version (uint32 big endian) · 12-byte nonce ·
  AES-GCM ciphertext and tag. The GCM additional data is the 5-byte header, the key ID and the caller's
  `aad_context`, so every byte is authenticated and a ciphertext only decrypts under its own key with its own AAD.
  Old versions keep decrypting after `Rotate`. Any failure (tampering, wrong AAD, wrong key) is `INVALID_ARGUMENT`
  with the same message.
- **Key material** is generated in-process with the Go standard library and stored next to the key in the DAL, in a
  separate record kind. It is never returned (except the public key), never logged and never put into operations.
  Domain material types redact themselves in `fmt` and `slog`.
- **Deletion.** `Delete` sets `SCHEDULED_FOR_DELETION`, `deleted_at` and `purge_at = deleted_at + 30 days` (the
  docs don't state a default; 30 days matches the quota note and the API's maximum delay). The key's name is
  freed at once, and `Undelete` takes an optional new name for when it was reused. `Get`/`List` hide the key unless
  `show_scheduled_for_deletion` is set. Purging is lazy: on any access after `purge_at` on the mock clock, the key and
  its material are removed and the key is `NOT_FOUND`. This is observably the same as a background sweep.
- **Permissions.** `kms.{symmetricKeys,asymmetricKeys}.{get,list,create,update,delete}`, plus `kms.symmetricKeys.rotate`,
  `kms.*.undelete`, `kms.symmetricKeys.encrypt` (Encrypt, GenerateDataKey), `kms.symmetricKeys.decrypt`,
  `kms.asymmetricKeys.sign` and `kms.asymmetricKeys.decrypt`. `UpdateDeletionDelay` needs `update`. `GetPublicKey`
  needs `kms.asymmetricKeys.get`, because the public key isn't secret. In the default catalog, `editor` is granted
  `kms.*.{encrypt,decrypt,sign,rotate,undelete}`: the docs say crypto operations need the `editor` role. `viewer`
  can read keys and public keys but cannot use them.
- **Future consumers** (MysteryBox, disk encryption) should call `domain.SymmetricKeyService.Encrypt/Decrypt/
  GenerateDataKey` through their own port, wired in `cmd/`, never by importing `services/kms`.

## Overview

Symmetric keys (`AES_128`, `AES_256`) with rotation, and asymmetric keys (`ECDSA_NIST_P256_SHA_256`,
`ECDSA_NIST_P384_SHA_384`, `RSA_4096_ENC_OAEP_SHA_256`). The data plane provides `Encrypt/Decrypt/GenerateDataKey` and
`SignHash/GetPublicKey/Decrypt`.

| Resource kind | Message | NID prefix | Parent |
|---|---|---|---|
| Symmetric key | `SymmetricKey` | `kmssymkey` | project |
| Asymmetric key | `AsymmetricKey` | `kmsasymkey` | project |

## Emulation design

- Key material is generated in-process with Go's `crypto` packages (real crypto is cheap, so even M1 is functional).
  Material is never exposed except as `GetPublicKey` output.
- `Rotate` creates a new primary key version. Old versions still decrypt. Ciphertexts embed the version.
- Deletion is delayed: `UpdateDeletionDelay` and `Undelete`, with the delay counted on the mock clock. Crypto calls on a
  key scheduled for deletion → `FAILED_PRECONDITION`.
- `GenerateDataKey` returns plaintext and wrapped data keys (envelope encryption). MysteryBox uses the same path internally.
- Permissions: see Behavior notes above.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_kms_v1_asymmetric_key` | `nebius.kms.v1.AsymmetricKeyService` | Create, Update, Get, GetByName, Delete + OperationService.Get | T1 |
| Resource | `nebius_kms_v1_symmetric_key` | `nebius.kms.v1.SymmetricKeyService` | Create, Update, Get, GetByName, Delete + OperationService.Get | T1 |
| Data source | `nebius_kms_v1_asymmetric_key` | `nebius.kms.v1.AsymmetricKeyService` | Get, GetByName | T1 |
| Data source | `nebius_kms_v1_symmetric_key` | `nebius.kms.v1.SymmetricKeyService` | Get, GetByName | T1 |

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.kms.v1.AsymmetricCryptoService`

Endpoint: `dpl.kms.api.nebius.cloud:443` · Proto: [`nebius/kms/v1/asymmetric_crypto_service.proto`](https://github.com/nebius/api/blob/main/nebius/kms/v1/asymmetric_crypto_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `SignHash` | `AsymmetricSignHashRequest` | `AsymmetricSignHashResponse` |  |
| `GetPublicKey` | `AsymmetricGetPublicKeyRequest` | `AsymmetricGetPublicKeyResponse` |  |
| `Decrypt` | `AsymmetricDecryptRequest` | `AsymmetricDecryptResponse` |  |

#### `nebius.kms.v1.AsymmetricKeyService`

Endpoint: `cpl.kms.api.nebius.cloud:443` · Proto: [`nebius/kms/v1/asymmetric_key_service.proto`](https://github.com/nebius/api/blob/main/nebius/kms/v1/asymmetric_key_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateAsymmetricKeyRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateAsymmetricKeyRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetAsymmetricKeyRequest` | `AsymmetricKey` | ✅ |
| `GetByName` | `GetAsymmetricKeyByNameRequest` | `AsymmetricKey` | ✅ |
| `List` | `ListAsymmetricKeysRequest` | `ListAsymmetricKeysResponse` |  |
| `Delete` | `DeleteAsymmetricKeyRequest` | `common.v1.Operation` | ✅ |
| `UpdateDeletionDelay` | `UpdateAsymmetricKeyDeletionDelayRequest` | `common.v1.Operation` |  |
| `Undelete` | `UndeleteAsymmetricKeyRequest` | `common.v1.Operation` |  |

#### `nebius.kms.v1.SymmetricCryptoService`

Endpoint: `dpl.kms.api.nebius.cloud:443` · Proto: [`nebius/kms/v1/symmetric_crypto_service.proto`](https://github.com/nebius/api/blob/main/nebius/kms/v1/symmetric_crypto_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Encrypt` | `SymmetricEncryptRequest` | `SymmetricEncryptResponse` |  |
| `Decrypt` | `SymmetricDecryptRequest` | `SymmetricDecryptResponse` |  |
| `GenerateDataKey` | `GenerateDataKeyRequest` | `GenerateDataKeyResponse` |  |

#### `nebius.kms.v1.SymmetricKeyService`

Endpoint: `cpl.kms.api.nebius.cloud:443` · Proto: [`nebius/kms/v1/symmetric_key_service.proto`](https://github.com/nebius/api/blob/main/nebius/kms/v1/symmetric_key_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateSymmetricKeyRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateSymmetricKeyRequest` | `common.v1.Operation` | ✅ |
| `Rotate` | `RotateSymmetricKeyRequest` | `common.v1.Operation` |  |
| `Get` | `GetSymmetricKeyRequest` | `SymmetricKey` | ✅ |
| `GetByName` | `GetSymmetricKeyByNameRequest` | `SymmetricKey` | ✅ |
| `List` | `ListSymmetricKeysRequest` | `ListSymmetricKeysResponse` |  |
| `Delete` | `DeleteSymmetricKeyRequest` | `common.v1.Operation` | ✅ |
| `UpdateDeletionDelay` | `UpdateSymmetricKeyDeletionDelayRequest` | `common.v1.Operation` |  |
| `Undelete` | `UndeleteSymmetricKeyRequest` | `common.v1.Operation` |  |
