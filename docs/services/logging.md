# Logging

> Module: `services/logging` · Image: `nebius-mocker-logging` · `NEBIUS_MOCKER_SERVICES=logging`
> M1 tier: **T0** · M4: **T1** · Endpoints: `cpl.teplo` (log export), `observability-agent-manager` (agent versions)

## Overview

- `LogExportService`: `ExportLogs` (operation), `GetExportInfo`, `ListExports`, `CancelExportLogs`. Export jobs for
  platform logs (prefix `oplogexport`).
- `agentmanager.v1.VersionService.GetVersion`: tells the observability agent which version to run.

The Terraform provider does not use it.

## Emulation design (M4)

- Log export: export jobs complete on the mock clock and write synthetic log lines (generated from the request journal
  and resource lifecycle events) to the S3 data plane.
- Agent version: returns a configurable version (`NEBIUS_MOCKER_LOGGING_AGENT_VERSION`) for agent-update tests.

## Terraform

_No Terraform provider objects use this area._

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.logging.agentmanager.v1.VersionService`

Endpoint: `observability-agent-manager.api.nebius.cloud:443` · Proto: [`nebius/logging/v1/agentmanager/version_service.proto`](https://github.com/nebius/api/blob/main/nebius/logging/v1/agentmanager/version_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `GetVersion` | `GetVersionRequest` | `GetVersionResponse` |  |

#### `nebius.logging.v1.LogExportService`

Endpoint: `cpl.teplo.api.nebius.cloud:443` · Proto: [`nebius/logging/v1/log_export_service.proto`](https://github.com/nebius/api/blob/main/nebius/logging/v1/log_export_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `ExportLogs` | `ExportLogsRequest` | `common.v1.Operation` |  |
| `GetExportInfo` | `GetExportInfoRequest` | `ExportStatus` |  |
| `ListExports` | `ListExportsRequest` | `ListExportsResponse` |  |
| `CancelExportLogs` | `CancelExportLogsRequest` | `google.protobuf.Empty` |  |
