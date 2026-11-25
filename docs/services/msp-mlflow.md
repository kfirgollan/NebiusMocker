# Managed MLflow (MSP)

> Module: `services/msp` (sub-area `mlflow`) · Image: `nebius-mocker-msp` · `NEBIUS_MOCKER_SERVICES=msp`
> M1 tier: **T1** · Endpoint: `mlflow.msp.api.nebius.cloud` · Operations: `common.v1alpha1.OperationService`

## Overview

Managed MLflow tracking servers (`Cluster`, prefix `mlflow`): `admin_username`/`admin_password` (sensitive),
`service_account_id`, `storage_bucket_name`, `network_id`, `public_access`, `size`. Status has `tracking_endpoint(s)`,
`effective_storage_bucket_name`, `mlflow_version` and `experiments_count`.

## Emulation design

- References (SA via iam, bucket via storage, network via vpc) are checked through `peer`. When `storage_bucket_name` is
  empty, the module creates a managed bucket, recorded in `effective_storage_bucket_name`.
- Synthesized tracking endpoint: `https://<cluster-id>.mlflow.mocker.localhost`.
- No `Update` in the provider's call set (`Create`, `Get`, `GetByName`, `Delete`), so changes plan as replacements.
- **Possible T3**: an `mlflow` container backend.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_msp_mlflow_v1alpha1_cluster` | `nebius.msp.mlflow.v1alpha1.ClusterService` | Get, GetByName, Create, Delete + OperationService.Get | T1 |
| Data source | `nebius_msp_mlflow_v1alpha1_cluster` | `nebius.msp.mlflow.v1alpha1.ClusterService` | Get, GetByName | T1 |

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.msp.mlflow.v1alpha1.ClusterService`

Endpoint: `mlflow.msp.api.nebius.cloud:443` · Proto: [`nebius/msp/mlflow/v1alpha1/cluster_service.proto`](https://github.com/nebius/api/blob/main/nebius/msp/mlflow/v1alpha1/cluster_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetClusterRequest` | `Cluster` | ✅ |
| `GetByName` | `GetClusterByNameRequest` | `Cluster` | ✅ |
| `List` | `ListClustersRequest` | `ListClustersResponse` |  |
| `Create` | `CreateClusterRequest` | `common.v1alpha1.Operation` | ✅ |
| `Delete` | `DeleteClusterRequest` | `common.v1alpha1.Operation` | ✅ |
| `Stop` | `StopClusterRequest` | `common.v1alpha1.Operation` |  |
| `Start` | `StartClusterRequest` | `common.v1alpha1.Operation` |  |
