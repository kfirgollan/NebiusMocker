# NebiusMocker

A local emulator of the [Nebius AI Cloud](https://nebius.com) API, in the spirit of LocalStack. It serves the same
gRPC services as the real cloud, so the **official** Nebius Terraform provider, CLI and SDKs run against your laptop or
CI with no Nebius account.

> [!IMPORTANT]
> **This is not an official Nebius project.** It is not affiliated with, endorsed by or supported by Nebius. I develop
> it as a side project to support my other work. I hope it benefits the community and helps others, but don't assume
> it is production grade. Its coverage and behavior will keep changing, driven by how I use Nebius. Issues and
> contributions are welcome.

> Status: early. **IAM groups, memberships, access permits, service accounts and access keys, with enforced
> authorization,** and **Object Storage buckets and objects** (S3 data plane backed by [RustFS](https://github.com/rustfs/rustfs))
> work end to end with the official Terraform provider and SDKs. **Compute images** (public catalog per region, custom
> images imported from Object Storage) work with the provider's image data source, the Go SDK and the `nebius` CLI. See
> [docs/services/iam.md](docs/services/iam.md#implementation-status),
> [docs/services/storage.md](docs/services/storage.md#implementation-status),
> [docs/services/compute.md](docs/services/compute.md#implementation-status) and the [roadmap](docs/architecture/13-milestones.md).
> **Container Registry** works with Terraform, the Go SDK, the `nebius` CLI and `docker push/pull` (images stored in
> [Harbor](https://goharbor.io)); see [docs/services/registry.md](docs/services/registry.md#implementation-status).
> **KMS** symmetric and asymmetric keys work with real crypto (AES-256-GCM, ECDSA, RSA-OAEP), plus rotation and
> delayed deletion, through Terraform, the Go SDK and the `nebius kms` CLI; see
> [docs/services/kms.md](docs/services/kms.md#implementation-status).
> **MysteryBox (SecretStash) secrets, versions and payloads** work with the official Terraform provider, CLI and Go
> SDK: see [docs/services/mysterybox.md](docs/services/mysterybox.md#implementation-status).
> **VPC networking** (pools, networks, subnets, private and public allocations, route tables and routes, security
> groups and rules, with real IPAM) works with Terraform, the Go SDK and the `nebius vpc` CLI; see
> [docs/services/vpc.md](docs/services/vpc.md#implementation-status).
> **Serverless AI endpoints** keep a real inventory (validation, lifecycle, start/stop/restart, auth token or
> MysteryBox secret) through Terraform, the Go SDK and the `nebius ai endpoint` CLI. No container runs: each endpoint
> URL points at an HTTPS stub on `:4600` (local CA, trust it with `curl --cacert`) that answers
> `200 "endpoint was called"` to authorized calls; see
> [docs/services/ai.md](docs/services/ai.md#implementation-status).
> **Audit logs**: every mutating call to any service (plus authorization denials) is recorded as a redacted audit
> event, searchable with the documented filter language and exportable to Object Storage through the Go SDK and
> `nebius audit`; see [docs/services/audit.md](docs/services/audit.md#implementation-status).

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

### Object Storage (S3)

Buckets work out of the box. For object reads and writes, run [RustFS](https://github.com/rustfs/rustfs) and point the
mocker at it; the mocker's S3 front (`:4580`) checks Nebius access keys and permits and relays to RustFS:

```bash
docker run -d --name rustfs -p 127.0.0.1:9000:9000 rustfs/rustfs:1.0.6
NEBIUS_MOCKER_OBJSTORE=rustfs make run
```

Create a service account with a permit (or in the `editors` group) and a `nebius_iam_v2_access_key`, then use any S3
client with that key, region `eu-north1` and endpoint `http://127.0.0.1:4580` (path style), as the Nebius quick start
does: `aws configure set endpoint_url http://127.0.0.1:4580`, `aws s3 mb s3://my-bucket`, `aws s3 cp`, `aws s3 ls`.
Buckets can also be created with `nebius storage bucket create` or `nebius_storage_v1_bucket`. CORS, bucket policies
(incl. anonymous reads), lifecycle rules, size limits and storage classes take effect; see the storage doc for what is
rejected. Terraform objects use the official `hashicorp/aws`
provider; see [docs/services/storage.md](docs/services/storage.md#terraform).

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
deploy/harbor/harbor.sh up /tmp/harbor && . /tmp/harbor/env      # throwaway Harbor (Docker)
make docker-image                                                 # nebius-mocker:dev
docker run -d --name nebius-mocker --network "$HARBOR_NETWORK" -p 127.0.0.1:4566:4566 -p 127.0.0.1:4590:4590 \
  -e NEBIUS_MOCKER_REGISTRY_BACKEND=harbor -e NEBIUS_MOCKER_REGISTRY_HARBOR_URL="$HARBOR_INTERNAL_URL" nebius-mocker:dev

# the documented Nebius login, with a profile whose endpoint is localhost:4566 and a dev token file
nebius iam get-access-token --insecure | docker login 127.0.0.1:4590 --username iam --password-stdin
docker push 127.0.0.1:4590/<registry ID without "registry-">/app:v1
```

Run the mocker as a container for docker: Docker Desktop's daemon can reach published container ports but not a host
process. The credential-helper flow (`nebius registry configure-helper`) and the details are in
[docs/services/registry.md](docs/services/registry.md#running-it-the-documented-setup).
| `NEBIUS_MOCKER_OBJSTORE` | `memory` | `rustfs` stores objects in RustFS (`NEBIUS_MOCKER_OBJSTORE_ENDPOINT`, default `http://127.0.0.1:9000`) |
| `NEBIUS_MOCKER_STORAGE_S3_LISTEN` | `:4580` | Object Storage S3 front (`off` disables it) |
| `NEBIUS_MOCKER_AI_LISTEN` | `127.0.0.1:4600` | HTTPS stub that AI endpoint URLs point at (`NEBIUS_MOCKER_AI_PUBLIC_URL` overrides the advertised base URL; TCP ports use `NEBIUS_MOCKER_AI_TCP_LISTEN`, `:4601`) |
| `NEBIUS_MOCKER_AI_TLS_DIR` | `<user config dir>/nebius-mocker/ca` | Local CA of the AI data plane; trust `<dir>/ca.pem` (`curl --cacert`) |

## Development

```bash
make hooks   # once: install the pre-commit gate
make check   # lint (layering rules included) + unit/contract/architecture tests with -race
make e2e     # real terraform + official nebius provider against the mock
make e2e-images  # compute images on RustFS with the Go SDK and the nebius CLI (Docker)
make e2e-registry  # Harbor + nebius CLI + docker push/pull (needs Docker; Harbor is started and removed)
make e2e-storage  # Object Storage with RustFS in Docker: Terraform (nebius + aws providers), gosdk + AWS SDK, nebius + aws CLIs
```

Read [docs/architecture](docs/architecture/README.md) first. The Go rules for this repo live in `.claude/skills/`
(`go-architecture`, `go-style`, `go-errors`, `go-concurrency`, `go-patterns`, `go-testing`, `go-lint`) and are enforced
by `.golangci.yml` and `test/arch`.

## License

Apache-2.0. See [LICENSE](LICENSE).
