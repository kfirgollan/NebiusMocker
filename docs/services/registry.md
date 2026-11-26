# Container Registry

> Module: `services/registry` · `NEBIUS_MOCKER_SERVICES=registry`
> Tier: **T2** control plane · **T3** OCI data plane through [Harbor](https://goharbor.io) · Endpoint: `registry.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.registry.v1.RegistryService`: Create, Get, GetByName, List (paged), Update (reset mask, `resource_version`), Delete (`force`) | ✅ T2 |
| `nebius.registry.v1.ArtifactService`: List (paged), Get, Delete. Read live from the image backend | ✅ T2 |
| `nebius_registry_v1_registry` resource and data source with the official Terraform provider (by ID and by name) | ✅ e2e: `test/terraform/registry` |
| Docker data plane: `docker push/pull` with Nebius IAM tokens, images stored in Harbor, mocker run as a container (the documented setup, also on Docker Desktop) | ✅ e2e: `make e2e-registry` |
| Short-lived token login: `nebius iam get-access-token \| docker login <fqdn> --username iam --password-stdin` | ✅ e2e: `TestStaticTokenLogin` |
| Credential helper: `nebius registry configure-helper`, then push/pull with no login, on `cr.<region>.nebius.cloud` | ✅ e2e: `TestCredentialHelper` ([setup](#credential-helper)) |
| TLS on the gRPC edge and the docker endpoint (`NEBIUS_MOCKER_TLS_CERT_FILE` / `_KEY_FILE`) | ✅ |
| `nebius.iam.v1.ProfileService.Get` (the CLI calls it before printing a token; served by the IAM module) | ✅ |
| Authorization of RPCs and of docker pull/push under `NEBIUS_MOCKER_AUTHZ` | ✅ |
| `ListRegistries` / `ListArtifacts` `filter` | Rejected with `INVALID_ARGUMENT` on field `filter`: no filter syntax is documented upstream (the Compute proto marks the same field "not implemented, filtering could be done on client side") |
| Quota `registry.count` (registries per project and tenant, per region) enforced on Create ([quotas.md](quotas.md)) | ✅ contract: `test/contract/quotas_areas_test.go` |
| Quota on registry storage size | Not yet |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312, Harbor v2.16.0.

Tests: domain unit tests with fakes (`services/registry/domain`), DAL tests on the memory store
(`services/registry/dal`), Harbor client and docker front tests over `httptest` (`services/registry/backend/harbor`,
`services/registry/docker`), gosdk contract tests against the in-memory backend (`test/contract/registry_test.go`,
`tls_test.go`), Terraform CRUD (`make e2e`), and the user workflows with the mocker image, Harbor, the `nebius` CLI
and docker (`make e2e-registry`). The contract tests seed images into the in-memory backend to check API semantics
only; image upload is proven by `make e2e-registry`, through `docker push`.

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
  For the credential helper set it to `cr.<region>.nebius.cloud` ([below](#credential-helper)).
  `status.images_count` (and the deprecated `spec.images_count`) is read live from the backend.
- **Artifacts** are read live from Harbor (`/api/v2.0/projects/{p}/repositories/{r}/artifacts?with_tag=true`):
  one artifact per manifest. `name` is the image (repository) name inside the registry, `tags` are its tags, and
  `type` is `MANIFEST` or `MANIFEST_LIST`. Artifact NIDs are assigned on first sight and kept in a small index
  (`registry, repository, digest → artifact-…`), so IDs stay stable across listings. `Delete` removes the manifest from
  Harbor.
- **Delete** of a registry that still holds images → `FAILED_PRECONDITION`, unless `force=true` (the real behavior is
  an open question). Deleting removes every Harbor repository and then the project.
- **Docker front** (`services/registry/docker`, `NEBIUS_MOCKER_REGISTRY_LISTEN`, default `:4590`; started only with
  the Harbor backend; HTTPS when `NEBIUS_MOCKER_TLS_*` is set) serves `/v2/`:
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
| `NEBIUS_MOCKER_TLS_CERT_FILE` / `_KEY_FILE` | — (plaintext) | PEM certificate and key: the gRPC edge and the docker endpoint serve TLS |

### Running it (the documented setup)

Run NebiusMocker **as a container** next to Harbor. That is the setup the tests use, and it is the one that works with
an unmodified Docker on every platform. Docker Desktop's daemon runs in a VM: it can reach **published container
ports** on `127.0.0.1`, but not a process listening on the host's loopback (`make run`). On a native Linux daemon both
work.

```bash
deploy/harbor/harbor.sh up /tmp/harbor && . /tmp/harbor/env      # Harbor v2.16.0 (Docker), see below
make docker-image                                                 # builds nebius-mocker:dev (no base image pulled)
docker run -d --name nebius-mocker --network "$HARBOR_NETWORK" \
  -p 127.0.0.1:4566:4566 -p 127.0.0.1:4590:4590 \
  -e NEBIUS_MOCKER_REGISTRY_BACKEND=harbor -e NEBIUS_MOCKER_REGISTRY_HARBOR_URL="$HARBOR_INTERNAL_URL" \
  nebius-mocker:dev
```

If you publish the docker endpoint on another host port, set `NEBIUS_MOCKER_REGISTRY_LISTEN=:<port>` and publish
`<port>:<port>`, so that `status.registry_fqdn` (`127.0.0.1:<port>`) matches what docker dials.

The CLI dials `<service>.<endpoint>` (for example `registry.localhost:4566`), so the profile endpoint must be
`localhost:<port>`, not an IP address. Plaintext needs `--insecure` on every command.

```bash
echo nmock-dev-editor > ~/.nebius-mocker-token                    # a built-in dev token
nebius profile create mocker --endpoint localhost:4566 --token-file ~/.nebius-mocker-token
nebius registry create --name app --insecure                      # status.registry_fqdn: 127.0.0.1:4590
```

#### Short-lived token login

This is Nebius's documented "CI/CD: short-lived access token" flow, unchanged:

```bash
nebius iam get-access-token --insecure | docker login 127.0.0.1:4590 --username iam --password-stdin
docker push 127.0.0.1:4590/<registry ID without "registry-">/app:v1
```

#### Credential helper

`nebius registry configure-helper` registers `docker-credential-nebius` for the fixed hosts
`cr.<region>.nebius.cloud`. The helper runs `nebius registry docker-credential get` without `--insecure`, and the
profile cannot store that flag. So the helper flow needs what any local stand-in for those hosts needs. The `nebius`
tools are not modified; only the machine is configured:

1. **TLS from a CA the machine trusts.** Start the mocker with `NEBIUS_MOCKER_TLS_CERT_FILE` / `_KEY_FILE`. The
   certificate's SANs must cover `cr.<region>.nebius.cloud` and the API names the CLI dials: `api.nebius.cloud`,
   `cpl.iam.api.nebius.cloud` and `registry.api.nebius.cloud`. Set `NEBIUS_MOCKER_REGISTRY_LISTEN=:443` and
   `NEBIUS_MOCKER_REGISTRY_FQDN=cr.<region>.nebius.cloud`.
   - The CLI needs the CA in the system trust store.
   - dockerd needs it in `/etc/docker/certs.d/cr.<region>.nebius.cloud/ca.crt`.
2. **Those names resolve to the mocker.** For example, add network aliases on the mocker container for clients on the
   same Docker network, or add `/etc/hosts` entries.
3. A profile with `--endpoint api.nebius.cloud:4566` and a token file. Then the documented steps, verbatim:
   `nebius registry configure-helper`, then `docker push`/`docker pull cr.<region>.nebius.cloud/<id>/app:v1`.

`TestCredentialHelper` does exactly this on a fresh official `docker:dind` container, which plays the Linux machine:
- it installs `bash`, `curl` and `coreutils`, because the official installer needs GNU `mktemp`;
- it installs the CA as above;
- it installs the CLI with the documented `curl … install.sh | bash`;
- DNS comes from network aliases on the mocker container.

On a macOS host the same setup means trusting the CA in the Keychain (Docker Desktop imports Keychain CAs) and
mapping the names in `/etc/hosts`. Those are system-wide changes, so the suite does not make them on the developer's
machine.

### Harbor

`deploy/harbor/harbor.sh` generates the configuration with the official `goharbor/prepare:v2.16.0` image (the first
release with arm64 images). It then starts Harbor under a unique compose project with an override that drops the fixed
container names, the syslog logging driver (fixed host port 1514) and the fixed proxy port. The proxy binds
`127.0.0.1:<random>`. The script waits for `/api/v2.0/health` plus an authenticated `/v2/`, and writes
`<workdir>/env`:
- `HARBOR_URL`, the address from the host;
- `HARBOR_NETWORK` and `HARBOR_INTERNAL_URL`, for the mocker container;
- the admin credentials.

`down` removes the containers, networks, volumes and data dir. `make e2e-registry` builds the image, starts Harbor and
removes everything through a trap, even when tests fail.

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
