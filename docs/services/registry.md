# Container Registry

> Module: `services/registry` · `NEBIUS_MOCKER_SERVICES=registry`
> Tier: **T2** control plane · **T3** OCI data plane through [Harbor](https://goharbor.io) · Endpoint: `registry.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.registry.v1.RegistryService`: Create, Get, GetByName, List (paged), Update (reset mask, `resource_version`), Delete (`force`) | ✅ T2 |
| `nebius.registry.v1.ArtifactService`: List (paged), Get, Delete. Read live from the image backend | ✅ T2 |
| `nebius_registry_v1_registry` resource and data source with the official Terraform provider (by ID and by name) | ✅ e2e: `test/terraform/registry` |
| Docker data plane: `docker login/push/pull` with Nebius IAM tokens, images stored in Harbor | ✅ e2e: `make e2e-registry` |
| `nebius iam get-access-token \| docker login <fqdn> --username iam --password-stdin` with the official `nebius` CLI | ✅ e2e: `make e2e-registry` |
| `nebius.iam.v1.ProfileService.Get` (the CLI calls it before printing a token; served by the IAM module) | ✅ |
| Authorization of RPCs and of docker pull/push under `NEBIUS_MOCKER_AUTHZ` | ✅ |
| `nebius registry configure-helper` (docker credential helper) | Not yet: it only writes `cr.<region>.nebius.cloud` entries, so it cannot target a local FQDN |
| `ListRegistries`/`ListArtifacts` `filter` | Ignored |
| Quotas (`registry.registry.count`, `registry.size`) | Not yet |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312, Harbor v2.16.0.

Tests: domain unit tests with fakes (`services/registry/domain`), DAL tests on the memory store
(`services/registry/dal`), Harbor client and docker front tests over `httptest` (`services/registry/backend/harbor`,
`services/registry/docker`), gosdk contract tests against the in-memory backend (`test/contract/registry_test.go`),
Terraform CRUD (`make e2e`), and the Docker + Harbor + CLI flow (`make e2e-registry`).

## Overview

Private OCI registries per project (`Registry`), and read access to the **artifacts** (images and tags) stored in them
(`ArtifactService`: Get/List/Delete).

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Registry | `Registry` | `registry` | project | `CREATING → ACTIVE` (`DELETING`, `SUSPENDED` unused) |
| Artifact | `Artifact` | `artifact` | registry | `ACTIVE`; read/delete only, created by `docker push` |

## Emulation design

NebiusMocker does not implement a registry. It wraps Harbor and adds the Nebius shape:

```text
terraform / gosdk / nebius CLI ──gRPC :4566──▶ RegistryService / ArtifactService ──Harbor REST API (admin)──▶ Harbor
docker login/push/pull ────────HTTP :4590──▶ docker front (IAM token → authz) ──reverse proxy (admin)───────▶ Harbor /v2/
```

- **Metadata is ours.** Registries live in `pkg/store` through `services/registry/dal`, like every other resource.
  The backend only holds images. `Create` stores the registry as `CREATING`. The operation's work step creates the
  backend namespace (idempotent) and marks the registry `ACTIVE`.
- **One registry is one Harbor project**, named after the registry ID without its prefix: `registry-e00abc…` becomes
  project `e00abc…`. Image names follow Nebius: `<registry_fqdn>/<registry ID suffix>/<image>:<tag>`.
- **`status.registry_fqdn`** is `NEBIUS_MOCKER_REGISTRY_FQDN`, or `127.0.0.1:<docker endpoint port>` by default.
  Docker treats `127.0.0.0/8` as an insecure (plain HTTP) registry, so no TLS or daemon configuration is needed.
  `status.images_count` (and the deprecated `spec.images_count`) is read live from the backend.
- **Artifacts** are read live from Harbor (`/api/v2.0/projects/{p}/repositories/{r}/artifacts?with_tag=true`):
  one artifact per manifest. `name` is the image (repository) name inside the registry, `tags` are its tags, and
  `type` is `MANIFEST` or `MANIFEST_LIST`. Artifact NIDs are assigned on first sight and kept in a small index
  (`registry, repository, digest → artifact-…`), so IDs stay stable across listings. `Delete` removes the manifest from
  Harbor.
- **Delete** of a registry that still holds images → `FAILED_PRECONDITION`, unless `force=true` (the real behavior is
  an open question). Deleting removes every Harbor repository and then the project.
- **Docker front** (`services/registry/docker`, `NEBIUS_MOCKER_REGISTRY_LISTEN`, default `:4590`; started only with
  the Harbor backend) serves `/v2/`:
  - `GET /v2/` without credentials → `401` + `WWW-Authenticate: Basic`. The password of `docker login --username iam`
    is the Nebius IAM token, the username is ignored, and a `Bearer` token is accepted too.
  - Every `/v2/<ns>/<image>/{manifests,blobs,tags,referrers}/…` request is authorized by the domain: the token must be
    valid, `<ns>` must belong to an `ACTIVE` registry, and the caller needs a permission on it. Cross-repository blob
    mounts (`?from=`) also need pull on the source. Allowed requests are reverse-proxied to Harbor with Harbor's admin
    credentials. Upload `Location` headers are made relative, so docker keeps talking to the front.
  - `/v2/_catalog` is not served; list images with `ArtifactService`.
- **Permissions**: the RPCs use `registry.registries.*` and `registry.artifacts.{get,list,delete}`. Docker actions map
  onto the existing verbs, so the default roles work unchanged: **pull = `registry.artifacts.get`** (viewer),
  **push = `registry.artifacts.create`** (editor), **delete = `registry.artifacts.delete`** (editor). Under
  `NEBIUS_MOCKER_AUTHZ=audit` docker denials are only logged, and under `off` they are skipped.
- **Backends** (`NEBIUS_MOCKER_REGISTRY_BACKEND`): `memory` (default) keeps image metadata in process memory and
  serves no docker endpoint (unit and contract tests add images directly). `harbor` uses a Harbor server.

### Configuration

| Env | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_REGISTRY_BACKEND` | `memory` | `memory` or `harbor` |
| `NEBIUS_MOCKER_REGISTRY_LISTEN` | `:4590` | Docker endpoint (Harbor backend only); `off` disables it |
| `NEBIUS_MOCKER_REGISTRY_FQDN` | `127.0.0.1:<listen port>` | `status.registry_fqdn` and the host part of image names |
| `NEBIUS_MOCKER_REGISTRY_HARBOR_URL` | `http://127.0.0.1:8080` | Harbor base URL |
| `NEBIUS_MOCKER_REGISTRY_HARBOR_USERNAME` / `_PASSWORD` | `admin` / `Harbor12345` | Harbor admin credentials (never given to users) |

### Local workflow

```bash
test/registry/harbor/harbor.sh up /tmp/harbor          # throwaway Harbor v2.16.0; prints its URL
. /tmp/harbor/env
NEBIUS_MOCKER_REGISTRY_BACKEND=harbor NEBIUS_MOCKER_REGISTRY_HARBOR_URL=$NEBIUS_MOCKER_E2E_HARBOR_URL make run

echo nmock-dev-editor > /tmp/mocker-token               # a built-in dev token
nebius profile create mocker --endpoint localhost:4566 --token-file /tmp/mocker-token   # see below
nebius registry create --name app --insecure            # status.registry_fqdn: 127.0.0.1:4590
nebius iam get-access-token --insecure | docker login 127.0.0.1:4590 --username iam --password-stdin
docker push 127.0.0.1:4590/<registry ID without "registry-">/app:v1
test/registry/harbor/harbor.sh down /tmp/harbor
```

The CLI dials `<service>.<endpoint>` (for example `registry.localhost:4566`), so the endpoint must be
`localhost:<port>`, not an IP address. Plaintext needs `--insecure` on every command. On **Docker Desktop** the daemon
runs in a VM and cannot reach the host's `127.0.0.1`. The e2e test then runs a tiny nginx forwarder with host
networking, `test/registry/harbor/forwarder.nginx.conf` (Harbor's own `goharbor/nginx-photon` image), which makes
the VM's `127.0.0.1:<port>` proxy to `host.docker.internal:<port>`. Use the same trick by hand. A native Linux daemon
needs nothing.

### Harbor for tests

`test/registry/harbor/harbor.sh` generates the configuration with the official `goharbor/prepare:v2.16.0` image (the
first release with arm64 images). It then starts Harbor under a unique compose project with an override that drops the
fixed container names, the syslog logging driver (fixed host port 1514) and the fixed proxy port. The proxy binds
`127.0.0.1:<random>`. The script waits for `/api/v2.0/health` plus an authenticated `/v2/`, and `down` removes the
containers, networks, volumes and data dir. `make e2e-registry` wraps the whole thing with a trap, so Harbor is removed
even when tests fail.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_registry_v1_registry` | `nebius.registry.v1.RegistryService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 ✅ |
| Data source | `nebius_registry_v1_registry` | `nebius.registry.v1.RegistryService` | Get, GetByName | T2 ✅ |

## Open questions
- The real FQDN format for private endpoints, and whether the artifact `name` includes the registry path.
- Whether a non-empty registry can be deleted without `force`.

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
