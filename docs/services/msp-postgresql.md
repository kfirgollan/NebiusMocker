# Managed PostgreSQL (MSP)

> Module: `services/msp` (sub-area `postgresql`) · Image: `nebius-mocker-msp` · `NEBIUS_MOCKER_SERVICES=msp`
> M1 tier: **T1** · Endpoint: `postgresql.msp.api.nebius.cloud` · Operations: `common.v1alpha1.OperationService`

## Overview

Managed PostgreSQL clusters (`Cluster`, prefix `postgresql`): network placement (`network_id`), config (version, presets,
pooler, parameters), bootstrap (database and user), backups. There is also a `BackupService` for listing and restoring backups.
Status has `phase`, `state`, `preset_details` and `connection_endpoints`.

## Emulation design

- `network_id` is reference-checked through vpc. Presets come from a seeded catalog.
- `connection_endpoints` are synthesized (`<cluster-id>.postgresql.mocker.localhost:5432`).
- Bootstrap passwords are `INPUT_ONLY` and sensitive: accepted, never returned.
- Backups are synthetic records created on a schedule (mock clock) when `spec.backup` is set.
- **Possible T3**: `NEBIUS_MOCKER_MSP_POSTGRES_BACKEND=docker`, which starts a real `postgres:<version>` container so that
  connection strings work.
- This is an alpha API on `v1alpha1` operations. The framework serves both operation-service versions, so nothing
  special is needed here.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_msp_postgresql_v1alpha1_cluster` | `nebius.msp.postgresql.v1alpha1.ClusterService` | Get, GetByName, Create, Delete, Update + OperationService.Get | T1 |
| Data source | `nebius_msp_postgresql_v1alpha1_cluster` | `nebius.msp.postgresql.v1alpha1.ClusterService` | Get, GetByName | T1 |

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.msp.postgresql.v1alpha1.BackupService`

Endpoint: `postgresql.msp.api.nebius.cloud:443` · Proto: [`nebius/msp/postgresql/v1alpha1/backup_service.proto`](https://github.com/nebius/api/blob/main/nebius/msp/postgresql/v1alpha1/backup_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetBackupRequest` | `Backup` |  |
| `List` | `ListBackupsRequest` | `ListBackupsResponse` |  |
| `ListByCluster` | `ListBackupsByClusterRequest` | `ListBackupsResponse` |  |
| `Create` | `CreateBackupRequest` | `common.v1alpha1.Operation` |  |
| `Delete` | `DeleteBackupRequest` | `common.v1alpha1.Operation` |  |

#### `nebius.msp.postgresql.v1alpha1.ClusterService`

Endpoint: `postgresql.msp.api.nebius.cloud:443` · Proto: [`nebius/msp/postgresql/v1alpha1/cluster_service.proto`](https://github.com/nebius/api/blob/main/nebius/msp/postgresql/v1alpha1/cluster_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetClusterRequest` | `Cluster` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Cluster` | ✅ |
| `GetForBackup` | `GetClusterForBackupRequest` | `Cluster` |  |
| `List` | `ListClustersRequest` | `ListClustersResponse` |  |
| `Create` | `CreateClusterRequest` | `common.v1alpha1.Operation` | ✅ |
| `Delete` | `DeleteClusterRequest` | `common.v1alpha1.Operation` | ✅ |
| `Update` | `UpdateClusterRequest` | `common.v1alpha1.Operation` | ✅ |
| `Restore` | `RestoreClusterRequest` | `common.v1alpha1.Operation` |  |
| `Stop` | `StopClusterRequest` | `common.v1alpha1.Operation` |  |
| `Start` | `StartClusterRequest` | `common.v1alpha1.Operation` |  |
