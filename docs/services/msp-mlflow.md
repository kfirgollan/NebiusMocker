# Managed MLflow (MSP)

> Module: `services/mspmlflow` · `NEBIUS_MOCKER_SERVICES=msp-mlflow`
> M1 tier: **T2** (`mock` backend) · **T3**: `docker` backend (a real MLflow tracking server in Docker) · Endpoint: `mlflow.msp.api.nebius.cloud` · Operations: `common.v1alpha1.OperationService`

## Implementation status

| Piece | Status |
|---|---|
| `ClusterService`: Get, GetByName, List (paged), Create, Delete, Stop, Start | ✅ |
| References checked: service account (same project), bucket by name, VPC network (default network when omitted; bound while the cluster lives) | ✅ |
| Artifact store: the named bucket, or a bucket created in the project (`status.effective_storage_bucket_name`); an IAM access key of the service account for the server, deleted with the cluster; the bucket stays | ✅ |
| `status`: phases, `tracking_endpoint(s)`, `mlflow_version`, live `experiments_count` | ✅ |
| Backend port (`domain.Backend` in `services/mspmlflow/domain/ports.go`); `mock` is the default | ✅ |
| `docker` backend: the official MLflow image with basic auth (`admin_username`/`admin_password`), HTTPS from the mocker CA, artifacts proxied to the Nebius bucket through the S3 front | ✅ e2e: `make e2e-msp` |
| Authorization: rules for every RPC (`mlflow.clusters.*`, `stop`/`start` verbs) | ✅ |
| `nebius_msp_mlflow_v1alpha1_cluster` resource and data source with the official provider | ✅ e2e: `test/terraform/msp` |
| `nebius msp mlflow v1alpha1 cluster …` CLI flows | ✅ e2e: `test/cli/msp` |
| Quotas, data export tooling | Not yet (see [Not yet](#not-yet)) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/mspmlflow/{domain,dal,api,backend/mock,backend/docker}`,
`services/iam/domain/systemaccesskeys_test.go`, `services/storage/domain/peerbuckets_test.go`), gosdk contract tests
(`test/contract/msp_mlflow_*`), the Terraform and CLI e2e suites (`test/terraform/msp`, `test/cli/msp`, `make e2e`), and
the Docker suite (`test/msp`, `make e2e-msp`).

| Resource kind | Message | NID prefix | Parent | Phases |
|---|---|---|---|---|
| Cluster | `msp.mlflow.v1alpha1.Cluster` | `mlflow` | project | `PROVISIONING`, `RUNNING`, `STOPPING`, `RESUMING`, `DELETING` (+ `state`) |

### Behavior notes

- **Lifecycle** as Managed PostgreSQL: Create → `PROVISIONING` → `RUNNING`; Stop → `STOPPING`/`FINISHED` (no stopped
  phase in the API); Start → `RESUMING` → `RUNNING`; Delete → `DELETING`, then gone. There is no Update RPC: the provider
  plans every change as a replacement.
- **Validation.** `admin_username` 1–63 characters; `admin_password` required and longer than 12 characters (MLflow's
  basic-auth server refuses shorter admin passwords); `service_account_id` must exist in the cluster's project;
  `storage_bucket_name`, when set, must be an existing bucket of the project; `size` defaults to `medium` (the only
  documented size: 8 vCPUs, 32 GiB), else must be in the catalog.
- **Secrets.** `admin_password` is `INPUT_ONLY` and sensitive: never stored, never returned, removed from the stored
  operation request.
- **Artifact store.** Without `storage_bucket_name` the cluster creates a bucket named after the cluster ID in its
  project. Either way the cluster gets an access key of its service account (an ordinary `iam.v2` AccessKey named after
  the cluster, visible in `AccessKeyService.List`); the tracking server writes artifacts with it, acting with the
  service account's permissions, so the account needs write access to the bucket (for example membership of the
  project's `editors` group, as the docs set up). Delete removes the key; the bucket and its artifacts stay (docs).
- **Endpoints.** `status.tracking_endpoints.private` always, `.public` with `public_access`; `tracking_endpoint` is the
  public one with public access, else the private one. They are host[:port] without a scheme: clients prepend `https://`
  (docs). The mock backend's are `<id>.private.mlflow.mocker.localhost`-style names nothing serves.
- **`experiments_count`** is read from the tracking server on every Get/List of a running cluster.

#### Authorization

`mlflow.clusters.{get,list,create,delete,stop,start}` on the cluster or its project. The access key and bucket are
created by the service on the caller's behalf (no extra permission checks on the service account).

### Not yet

- Quotas (owned by the quotas area); the `mlflow-export-import` workflow is the user's own tool and works against the
  docker backend like any tracking server.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tested |
|---|---|---|---|---|
| Resource | `nebius_msp_mlflow_v1alpha1_cluster` | `nebius.msp.mlflow.v1alpha1.ClusterService` | Get, GetByName, Create, Delete + OperationService.Get | ✅ `test/terraform/msp` |
| Data source | `nebius_msp_mlflow_v1alpha1_cluster` | `nebius.msp.mlflow.v1alpha1.ClusterService` | Get, GetByName | ✅ `test/terraform/msp` |

## CLI

`nebius msp mlflow v1alpha1 cluster create|get|get-by-name|list|stop|start|delete` work against the mock (`test/cli/msp`).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_MSP_MLFLOW_BACKEND` | `mock` | `mock` (no server) or `docker` (a real tracking server) |
| `NEBIUS_MOCKER_MSP_MLFLOW_IMAGE` | `ghcr.io/mlflow/mlflow:v2.22.0` | Official image the server is built from (the backend adds `boto3` and `mlflow[auth]` once, tagged `<image>-nebius-mocker`) |
| `NEBIUS_MOCKER_MSP_MLFLOW_VERSION` | `2.22.0` | `status.mlflow_version`; must match the image |
| `NEBIUS_MOCKER_MSP_MLFLOW_SIZES` | `medium=8vcpu-32gb` | `size=<n>vcpu-<m>gb,…`, smallest (the default) first |
| `NEBIUS_MOCKER_MSP_S3_ENDPOINT` | `http://host.docker.internal:<S3 front port>` | Where the server containers reach the Object Storage S3 front |
| `NEBIUS_MOCKER_MSP_DOCKER_HOST`, `_BIND_IP`, `_ENDPOINT_HOST`, `_TLS_DIR`, `_START_TIMEOUT` | see [configuration](../architecture/11-configuration.md) | Shared with Managed PostgreSQL |

## Docker backend: a real tracking server

With `NEBIUS_MOCKER_MSP_MLFLOW_BACKEND=docker` every cluster runs `mlflow server --app-name basic-auth` from the official
MLflow image (arm64 and amd64) in its own container and volume (SQLite backend store and auth database), with:

- the admin user from `admin_username`/`admin_password` (HTTP basic auth, as `MLFLOW_TRACKING_USERNAME`/`PASSWORD`);
- HTTPS with a certificate from the mocker's local CA (`<NEBIUS_MOCKER_MSP_TLS_DIR>/ca.pem`);
- proxied artifact storage (`--serve-artifacts`) in `s3://<effective bucket>/`, through the mocker's S3 front with the
  service account's access key, so artifacts land in the Nebius bucket and are readable with any S3 client;
- memory and CPU limits from the size; a port picked once and kept across stop/start.

The image with the S3 and auth extras is built once on first use (needs network access to PyPI). Containers carry
`nebius-mocker.backend=msp-mlflow` and are removed on delete and at mocker startup/shutdown.

```bash
export MLFLOW_TRACKING_URI=https://$(nebius msp mlflow v1alpha1 cluster get-by-name --name mlflow --format jsonpath='{.status.tracking_endpoint}')
export MLFLOW_TRACKING_USERNAME=admin MLFLOW_TRACKING_PASSWORD='<admin password>'
export MLFLOW_TRACKING_SERVER_CERT_PATH="$NEBIUS_MOCKER_MSP_TLS_DIR/ca.pem"   # the mocker CA
python -c 'import mlflow; mlflow.set_experiment("demo"); mlflow.start_run(); mlflow.log_metric("loss", 0.1); mlflow.end_run()'
```

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
