# AI Studio — endpoints, jobs, devlabs

> Module: `services/ai` · Image: `nebius-mocker-ai` · `NEBIUS_MOCKER_SERVICES=ai`
> M1 tier: **T1** endpoint · T0 jobs, devlabs, hardware · Endpoint: `apps.msp.api.nebius.cloud`

## Overview

Container-based serverless AI workloads:

- **Endpoints** (`aiendpoint`): long-running inference containers (`image`, `ports`, `platform`/`preset`/`gpu`, `volumes`
  from S3 or filesystems, `auth_token` or MysteryBox secret, `subnet_id`, `public_ip`). Status has private/public
  endpoints and instances.
- **Jobs** (`aijob`): run-to-completion containers with `restart_attempts` and `timeout`.
- **Devlabs** (`devlab`): interactive development environments.
- **HardwareService**: lists the available hardware configurations.

## Emulation design

- Endpoint: T1 CRUD with references (subnet, MysteryBox secret, registry credentials) checked through `peer`, and
  platform/preset validated against compute's catalog. The lifecycle synthesizes endpoint URLs and fake instances.
  The provider calls `Create`, `Get`, `GetByName` and `Delete` (no Update), so changes plan as replacements.
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
