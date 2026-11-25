# KMS — Key Management Service

> Module: `services/kms` · Image: `nebius-mocker-kms` · `NEBIUS_MOCKER_SERVICES=kms`
> M1 tier: **T1** (crypto functional with real algorithms) · Endpoints: `cpl.kms` (keys), `dpl.kms` (crypto)

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
- Permissions: `kms.symmetricKeys.*`, `kms.asymmetricKeys.*` (+ `rotate`, `undelete`), `kms.crypto.encrypt/decrypt/sign`.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_kms_v1_asymmetric_key` | `nebius.kms.v1.AsymmetricKeyService` | Create, Update, Get, GetByName, Delete + OperationService.Get | T1 |
| Resource | `nebius_kms_v1_symmetric_key` | `nebius.kms.v1.SymmetricKeyService` | Create, Update, Get, GetByName, Delete + OperationService.Get | T1 |
| Data source | `nebius_kms_v1_asymmetric_key` | `nebius.kms.v1.AsymmetricKeyService` | Get, GetByName | T1 |
| Data source | `nebius_kms_v1_symmetric_key` | `nebius.kms.v1.SymmetricKeyService` | Get, GetByName | T1 |

## Open questions
- The ciphertext format (opaque in the API, but clients might inspect lengths).
- The default deletion delay and its allowed range.

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
