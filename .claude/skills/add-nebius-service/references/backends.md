# Backends: build the Nebius logic, adopt the heavy lifting

NebiusMocker exists so people can develop against Nebius APIs locally. It is not a cloud. Every hour spent
reimplementing a registry, a storage engine or a database is an hour not spent on Nebius fidelity, and it adds
maintenance burden forever.

## Decision rule

| Question | Yes | No |
|---|---|---|
| Is it Nebius-specific behavior (API shape, validation, IDs, lifecycle, operations, errors, authz, quotas)? | **Build it**, on `pkg/*` | ↓ |
| Is it a data plane that clients talk to directly (bytes, OCI, Kubernetes API, SQL, DNS)? | **Adopt** an open-source system | ↓ |
| Is it a small, well-specified algorithm (CIDR allocation, SigV4 verification, glob matching)? | Build it, or use a focused Go library | — |

## Choosing an open-source backend

All of the following must hold:
- **Popular and maintained**: a widely used project with recent releases. Prefer CNCF projects or the de-facto standard.
- **Permissive license** compatible with Apache-2.0 (Apache-2.0, MIT, BSD). Run it as a separate container; never vendor
  its code.
- **Official multi-arch Docker image** (`linux/amd64` **and** `linux/arm64`; contributors use Apple Silicon). Pin an
  exact tag and verify that it actually runs on arm64.
- **Scriptable**: an admin API or CLI, so the mocker can create tenants, projects, buckets or users on demand.
- **Starts in under a minute** with no manual steps.

## Known choices

| Area | Backend | Image / notes |
|---|---|---|
| Container Registry | [Harbor](https://github.com/goharbor/harbor) | `goharbor/*` v2.16.0+ (arm64 images start at 2.16). Generate the config with `goharbor/prepare`. The mocker maps registry → Harbor project and fronts docker auth with Nebius IAM tokens |
| Object Storage (and blobs for other services) | [RustFS](https://github.com/rustfs/rustfs) | `rustfs/rustfs`, S3-compatible. Reach it through the shared `pkg/objstore` port. The mocker's S3 front verifies Nebius access keys (SigV4) and forwards with admin credentials |
| Managed Kubernetes (mk8s) | [k3s](https://github.com/k3s-io/k3s) | `rancher/k3s`: one container per cluster, like LocalStack's EKS |
| Managed PostgreSQL | [PostgreSQL](https://www.postgresql.org/) | Official `postgres` image, one container per cluster |
| MLflow | [MLflow](https://github.com/mlflow/mlflow) | Official image, with artifacts in RustFS |
| DNS (authoritative answers) | [CoreDNS](https://github.com/coredns/coredns), or the `miekg/dns` library | Only if a scenario needs real resolution |

When adding a backend that isn't listed, add a row here in the same change, with the reasoning.

## How to wrap a backend

```text
services/<area>/domain/ports.go        # the port: only what the domain needs (CreateProject, PutObject, …)
services/<area>/backend/<name>/        # adapter over the backend's API (may use net/http as a client)
pkg/<shared>/                          # instead, when two areas need the same backend (e.g. pkg/objstore)
pkg/<shared>/memory                    # in-memory implementation for unit/contract tests (no Docker)
test/internal/<name>/                  # docker lifecycle helper for e2e: start, wait healthy, unique names, cleanup
```

- The backend is never the source of truth for Nebius metadata. Resources live in `pkg/store` through the DAL, and the
  backend holds data-plane state only. Create backend objects in the operation's `Work` step, and make it idempotent.
- Translate backend errors to `pkg/errs` kinds inside the adapter.
- Map the backend's auth to Nebius identities at the mocker's front door: validate Nebius tokens or keys, then call the
  backend with admin credentials. Never ask users for backend credentials.
- Configuration (`NEBIUS_MOCKER_<AREA>_*`) selects `memory` (default) or the real backend, plus its address. The default
  must work with no Docker.
