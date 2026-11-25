# Audit

> Module: `services/audit` · Image: `nebius-mocker-audit` · `NEBIUS_MOCKER_SERVICES=audit`
> M1 tier: **T0** · M4: **T1** · Endpoint: `audit.api.nebius.cloud`

## Overview

- `AuditEventService.List`: query audit events (CloudEvents-like: `source`, `type`, `service`, `action`, `time`,
  `authentication`, `authorization`, `resource`, `request`, `response`, `status`).
- `AuditEventExportService`: Start, Cancel, Get, List of export jobs.

The Terraform provider does not use it.

## Emulation design (M4)

The request journal ([architecture/04](../architecture/04-request-pipeline.md)) already captures every mutating call with
its principal, authz decision and result, so audit events are a **projection of the journal**: one event per mutating
RPC, plus authz denials. `List` supports time range and filter. Export jobs write JSONL to the S3 data plane, if enabled.
This lets users test their audit-processing pipelines against realistic events produced by their own Terraform runs.

## Terraform

_No Terraform provider objects use this area._

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.audit.v2.AuditEventExportService`

Endpoint: `audit.api.nebius.cloud:443` · Proto: [`nebius/audit/v2/audit_event_export_service.proto`](https://github.com/nebius/api/blob/main/nebius/audit/v2/audit_event_export_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Start` | `StartRequest` | `common.v1.Operation` |  |
| `Cancel` | `CancelRequest` | `common.v1.Operation` |  |
| `Get` | `GetRequest` | `AuditEventExport` |  |
| `List` | `ListRequest` | `ListAuditEventExportResponse` |  |

#### `nebius.audit.v2.AuditEventService`

Endpoint: `audit.api.nebius.cloud:443` · Proto: [`nebius/audit/v2/audit_event_service.proto`](https://github.com/nebius/api/blob/main/nebius/audit/v2/audit_event_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `List` | `ListAuditEventRequest` | `ListAuditEventResponse` |  |
