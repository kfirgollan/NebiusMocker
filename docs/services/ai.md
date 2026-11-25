# AI Studio — endpoints, jobs, devlabs

> Module: `services/ai` · Image: `nebius-mocker-ai` · `NEBIUS_MOCKER_SERVICES=ai`
> M1 tier: **T2** endpoint inventory (stub data plane) · T0 jobs, devlabs, hardware · Endpoint: `apps.msp.api.nebius.cloud`

## Overview

Container-based serverless AI workloads:

- **Endpoints** (`aiendpoint`): long-running inference containers (`image`, `ports`, `platform`/`preset`/`gpu`, `volumes`
  from S3 or filesystems, `auth_token` or MysteryBox secret, `subnet_id`, `public_ip`). Status has private/public
  endpoints and instances.
- **Jobs** (`aijob`): run-to-completion containers with `restart_attempts` and `timeout`.
- **Devlabs** (`devlab`): interactive development environments.
- **HardwareService**: lists the available hardware configurations.

## Implementation status

| Piece | Status |
|---|---|
| `nebius.ai.v1.EndpointService`: Create (incl. `dry_run`), Get (view `SECRET`), GetByName, List (paged), Delete, Start, Stop, Restart | ✅ T2 inventory · stub execution |
| Spec validation: image, ports, auth (`auth_token` ⊕ `auth_token_mysterybox_secret`, exactly one HTTP port), env vars, volumes/S3, injected files, registry credentials, disk minimums, platform/preset or `gpu`/`cpu` selectors | ✅ |
| Cross-service references (`project`, `vpcsubnet`, `mbsec`, `mbsecver`, `pricingpolicy`) checked through the authz hierarchy; MysteryBox refs pinned through `pkg/peer` `SecretPayloads` | ✅ |
| Lifecycle state machine as operations, synthetic instance, `status.private_endpoints` / `public_endpoints` | ✅ |
| Stub data plane: HTTPS on `:4600` (`200` / `401` / `503` / `404`) with a mocker-generated local CA; TLS stub for TCP ports on `:4601` | ✅ |
| Create checks the caller's `mysterybox.payload.read` on every referenced MysteryBox secret | ✅ |
| Authorization rules for every RPC (`ai.endpoints.get/list/create/delete/start/stop/restart`, plus `getSensitiveData` for view `SECRET`); editors hold start/stop/restart, viewers only get/list | ✅ |
| `nebius_ai_v1_endpoint` resource and data source with the official Terraform provider, incl. `auth_token_mysterybox_secret` backed by a real `nebius_mysterybox_v1_secret` and `subnet_id` from a real `nebius_vpc_v1_subnet` | ✅ e2e: `test/terraform/ai` |
| `nebius ai endpoint create/get-by-name/get/list/stop/start/restart/delete` + `curl --cacert` (docs quickstart, unmodified `startswith("https://")` filter) | ✅ e2e: `test/terraform/ai` |
| Not yet: `JobService`, `DevlabService`, `HardwareService`, `applications.v1alpha1.VmAppTemplateService`, `nebius ai endpoint logs`; UDP ports are rejected (`InvalidArgument` on `spec.ports[i].protocol`) | UNIMPLEMENTED |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100.

## Emulation design

**The inventory and its semantics are real; execution is a stub.** No container, image pull or VM ever happens.

### Resource and validation

- `aiendpoint` lives in a project (`metadata.parent_id`, normally the seeded `project-e00mock000000000default`).
  Names are unique per project. There is no Update RPC, so every spec or label change plans a **replace** in
  Terraform. `resource_version` stays `1`; lifecycle changes only touch `status`.
- The edge validator enforces the proto rules (required fields, ranges, the CEL rules such as
  `pricing_model_matches_preemptible`, `platform_required`, `hardware_selector_exclusive`). The domain adds the rules
  the protos only state in comments: `auth_token` and `auth_token_mysterybox_secret` are mutually exclusive, and
  authentication needs exactly one HTTP port; env var `value` ⊕ `mysterybox_secret`, unique names; volumes need a
  source and an absolute, unique `container_path`, `s3_config` needs an `s3://` source; injected files need content
  on create; registry password needs a username; disk ≥ 20 GiB (CPU) / 50 GiB (GPU) when set.
- **Platforms.** There is no compute `PlatformService` yet, so `platform`/`preset` are validated against a small
  catalog inside the area (`cpu-d3`, `cpu-e2`, `gpu-h100-sxm`, `gpu-h200-sxm`, `gpu-l40s-a`), overridable with
  `NEBIUS_MOCKER_AI_PLATFORMS` (JSON, see below). `gpu = ["H100:8"]` / `cpu = ["amd:16"]` selectors resolve to a
  platform and preset, which reads return. The catalog will be replaced by compute's once it exists.
- **References** are checked generically, never by importing other areas: an ID must have the right NID prefix;
  if the owning service is enabled (its kind is registered with the authz engine) the resource must exist
  (`NotFound` otherwise); if not, a well-formed ID is accepted and logged at debug.
