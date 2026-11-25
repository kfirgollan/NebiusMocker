# NebiusMocker

A local emulator of the [Nebius AI Cloud](https://nebius.com) API, in the spirit of LocalStack. It serves the same
gRPC services as the real cloud, so the **official** Nebius Terraform provider, CLI and SDKs run against your laptop or
CI with no Nebius account.

> Status: early. **IAM groups, memberships and access permits, with enforced authorization,** work end to end with the
> official Terraform provider. **Compute images** (public catalog per region, custom images imported from Object
> Storage) work with the provider's image data source, the Go SDK and the `nebius` CLI. See
> [docs/services/iam.md](docs/services/iam.md#implementation-status),
> [docs/services/compute.md](docs/services/compute.md#implementation-status) and the [roadmap](docs/architecture/13-milestones.md).
> **Container Registry** works with Terraform, the Go SDK, the `nebius` CLI and `docker push/pull` (images stored in
> [Harbor](https://goharbor.io)); see [docs/services/registry.md](docs/services/registry.md#implementation-status).
> **KMS** symmetric and asymmetric keys work with real crypto (AES-256-GCM, ECDSA, RSA-OAEP), plus rotation and
> delayed deletion, through Terraform, the Go SDK and the `nebius kms` CLI; see
> [docs/services/kms.md](docs/services/kms.md#implementation-status).

## Quick start

```bash
make run    # all-in-one server on :4566 (or: go run ./cmd/nebius-mocker)
```

Point the official provider at it. Nothing else changes in your Terraform:

```hcl
terraform {
  required_providers {
    nebius = { source = "nebius/nebius", version = "= 0.6.91" }
  }
}

provider "nebius" {
  resolvers       = { "*" = "127.0.0.1:4566" }      # every Nebius service → the mocker
  address_options = { "*" = { insecure = true } }   # plaintext gRPC
  token           = "nmock-dev-admin"               # built-in dev token
}

resource "nebius_iam_v1_group" "engineers" {
  parent_id = "tenant-e00mock0000000000default"     # seeded default tenant
  name      = "engineers"
}

data "nebius_compute_v1_image" "ubuntu" {           # seeded public image catalog
  parent_id = "project-e00public-images"
  name      = "ubuntu24.04-driverless-20251015"
}
```

Compute images work with the `nebius` CLI too (profile endpoint `localhost:4566`, `--insecure`), with payloads in
RustFS when `NEBIUS_MOCKER_OBJSTORE=rustfs`
([details](docs/services/compute.md#implementation-status)):

```bash
nebius compute image list-public --region eu-north1
nebius compute image get-latest-by-family --image-family ubuntu24.04-driverless
nebius compute image create --parent-id project-e00mock000000000default --name my-image \
  --source-storage-bucket-name my-bucket --source-storage-object-name disk.qcow2
```

Common settings (all `NEBIUS_MOCKER_*` env vars, see [configuration](docs/architecture/11-configuration.md)):

| Env | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_LISTEN` | `:4566` | gRPC edge address |
| `NEBIUS_MOCKER_AUTHN` | `optional` | `required` rejects requests without a valid token |
| `NEBIUS_MOCKER_AUTHZ` | `audit` | `enforce` blocks requests the caller's roles do not allow; `audit` only logs them; `off` skips checks |
| `NEBIUS_MOCKER_DEV_TOKENS` | `nmock-dev-admin`, `nmock-dev-editor`, `nmock-dev-viewer` | `token=principal,…` static tokens |
| `NEBIUS_MOCKER_OP_DELAY` | `0` | Keep operations running this long (exercise client polling) |
| `NEBIUS_MOCKER_IDS` | `random` | `sequential` for predictable IDs |
| `NEBIUS_MOCKER_REGIONS` | `eu-north1=e00,eu-west1=e01,us-central1=u00,me-west1=m00` | Regions (and NID routing codes) with a public image catalog |
| `NEBIUS_MOCKER_COMPUTE_IMAGES_FILE` | built-in | Replace the public image catalog ([format](docs/services/compute.md#catalogs-seed)) |
| `NEBIUS_MOCKER_REGISTRY_BACKEND` | `memory` | `harbor` stores images in Harbor and serves docker on `NEBIUS_MOCKER_REGISTRY_LISTEN` (`:4590`) |

### Container Registry with docker

```bash
test/registry/harbor/harbor.sh up /tmp/harbor && . /tmp/harbor/env     # throwaway Harbor (Docker)
NEBIUS_MOCKER_REGISTRY_BACKEND=harbor NEBIUS_MOCKER_REGISTRY_HARBOR_URL=$NEBIUS_MOCKER_E2E_HARBOR_URL make run

# the documented Nebius login, with a profile whose endpoint is localhost:4566 and a dev token file
nebius iam get-access-token --insecure | docker login 127.0.0.1:4590 --username iam --password-stdin
docker push 127.0.0.1:4590/<registry ID without "registry-">/app:v1
```

Details, including Docker Desktop, are in [docs/services/registry.md](docs/services/registry.md#local-workflow).

## Development

```bash
make hooks   # once: install the pre-commit gate
make check   # lint (layering rules included) + unit/contract/architecture tests with -race
make e2e     # real terraform + official nebius provider against the mock
make e2e-images  # compute images on RustFS with the Go SDK and the nebius CLI (Docker)
make e2e-registry  # Harbor + nebius CLI + docker push/pull (needs Docker; Harbor is started and removed)
```

Read [docs/architecture](docs/architecture/README.md) first. The Go rules for this repo live in `.claude/skills/`
(`go-architecture`, `go-style`, `go-errors`, `go-concurrency`, `go-patterns`, `go-testing`, `go-lint`) and are enforced
by `.golangci.yml` and `test/arch`.

## License

Apache-2.0. See [LICENSE](LICENSE).
