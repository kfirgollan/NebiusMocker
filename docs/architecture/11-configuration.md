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
| `NEBIUS_MOCKER_DEV_TOKENS` | `nmock-dev-admin`, `nmock-dev-editor`, `nmock-dev-viewer` → seeded members of `admins`/`editors`/`viewers` | Static tokens → principals (`token=principal,…` replaces the defaults) |
| `NEBIUS_MOCKER_TOKEN_SECRET` | random per boot | HMAC secret of issued access tokens (at least 32 bytes). Set it explicitly for split deployments and persistence |
| `NEBIUS_MOCKER_TOKEN_TTL` | `12h` | Access-token lifetime (`expires_in` of the token exchange) |
| `NEBIUS_MOCKER_IAM_JWT_MAX_TTL` | `1h` | Longest `exp` (from now) accepted on service-account JWTs in the token exchange |
| `NEBIUS_MOCKER_IAM_FAKE_OIDC` | `true` | Serve a fake OIDC issuer at `/_mocker/oidc/<name>` on the edge port, for federated credentials ([iam.md](../services/iam.md#federated-credentials-workload-identity)). `false` removes it |
| `NEBIUS_MOCKER_TIMING` | profile | `instant` \| `fast` \| `realistic` \| path to a durations YAML |
| `NEBIUS_MOCKER_IDS` | `random` | `random` \| `sequential` \| `seeded:<int>` |
| `NEBIUS_MOCKER_REGIONS` | `eu-north1=e00,eu-west1=e01,us-central1=u00,me-west1=m00` | Region → NID routing code (configurable; the defaults are placeholders). **Implemented**: the first region is the default; Compute seeds a public image catalog per region |
| `NEBIUS_MOCKER_IAM_PROJECTS` | — | **Implemented**: `name[:region],…` projects seeded in the default tenant after `default` (`NEBIUS_MOCKER_PROJECT_ID`). IDs are derived from tenant + name (stable across restarts); regions must be in `NEBIUS_MOCKER_REGIONS` |
| `NEBIUS_MOCKER_COMPUTE_FABRICS_FILE` | built-in (`services/compute/domain/fabrics.yaml`) | **Implemented**: InfiniBand fabric catalog of GPU clusters (YAML/JSON, [format](../services/compute.md#gpu-clusters-implemented)); replaces the built-in one |
| `NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE` | built-in (`services/compute/domain/platforms.yaml`) | **Implemented**: platform/preset catalog per region (YAML/JSON, [format](../services/compute.md#catalogs-seed)); replaces the built-in one |
| `NEBIUS_MOCKER_COMPUTE_PUBLIC_IP_RELEASE_AFTER` | `1h` | **Implemented**: how long a `STOPPED` instance keeps a dynamic public IP (`public_ip_address = {}`) before it is released (docs: more than one hour); a positive duration, on the mock clock ([compute](../services/compute.md)) |
| `NEBIUS_MOCKER_PERSISTENCE` | `0` | Persist state across restarts |
| `NEBIUS_MOCKER_DATA_DIR` | `/var/lib/nebius-mocker` | State and snapshot location |
| `NEBIUS_MOCKER_OP_CONFLICT` | `reject` | `reject` \| `abort-previous` |
| `NEBIUS_MOCKER_OP_RETENTION` | `168h` | Finished-operation retention (mock clock) |
| `NEBIUS_MOCKER_RATE_LIMIT` | off | For example `compute=50/s,*=200/s` |
| `NEBIUS_MOCKER_FAULTS_FILE` | — | YAML fault rules loaded at boot |
| `NEBIUS_MOCKER_JOURNAL` | `0` | Record all RPCs to `DATA_DIR/journal.jsonl` (secrets redacted) |
| `NEBIUS_MOCKER_AUDIT_MAX_EVENTS` | `50000` | Capacity of the in-memory audit event log; the oldest events are evicted ([audit](../services/audit.md)) |
| `NEBIUS_MOCKER_AUDIT_READS` | `false` | Also record Get/List calls as audit events (default: mutating calls and authz denials only) |
| `NEBIUS_MOCKER_AUDIT_EXPORT_DURATION` | `0` | How long an audit export stays `RUNNING` before writing its objects; `0` exports inside `Start` |
| `NEBIUS_MOCKER_LOG_LEVEL` / `_LOG_FORMAT` | `info` / `text` | `debug` logs every request with principal, permission decision and timing |
| `NEBIUS_MOCKER_PEERS` | — | `area=host:port,...` remote modules for split deployments |
| `NEBIUS_MOCKER_GATEWAY` | — | Run as a routing gateway to these peers |
| `NEBIUS_MOCKER_IAM` | `auto` | `auto` \| `embedded` (IAM stub for single-service images) \| `peer` |
| `NEBIUS_MOCKER_OBJSTORE` | `memory` | Shared object store (`pkg/objstore`) for modules that keep blobs: `memory` \| `rustfs` |
| `NEBIUS_MOCKER_OBJSTORE_ENDPOINT` / `_ACCESS_KEY` / `_SECRET_KEY` / `_REGION` | `http://127.0.0.1:9000` / `rustfsadmin` / `rustfsadmin` / `us-east-1` | RustFS server used when `NEBIUS_MOCKER_OBJSTORE=rustfs` |

Module-specific keys are named `NEBIUS_MOCKER_<AREA>_<KEY>`, for example `NEBIUS_MOCKER_MK8S_BACKEND=k3s`.
Implemented so far: `NEBIUS_MOCKER_REGISTRY_BACKEND` (`memory`|`harbor`), `_LISTEN` (`:4590`, `off`), `_FQDN`,
`_HARBOR_URL`, `_HARBOR_USERNAME`, `_HARBOR_PASSWORD` (see [registry](../services/registry.md#configuration)).
`NEBIUS_MOCKER_TLS_CERT_FILE` / `NEBIUS_MOCKER_TLS_KEY_FILE` (both or neither) make the gRPC edge and module endpoints
(the registry's docker endpoint) serve TLS with that PEM certificate.
They are documented in each service doc (for example `NEBIUS_MOCKER_STORAGE_*` in
[services/storage.md](../services/storage.md#configuration)).
VPC: `NEBIUS_MOCKER_VPC_DEFAULT_PRIVATE_CIDR` (default `10.0.0.0/13`, the `default-network-pool` of each project) and
`NEBIUS_MOCKER_VPC_PUBLIC_CIDR` (default `203.0.113.0/24`, the `default-public-pool`); see
[services/vpc.md](../services/vpc.md#configuration).
mk8s: `NEBIUS_MOCKER_MK8S_BACKEND` (`mock`, the default, runs no Kubernetes API; `k3s` is not available yet) and
`NEBIUS_MOCKER_MK8S_VERSIONS` (default `1.34,1.35,1.36`, comma-separated `<major>.<minor>`, the newest is the default
for new clusters); see [services/mk8s.md](../services/mk8s.md#configuration).
Applications: `NEBIUS_MOCKER_APPLICATIONS_PRODUCTS` (comma-separated Marketplace product slugs a K8s release may install;
default `nebius/soperator,nebius/nvidia-gpu-operator,nebius/nvidia-network-operator,nebius/kuberay,nebius/jupyterhub,nebius/mlflow`);
see [services/applications.md](../services/applications.md#configuration).
DNS: `NEBIUS_MOCKER_DNS_LISTEN` (default `:4653`, UDP and TCP; `off` disables it) is the address of the DNS responder
that answers from the mock's zones; see [services/dns.md](../services/dns.md#configuration).

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
| Platforms and presets | compute | `cpu-e2`, `cpu-d3`, `gpu-h100-sxm`, `gpu-h200-sxm`, `gpu-b200-sxm`, `gpu-b300-sxm`, `gpu-rtx6000`, `gpu-l40s-a`, `gpu-l40s-d`, … with presets such as `2vcpu-8gb`, `8gpu-128vcpu-1600gb`, per region (`NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE`) |
| Public images and families | compute | `ubuntu22.04-driverless`, `ubuntu22.04-cuda12`, `ubuntu24.04-driverless`, `mk8s-worker-node-v-1-31-ubuntu22.04`, … |
| InfiniBand fabrics of GPU clusters | compute | `fabric-3` (eu-north1, `gpu-h100-sxm`), `fabric-5` (eu-west1, `gpu-h200-sxm`), `us-central1-b` (us-central1, `gpu-b200-sxm`), … from the public docs; **implemented**, replaced with `NEBIUS_MOCKER_COMPUTE_FABRICS_FILE` ([format](../services/compute.md#gpu-clusters-implemented)) |
| Kubernetes versions | mk8s | `1.34`, `1.35`, `1.36` |
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