- **MysteryBox.** When the mysterybox module provides `peer.SecretPayloadsKey`, every secret reference is resolved:
  a ref with only `secret_id` is pinned to the primary version (`version_id` is filled, as the API documents), the
  auth secret must contain `AUTH_TOKEN`, and registry secrets `REGISTRY_USERNAME`/`REGISTRY_PASSWORD`.
  Wiring a secret into an endpoint hands its payload to the workload, so `Create` requires the **caller** to hold
  `mysterybox.payload.read` on every referenced secret (auth token, env vars, S3 credentials, registry credentials).
  It is checked with the authz engine under the configured mode (`enforce` denies with `PERMISSION_DENIED`, `audit`
  logs), before the payload is read. In the default roles only `admin` and `mysterybox.payload-viewer` hold it, so an
  editor needs `mysterybox.payload-viewer` too. The data plane's later reads of the token go in-process through
  `pkg/peer` and are not RPC-authorized: that is the platform reading the secret on the endpoint's behalf.
- Input-only secrets (registry password, injected file content, inline S3 credentials) are stored but returned only
  by `Get` with `view = SECRET`. `auth_token` is returned as sent (Terraform keeps it in state as sensitive).
  `view = SECRET` also needs `ai.endpoints.getSensitiveData` (`resource.aiendpoint.getSensitiveData` upstream);
  in the default roles only `admin` holds it, so editors and viewers get `PERMISSION_DENIED`.

### Lifecycle

| Verb | Allowed from | While the operation runs | Ends in |
|---|---|---|---|
| Create | — | `PROVISIONING` | `RUNNING` |
| Stop | `RUNNING`, `ERROR` | `STOPPING` | `STOPPED` (instances released, URLs kept) |
| Start | `STOPPED`, `ERROR` | `STARTING` | `RUNNING` (new instance) |
| Restart | `RUNNING`, `ERROR` | `STARTING` | `RUNNING` (new instance) |
| Delete | any settled or transitional state | `DELETING` | removed |

Other verbs return `FAILED_PRECONDITION` (bad state); a second operation while one runs returns the operation-conflict
error. With `NEBIUS_MOCKER_OP_DELAY` the transitional states are visible for that long. `status.state_details.code`
is the short state name. `status.instances` holds one **synthetic** instance while running: a `computeinstance-…`
ID that is not a real compute instance, a deterministic private IP, and a public IP when `public_ip` or SSH keys are
set.

### Stub data plane (contract)

The module starts two listeners (4580 is S3 and 4590 the registry). Both use TLS with a certificate signed by a
**local CA** that the mocker creates on first start and keeps in `NEBIUS_MOCKER_AI_TLS_DIR` (default
`<user config dir>/nebius-mocker/ca`, e.g. `~/Library/Application Support/nebius-mocker/ca` on macOS,
`~/.config/nebius-mocker/ca` on Linux). Trust `<dir>/ca.pem` once: `curl --cacert <dir>/ca.pem …`, or add it to your
OS trust store. The startup log prints the URL.

**HTTP ports: HTTPS on `NEBIUS_MOCKER_AI_LISTEN` (default `127.0.0.1:4600`).** Routing is path-based, because the
mock has no wildcard DNS for per-endpoint hosts:

- an endpoint with one HTTP port is served at `https://127.0.0.1:4600/<endpoint-id>`; with several, each at
  `https://127.0.0.1:4600/<endpoint-id>:<container_port>`. These URLs are what `status.public_endpoints` and
  `status.private_endpoints` contain, like the managed `https://` URLs of real endpoints, so the quickstart's
  `select(startswith("https://"))` works unchanged.
- Anything after the first path segment is the application path: `POST <url>/v1/chat/completions` works.

| Request | Response |
|---|---|
| Unknown endpoint ID or port | `404` |
| Endpoint not `RUNNING` (provisioning, stopped, …) | `503` |
| Auth configured, `Authorization: Bearer <token>` missing or wrong | `401` with `WWW-Authenticate: Bearer` |
| Running, token right (or no auth configured: endpoints without auth are open) | `200` |

The `200` body is `{"message":"endpoint was called","endpoint_id":"aiendpoint-…","port":80,"method":"POST","path":"/v1/chat/completions"}`.
The token is `spec.auth_token`, or the `AUTH_TOKEN` entry of the pinned MysteryBox secret version, read at call time
(`503` if that secret cannot be read).

**TCP ports: TLS on `NEBIUS_MOCKER_AI_TCP_LISTEN` (default `127.0.0.1:4601`).** Nebius gives TCP ports a
`tls://<host>:443` address used with a TLS client; the mock gives `tls://<endpoint-id>-p<container_port>.ai.localhost:4601`
(`*.localhost` resolves to loopback; the suffix is `NEBIUS_MOCKER_AI_TCP_DOMAIN`). The listener routes by TLS server
name (SNI), writes one line and closes the connection:

```text
endpoint was called: aiendpoint-… port 5432     (RUNNING)
error 503: endpoint is not running (state STOPPED)
error 404: …                                     (unknown endpoint, or not one of its TCP ports)
```

