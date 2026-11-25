# 11 — Configuration

Configuration precedence: **flags > environment > config file (`/etc/nebius-mocker/config.yaml` or `--config`) > profile defaults**.
Every key has an environment form: `NEBIUS_MOCKER_` + the upper-snake path.

## Core settings

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_PROFILE` | `dev` | `dev` \| `ci` \| `strict` (see [06](06-iam-and-authorization.md#profiles-nebius_mocker_profile)) |
| `NEBIUS_MOCKER_SERVICES` | all (all-in-one) | Comma list of areas: `iam,vpc,compute,mk8s,storage,registry,mysterybox,kms,dns,quotas,billing,capacity,msp,ai,applications,audit,logging,maintenance,tunnel` |
| `NEBIUS_MOCKER_LAZY` | `0` | Start modules on first request |
| `NEBIUS_MOCKER_LISTEN` | `:4566` | Edge listener |
| `NEBIUS_MOCKER_ADMIN_LISTEN` | `:4567` | Admin API |
| `NEBIUS_MOCKER_TLS` | `off` | `off` (h2c) \| `self-signed` \| `wildcard` (mint a CA + `*.api.nebius.cloud` cert; CA exported at `/_mocker/tls/ca.pem`) \| `files` (`_CERT_FILE`/`_KEY_FILE`) |
| `NEBIUS_MOCKER_PER_SERVICE_PORTS` | `0` | Also open one port per service |
| `NEBIUS_MOCKER_AUTHN` | profile | `optional` \| `required` |
| `NEBIUS_MOCKER_AUTHZ` | profile | `off` \| `audit` \| `enforce` |
| `NEBIUS_MOCKER_DEV_TOKENS` | `nmock-dev-admin=tenantuseraccount-e00mock…` | Static tokens → principals |
| `NEBIUS_MOCKER_TOKEN_SECRET` | random per boot | HMAC secret. Set it explicitly for split deployments and persistence |
| `NEBIUS_MOCKER_TOKEN_TTL` | `12h` | Access-token lifetime |
| `NEBIUS_MOCKER_TIMING` | profile | `instant` \| `fast` \| `realistic` \| path to a durations YAML |
| `NEBIUS_MOCKER_IDS` | `random` | `random` \| `sequential` \| `seeded:<int>` |
| `NEBIUS_MOCKER_REGIONS` | `eu-north1=e00,eu-west1=e01,us-central1=u00,me-west1=m00` | Region → NID routing code (configurable; the defaults are placeholders) |
| `NEBIUS_MOCKER_PERSISTENCE` | `0` | Persist state across restarts |
| `NEBIUS_MOCKER_DATA_DIR` | `/var/lib/nebius-mocker` | State and snapshot location |
| `NEBIUS_MOCKER_OP_CONFLICT` | `reject` | `reject` \| `abort-previous` |
| `NEBIUS_MOCKER_OP_RETENTION` | `168h` | Finished-operation retention (mock clock) |
| `NEBIUS_MOCKER_RATE_LIMIT` | off | For example `compute=50/s,*=200/s` |
| `NEBIUS_MOCKER_FAULTS_FILE` | — | YAML fault rules loaded at boot |
| `NEBIUS_MOCKER_JOURNAL` | `0` | Record all RPCs to `DATA_DIR/journal.jsonl` (secrets redacted) |
| `NEBIUS_MOCKER_LOG_LEVEL` / `_LOG_FORMAT` | `info` / `text` | `debug` logs every request with principal, permission decision and timing |
| `NEBIUS_MOCKER_PEERS` | — | `area=host:port,...` remote modules for split deployments |
| `NEBIUS_MOCKER_GATEWAY` | — | Run as a routing gateway to these peers |
| `NEBIUS_MOCKER_IAM` | `auto` | `auto` \| `embedded` (IAM stub for single-service images) \| `peer` |

Module-specific keys are named `NEBIUS_MOCKER_<AREA>_<KEY>`, for example `NEBIUS_MOCKER_MK8S_BACKEND=k3s`.
They are documented in each service doc.

## Seed data

Seed files are YAML documents, each holding a list of resources expressed as **the same JSON/YAML shape as the proto
messages** (protojson field names). They are applied in order at boot after the module defaults:

```yaml
# /etc/nebius-mocker/init/boot.d/10-tenant.yaml
apiVersion: nebius-mocker/v1
kind: Seed
items:
  - type: nebius.iam.v1.Container          # tenant
    as: tenant
    value: { metadata: { id: tenant-e00acme00000000000001, name: acme } }
  - type: nebius.iam.v1.Container          # project
    parentRef: tenant
    value:
      metadata: { id: project-e00acme00000000000001, name: prod }
      spec: { region: eu-north1 }
  - type: nebius.iam.v1.ServiceAccount
    value: { metadata: { parent_id: project-e00acme00000000000001, name: terraform } }
  - type: nebius-mocker.quota            # mocker-specific helpers
    value: { scope: tenant-e00acme00000000000001, name: compute.gpu.h100, limit: 32 }
```

Seeded resources skip operations: they are written directly in their terminal state.

Default catalogs that each module ships (all overridable):

| Catalog | Module | Examples |
|---|---|---|
| Platforms and presets | compute | `cpu-e2`, `cpu-d3`, `gpu-h100-sxm`, `gpu-h200-sxm`, `gpu-b200-sxm`, `gpu-l40s-a`, … with presets such as `2vcpu-8gb`, `8gpu-128vcpu-1600gb` |
| Public images and families | compute | `ubuntu22.04-driverless`, `ubuntu22.04-cuda12`, `ubuntu24.04-driverless`, `mk8s-worker-node-v-1-31-ubuntu22.04`, … |
| Kubernetes versions | mk8s | `1.30`, `1.31`, `1.32` |
| Roles | iam | `admin`, `editor`, `viewer`, narrower roles |
| Quota defaults | quotas | see [09](09-quotas-limits-faults.md) |
| Regions | core | `eu-north1`, `eu-west1`, `us-central1`, `me-west1` |

The catalog values are **illustrative defaults** to be checked against live `PlatformService.List`, `ImageService.List`
and similar calls during parity recording ([12](12-test-harness.md#4-parity-tests-against-real-nebius)). The goal is for
the defaults to match production names.

## Init hooks

These follow LocalStack's `init/{boot,start,ready,shutdown}.d`:

| Directory | When | Accepted files |
|---|---|---|
| `boot.d` | Before modules start | `*.yaml` seeds |
| `ready.d` | After all modules are running | `*.yaml` seeds, `*.sh` (run with `NEBIUS_IAM_TOKEN` and resolver env exported, so the `nebius` CLI just works) |
| `shutdown.d` | On SIGTERM | `*.sh` |
