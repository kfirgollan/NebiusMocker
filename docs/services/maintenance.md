# Maintenance (MSP)

> Module: `services/maintenance` · Image: `nebius-mocker-maintenance` · `NEBIUS_MOCKER_SERVICES=maintenance`
> M1 tier: **T0** · M4: **T1** · Endpoint: `maintenance.msp.api.nebius.cloud`

## Overview

Scheduled maintenance windows for managed services (`Maintenance`, prefix `maintenance`): `spec.scheduled_at`, and a
status with `affected_resources`, `started_at`, `finished_at`, `state` and `reschedulable`. RPCs: `Get`, `List`, `Update`
(reschedule). Not to be confused with `compute.v1.MaintenanceService`, which covers host maintenance events for instances
(see [compute](compute.md)).

## Emulation design (M4)

Maintenance events are injected via the admin API (`POST /_mocker/maintenance`) against existing MSP or compute resources.
They then progress on the mock clock: scheduled → in progress → finished, setting `reconciling` on the affected
resources while in progress. `Update` reschedules only if `reschedulable`. Users can test their maintenance-handling
automation with this.

## Terraform

_No Terraform provider objects use this area._

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.maintenance.v1alpha1.MaintenanceService`

Endpoint: `maintenance.msp.api.nebius.cloud:443` · Proto: [`nebius/maintenance/v1alpha1/maintenance_service.proto`](https://github.com/nebius/api/blob/main/nebius/maintenance/v1alpha1/maintenance_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetMaintenanceRequest` | `Maintenance` |  |
| `List` | `ListMaintenancesRequest` | `ListMaintenancesResponse` |  |
| `Update` | `UpdateMaintenanceRequest` | `UpdateMaintenanceResponse` |  |