e.g. `openssl s_client -connect 127.0.0.1:4601 -servername <id>-p5432.ai.localhost -CAfile <dir>/ca.pem -quiet`.
TCP ports carry no endpoint authentication (Nebius allows auth only with a single HTTP port).

**UDP ports are rejected** with `InvalidArgument` on `spec.ports[i].protocol`: Nebius documents no address format
for them, so the mock could not answer the way the cloud does.

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_AI_LISTEN` | `127.0.0.1:4600` | HTTPS stub for HTTP ports |
| `NEBIUS_MOCKER_AI_PUBLIC_URL` | derived from the listener | Base URL written into endpoint statuses (e.g. `https://mocker.local:4600` behind Docker port mapping; its host is added to the certificate) |
| `NEBIUS_MOCKER_AI_TCP_LISTEN` | `127.0.0.1:4601` | TLS stub for TCP ports |
| `NEBIUS_MOCKER_AI_TCP_DOMAIN` | `ai.localhost` | DNS suffix of TCP port addresses |
| `NEBIUS_MOCKER_AI_TLS_DIR` | `<user config dir>/nebius-mocker/ca` | Local CA; clients trust `<dir>/ca.pem` |
| `NEBIUS_MOCKER_AI_PLATFORMS` | built-in catalog | JSON `[{"name":"cpu-d3","cpu":"amd","presets":[{"name":"4vcpu-16gb","vcpus":4}]},{"name":"gpu-h100-sxm","gpu":"H100","presets":[{"name":"1gpu-16vcpu-200gb","gpus":1,"vcpus":16}]}]` |

### Later

- **M4**: jobs and devlabs at T1, with a job lifecycle that runs to `SUCCEEDED` after the configured duration (or
  `FAILED` via fault injection). Hardware is listed from compute's platform catalog.
- **Possible T3**: run endpoint and job images as local containers through the Docker socket (CPU-only).

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_ai_v1_endpoint` | `nebius.ai.v1.EndpointService` | Get, GetByName, Create, Delete + OperationService.Get | T1 |
| Data source | `nebius_ai_v1_endpoint` | `nebius.ai.v1.EndpointService` | Get, GetByName | T1 |

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.ai.v1.DevlabService`

Endpoint: `apps.msp.api.nebius.cloud:443` · Proto: [`nebius/ai/v1/devlab_service.proto`](https://github.com/nebius/api/blob/main/nebius/ai/v1/devlab_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetDevlabRequest` | `Devlab` |  |
| `GetByName` | `common.v1.GetByNameRequest` | `Devlab` |  |
| `List` | `ListDevlabsRequest` | `ListDevlabsResponse` |  |
| `Create` | `CreateDevlabRequest` | `common.v1.Operation` |  |
| `Delete` | `DeleteDevlabRequest` | `common.v1.Operation` |  |
| `Restart` | `RestartDevlabRequest` | `common.v1.Operation` |  |
| `Stop` | `StopDevlabRequest` | `common.v1.Operation` |  |

#### `nebius.ai.v1.EndpointService`

Endpoint: `apps.msp.api.nebius.cloud:443` · Proto: [`nebius/ai/v1/endpoint_service.proto`](https://github.com/nebius/api/blob/main/nebius/ai/v1/endpoint_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetEndpointRequest` | `Endpoint` | ✅ |
| `GetByName` | `GetEndpointByNameRequest` | `Endpoint` | ✅ |
| `List` | `ListEndpointsRequest` | `ListEndpointsResponse` |  |
| `Create` | `CreateEndpointRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteEndpointRequest` | `common.v1.Operation` | ✅ |
| `Start` | `StartEndpointRequest` | `common.v1.Operation` |  |
| `Restart` | `RestartEndpointRequest` | `common.v1.Operation` |  |
| `Stop` | `StopEndpointRequest` | `common.v1.Operation` |  |

#### `nebius.ai.v1.HardwareService`

Endpoint: `apps.msp.api.nebius.cloud:443` · Proto: [`nebius/ai/v1/hardware_service.proto`](https://github.com/nebius/api/blob/main/nebius/ai/v1/hardware_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `List` | `ListHardwareRequest` | `ListHardwareResponse` |  |

#### `nebius.ai.v1.JobService`

Endpoint: `apps.msp.api.nebius.cloud:443` · Proto: [`nebius/ai/v1/job_service.proto`](https://github.com/nebius/api/blob/main/nebius/ai/v1/job_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetJobRequest` | `Job` |  |
| `GetByName` | `GetJobByNameRequest` | `Job` |  |
| `List` | `ListJobsRequest` | `ListJobsResponse` |  |
| `Create` | `CreateJobRequest` | `common.v1.Operation` |  |
| `Delete` | `DeleteJobRequest` | `common.v1.Operation` |  |
| `Cancel` | `CancelJobRequest` | `common.v1.Operation` |  |
| `Restart` | `RestartJobRequest` | `common.v1.Operation` |  |
