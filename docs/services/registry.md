# Container Registry

> Module: `services/registry` · Image: `nebius-mocker-registry` · `NEBIUS_MOCKER_SERVICES=registry`
> M1 tier: **T2** · M3: **T3** OCI data plane · Endpoint: `registry.api.nebius.cloud`

## Overview

Private OCI registries per project (`Registry`), and read access to the **artifacts** (images and tags) stored in them
(`ArtifactService`: Get/List/Delete).

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Registry | `Registry` | `registry` | project | `CREATING, ACTIVE, DELETING, SUSPENDED` |
| Artifact | `Artifact` | `artifact` | registry | read/delete only |

## Emulation design

- `spec.description` is the only user field. `status.registry_fqdn` is synthesized as `cr.<region>.mocker.localhost:4566`,
  and `status.images_count` is maintained from artifacts.
- Delete of a non-empty registry → `FAILED_PRECONDITION`. The real behavior here is an open question.
- **M1**: artifacts are created only via seed or admin API (`POST /_mocker/registry/<id>/artifacts`), so that
  `ArtifactService` List/Get/Delete can be tested.
- **M3 OCI data plane**: an embedded `distribution` server under `/v2/` on the edge port, with namespaces mapped to
  registry IDs (`<fqdn>/<registry-id-suffix>/<image>`). Pushes create `Artifact` records. Auth accepts mocker IAM tokens
  via the Docker token flow, which matches the real `nebius` credential helper approach.
- Permissions: `registry.registries.*`, `registry.artifacts.get/list/delete`, `registry.images.push/pull` (data plane).
  Quotas: `registry.registry.count` (project), `registry.size` (data plane).

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_registry_v1_registry` | `nebius.registry.v1.RegistryService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Data source | `nebius_registry_v1_registry` | `nebius.registry.v1.RegistryService` | Get, GetByName | T2 |

## Open questions
- The real FQDN format and the artifact naming scheme.
- Whether a non-empty registry can be deleted.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.registry.v1.ArtifactService`

Endpoint: `registry.api.nebius.cloud:443` · Proto: [`nebius/registry/v1/artifact_service.proto`](https://github.com/nebius/api/blob/main/nebius/registry/v1/artifact_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetArtifactRequest` | `Artifact` |  |
| `List` | `ListArtifactsRequest` | `ListArtifactsResponse` |  |
| `Delete` | `DeleteArtifactRequest` | `common.v1.Operation` |  |

#### `nebius.registry.v1.RegistryService`

Endpoint: `registry.api.nebius.cloud:443` · Proto: [`nebius/registry/v1/registry_service.proto`](https://github.com/nebius/api/blob/main/nebius/registry/v1/registry_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetRegistryRequest` | `Registry` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Registry` | ✅ |
| `List` | `ListRegistriesRequest` | `ListRegistriesResponse` |  |
| `Create` | `CreateRegistryRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateRegistryRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteRegistryRequest` | `common.v1.Operation` | ✅ |
