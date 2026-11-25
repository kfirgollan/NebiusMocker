# 10 — Deployment topologies

## Images

A multi-target `deploy/docker/Dockerfile` builds static binaries (`CGO_ENABLED=0`) on a distroless base.

| Image | Contents | Default `SERVICES` |
|---|---|---|
| `ghcr.io/kfirgollan/nebius-mocker:<ver>` | all-in-one binary | all |
| `ghcr.io/kfirgollan/nebius-mocker-<area>:<ver>` | single-module binary, for example `-compute` | that area (plus `common` ops) |

Tags follow `vX.Y.Z`, plus `api-<nebius/api short sha>` so users can match an API revision. Images are multi-arch
(`linux/amd64`, `linux/arm64`).

## Ports

| Port | Purpose |
|---|---|
| `4566` | Edge: gRPC (h2c, or TLS when enabled) **and** HTTP/1.1 for `/oauth2/token/exchange` and the optional S3/OCI data planes |
| `4567` | Admin API (`/_mocker/*`), health, metrics (`/metrics`, Prometheus) |
| `4570+` | Optional per-service gRPC listeners (`NEBIUS_MOCKER_PER_SERVICE_PORTS=1`), assigned deterministically in [services/README](../services/README.md#endpoint-map) order |

## Topology A: single service

Use this to test one service's client code in isolation.

```bash
docker run --rm -p 4566:4566 -e NEBIUS_MOCKER_PROFILE=dev ghcr.io/kfirgollan/nebius-mocker-vpc
```

The `vpc` image still needs to validate tokens and check that `parent_id` projects exist. The single-service binary
handles this with an **embedded IAM stub** (`NEBIUS_MOCKER_IAM=embedded`, the default when `iam` is not among the
local modules and no `NEBIUS_MOCKER_PEER_IAM` is set). The stub seeds the default tenant and project and accepts dev tokens.
The container is fully standalone, and projects can be created via the admin API.

## Topology B: all-in-one

This is the recommended default and the Terraform M1 target.

```yaml
# deploy/compose/all-in-one.yaml
services:
  nebius:
    image: ghcr.io/kfirgollan/nebius-mocker:latest
    ports: ["4566:4566", "4567:4567"]
    environment:
      NEBIUS_MOCKER_PROFILE: ci
      NEBIUS_MOCKER_PERSISTENCE: "0"
    volumes:
      - ./init:/etc/nebius-mocker/init:ro
      - /var/run/docker.sock:/var/run/docker.sock   # only if mk8s k3s backend is enabled
```

## Topology C: multiple containers

Use this to test service boundaries and to scale heavy modules (mk8s with k3s) separately.

```yaml
# deploy/compose/split.yaml
services:
  iam:
    image: ghcr.io/kfirgollan/nebius-mocker-iam
    environment: { NEBIUS_MOCKER_TOKEN_SECRET: "${SECRET}" }
  vpc:
    image: ghcr.io/kfirgollan/nebius-mocker-vpc
    environment:
      NEBIUS_MOCKER_PEERS: "iam=iam:4566"
      NEBIUS_MOCKER_TOKEN_SECRET: "${SECRET}"
  compute:
    image: ghcr.io/kfirgollan/nebius-mocker-compute
    environment:
      NEBIUS_MOCKER_PEERS: "iam=iam:4566,vpc=vpc:4566"
      NEBIUS_MOCKER_TOKEN_SECRET: "${SECRET}"
  gateway:
    image: ghcr.io/kfirgollan/nebius-mocker
    environment:
      NEBIUS_MOCKER_SERVICES: ""                 # no local modules
      NEBIUS_MOCKER_GATEWAY: "iam=iam:4566,vpc=vpc:4566,compute=compute:4566"
    ports: ["4566:4566"]
```

Clients can talk to each container directly, using per-service resolvers in the provider
(`resolvers = { "nebius.vpc.*" = "localhost:4601", … }`). They can also go through the **gateway mode** of the all-in-one
binary. Gateway mode is a transparent gRPC reverse proxy that routes on the `/package.Service/` prefix, so clients still
need only one address.

### Cross-container dependencies

| Need | Mechanism |
|---|---|
| Token validation | Stateless HMAC tokens with a shared `NEBIUS_MOCKER_TOKEN_SECRET`. Revocation lists are fetched from the IAM peer and cached for 5s |
| Authz decisions | The IAM peer exposes the decision engine through the **public** `AccessPermitService`/`GroupMembershipService` APIs, plus a small internal `nebius.mocker.internal.v1.AuthzService/Check`. The internal service is served only to callers holding the internal token, and is never exposed on the gateway |
| Reference checks (`subnet_id` exists?) | `peer.Directory.Get(id)` → a public `Get` RPC on the owning container, chosen by NID prefix → module → peer address |
| `OperationService` | The gateway routes by operation-ID prefix to the owning container |
| Events (cascade deletes, IP allocation) | Synchronous peer RPCs at the point of need. There is no cross-container event bus in v1 |

## Kubernetes / CI

- A plain `Deployment` + `Service` of the all-in-one image works. A Helm chart (`deploy/helm`) comes later.
- GitHub Actions: a composite action `kfirgollan/NebiusMocker/action@v1` starts the container as a service, waits for
  `/_mocker/health`, and exports `NEBIUS_IAM_TOKEN` and the resolver env var.

## Health and readiness

`GET /_mocker/health` returns JSON with each module's status (`disabled | starting | running | error`), the API revision,
the profile and uptime. Init hooks in `ready.d` run once every enabled module is `running`. After that the endpoint reports
`"ready": true`, the moment CI should start sending traffic.
