# Managed PostgreSQL (MSP)

> Module: `services/msppostgresql` · `NEBIUS_MOCKER_SERVICES=msp-postgresql`
> M1 tier: **T2** (`mock` backend) · **T3**: `docker` backend (real PostgreSQL + PgBouncer in Docker) · Endpoint: `postgresql.msp.api.nebius.cloud` · Operations: `common.v1alpha1.OperationService`

## Implementation status

| Piece | Status |
|---|---|
| `ClusterService`: Get, GetByName, GetForBackup, List (paged), Create, Update (reset mask, `resource_version`), Delete, Restore (incl. point-in-time), Stop, Start | ✅ |
| `BackupService`: Get, List (by project, paged, proto ordering), ListByCluster, Create (on-demand), Delete (on-demand only) | ✅ |
| Catalog validation: version, platform/preset (`status.preset_details`), disk type and size (grow only), hosts count, pooler, `postgresql_config_16` ranges, backup window and retention | ✅ |
| Automatic backups: one right after a cluster first runs, then daily at `backup_window_start` (UTC, mock clock); retention `<N>d` (7–30) also for backups of deleted clusters | ✅ |
| Backend port (`domain.Backend` in `services/msppostgresql/domain/ports.go`); `mock` is the default | ✅ |
| `docker` backend: a real PostgreSQL primary + streaming replicas, PgBouncer pooler, TLS from the mocker CA, parameters applied, password changes, physical backups with WAL archiving and PITR restore | ✅ e2e: `make e2e-msp` |
| Authorization: rules for every RPC (`postgresql.clusters.*`, `postgresql.backups.*`, `stop`/`start` verbs) | ✅ |
| `nebius_msp_postgresql_v1alpha1_cluster` resource and data source with the official provider | ✅ e2e: `test/terraform/msp` |
| `nebius msp postgresql v1alpha1 cluster …` / `backup …` CLI flows | ✅ e2e: `test/cli/msp` |
| Quotas: `msp.postgresql.cluster.count`, `.vcpu`, `.memory`, `.disk.size` charged on Create and Restore, the growth on Update, released on Delete ([quotas.md](quotas.md#catalog)) | ✅ |
| Cross-region backup replication, maintenance windows | Not yet (see [Not yet](#not-yet)) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/msppostgresql/{domain,dal,api,backend/mock,backend/docker}`, `pkg/dockerhost`), gosdk
contract tests (`test/contract/msp_postgresql_*`), the Terraform and CLI e2e suites (`test/terraform/msp`,
`test/cli/msp`, run by `make e2e`), and the Docker suite (`test/msp` plus the backend's own `e2e_msp` test, `make e2e-msp`).

| Resource kind | Message | NID prefix | Parent | Phases |
|---|---|---|---|---|
| Cluster | `msp.postgresql.v1alpha1.Cluster` | `postgresql` | project | `PROVISIONING`, `RUNNING`, `UPDATING`, `STOPPING`, `RESUMING`, `DELETING` (+ `state`) |
| Backup | `msp.postgresql.v1alpha1.Backup` | `postgresqlbackup` | its source cluster (outlives it until retention) | — |

### Behavior notes

- **Lifecycle.** Create → `PROVISIONING`/`IN_PROGRESS` → `RUNNING`/`FINISHED`. Update → `UPDATING` → `RUNNING`. Stop →
  `STOPPING`/`IN_PROGRESS` → `STOPPING`/`FINISHED` (the API has no stopped phase; a stopped cluster stays
  `STOPPING`+`FINISHED`). Start → `RESUMING` → `RUNNING`. A backend failure leaves the phase and sets `STATE_ERROR`; the
  operation fails with that error. Update and backups of a stopped cluster fail with `FAILED_PRECONDITION`.
- **Catalog** (`NEBIUS_MOCKER_MSP_*`, see [Configuration](#configuration)): version `16` (docs: "currently supports only
  version 16"), platform `cpu-e2` with the compute non-GPU presets `2vcpu-8gb` … `80vcpu-320gb`, disk type
  `network-ssd`, 10–4096 GiB (Update may only grow it), 1–3 hosts.
- **Network.** `spec.network_id` defaults to the project's default network; the cluster binds it (vpc refuses to delete a
  bound network) and releases it on delete.
- **Bootstrap.** `bootstrap.user_password` is required on Create and Restore (every documented example sets it), is
  `INPUT_ONLY` and sensitive: never stored, never returned, removed from the stored operation request. An Update that
  sets it changes the password. `user_name`, `db_name` are immutable.
- **Defaults stay internal.** The provider marks `pooler_config`, `postgresql_config_16`, `public_access` and `backup`
  optional and not computed, so the mock echoes exactly what was sent and applies the documented defaults internally
  (pooling `SESSION`, pool size 20, backup window `00:02:00`, retention `7d`).
- **Immutable fields** (`INVALID_ARGUMENT` "field is immutable"): `description`, `network_id`, `config.version`,
  `bootstrap.user_name`, `bootstrap.db_name`, `backup.backup_window_start`, `backup.retention_policy`.
- **Backups.** A backup operation's `resource_id` is the backup it creates or deletes; it serializes with the
  cluster's other operations (one operation at a time per cluster). Only on-demand backups can be deleted;
  automatic ones expire with the retention. Backups outlive their cluster (`source_cluster_visible = false`), and
  `GetForBackup` returns the source cluster's configuration (no password) even after it was deleted.
- **Restore** creates a new cluster in the backup's project from `backup_id` of `source_cluster_id`: same version, a disk
  at least as large as the source's, a bootstrap password. `recovery_time` (point in time) needs an automatic backup and
  must lie between that backup's end and now.
- **Endpoints.** `status.connection_endpoints` come from the backend. With `public_access` the public endpoints are set,
  else empty. The mock backend's are `<id>.rw.postgresql.mocker.localhost:5432`-style names nothing listens on.

#### Fields rejected as not emulated

`INVALID_ARGUMENT` naming the field: `spec.backup.replication_regions` and `spec.backup.replication_interval`
(cross-region backup replication has nowhere to go locally).

#### Authorization

`postgresql.clusters.{get,list,create,update,delete,stop,start}` on the cluster or its project; GetForBackup needs `get`
on the cluster; Restore needs `create` on the new cluster's project. `postgresql.backups.{get,list,create,delete}` on
the backup's source cluster (List: on the project). Viewers read clusters and backups, editors change them. Backups of a
deleted cluster stay readable: the cluster ID still resolves to its project for authorization.

### Not yet

- Cross-region backup replication (`replication_regions`, `replication_interval`): rejected, see above.
- Maintenance windows (`nebius maintenance`), metrics and logs.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tested |
|---|---|---|---|---|
| Resource | `nebius_msp_postgresql_v1alpha1_cluster` | `nebius.msp.postgresql.v1alpha1.ClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | ✅ `test/terraform/msp` |
| Data source | `nebius_msp_postgresql_v1alpha1_cluster` | `nebius.msp.postgresql.v1alpha1.ClusterService` | Get, GetByName | ✅ `test/terraform/msp` |

The suite runs under `NEBIUS_MOCKER_AUTHZ=enforce`: create, empty plan, in-place update (preset, hosts, disk, a
parameter, pooler mode, password, public access), empty plan, replacements (description, `db_name`), both data sources,
destroy, a viewer that cannot apply, and an invalid preset that fails apply.

## CLI

`nebius msp postgresql v1alpha1 cluster create|get|get-by-name|list|update|stop|start|restore|delete` and
`backup create|list|list-by-cluster|delete` work against the mock (`test/cli/msp`).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND` | `mock` | `mock` (no server) or `docker` (real PostgreSQL) |
| `NEBIUS_MOCKER_MSP_POSTGRESQL_IMAGES` | `16=postgres:16.11-alpine` | `version=image,…`: the version catalog and the image per version |
| `NEBIUS_MOCKER_MSP_PGBOUNCER_IMAGE` | `edoburu/pgbouncer:v1.25.2-p0` | The connection pooler image |
| `NEBIUS_MOCKER_MSP_POSTGRESQL_PRESETS` | `cpu-e2=2vcpu-8gb\|…\|80vcpu-320gb` | `platform=preset\|…;…` resources catalog |
| `NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_TYPES` | `network-ssd` | Accepted disk types |
| `NEBIUS_MOCKER_MSP_POSTGRESQL_MAX_HOSTS` | `3` | Largest `hosts.count` |
| `NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_UNIT` | `1Gi` | Bytes one GiB of `size_gibibytes` holds, for disk-full enforcement only (`8Mi`: a 10 GiB disk holds 80 MiB). Validation stays 10-4096 |
| `NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_CHECK_INTERVAL` | `1m` | How often the disk usage of running clusters is checked |
| `NEBIUS_MOCKER_MSP_DOCKER_HOST`, `_BIND_IP`, `_ENDPOINT_HOST`, `_TLS_DIR`, `_START_TIMEOUT` | see [configuration](../architecture/11-configuration.md) | Shared with Managed MLflow |

## Docker backend: real PostgreSQL

With `NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND=docker` every cluster is real PostgreSQL from the official `postgres` image
(arm64 and amd64), driven through the Docker Engine API (`pkg/dockerhost`):

- **Hosts.** One primary and `hosts.count - 1` streaming replicas (`pg_basebackup -R`), each a container with its own
  data volume, memory and CPU limits from the preset, on a per-cluster Docker network. Scaling down removes hosts.
- **Pooler.** PgBouncer in front of the primary, with `pool_mode` and `default_pool_size` from `pooler_config`; it
  authenticates users through `auth_query` (CloudNativePG style), so new users and password changes need no restart.
- **Parameters.** `postgresql_config_16` becomes `-c name=value` server flags; a change recreates the hosts and keeps the
  data.
- **TLS.** PostgreSQL and PgBouncer serve a certificate from the mocker's local CA (`<NEBIUS_MOCKER_MSP_TLS_DIR>/ca.pem`),
  so the documented `sslmode=verify-full` works.
- **Endpoints.** `private_read_write` is `<NEBIUS_MOCKER_MSP_ENDPOINT_HOST>:<port>` of the pooler, `private_read_only`
  the first replica (the primary with one host); the public endpoints repeat them with `public_access`. Ports are picked
  once and stay stable across stop/start and recreation. Unlike Nebius (host name, port 5432) the endpoint carries the
  port, because every cluster shares one machine.
- **Backups.** WAL is archived continuously; a backup is a `pg_basebackup` into a per-mocker backups volume; Restore
  replays the archive to `recovery_time` and promotes.
- **Lifecycle.** Stop stops the containers, Start starts them; Delete removes containers, volumes and the network.
  Everything carries `nebius-mocker.backend=msp-postgresql` and `nebius-mocker.owner=<edge address>`; a mocker removes its
  own leftovers at startup and shutdown. Hosts and pooler run with restart policy `on-failure`; the e2e suites still
  fail on any crash, OOM kill or `RestartCount` above 0 (`pkg/dockerhost/crashwatch`).

### Disk size

`spec.config.template.disk.size_gibibytes` is enforced by the docker backend, approximately. Nebius documents no
full-disk behavior (only the dashboard metrics "Disk used space" and "Total service disk usage"), and the API has no
disk-usage field, so what follows is the mock's own design:

- **Usage** is `du` of the primary's data directory (`pg_wal` included; archived WAL and backups are not counted),
  measured after every create, update, start and restore, and every `NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_CHECK_INTERVAL`.
  The limit is `size_gibibytes` x `NEBIUS_MOCKER_MSP_POSTGRESQL_DISK_UNIT` (1 GiB by default).
- **Full** (usage at or above the limit): every connectable non-template database gets
  `ALTER DATABASE ... SET default_transaction_read_only = on`, and the other client sessions are terminated, so pooled
  and existing connections pick it up. Writes fail with SQLSTATE `25006` ("cannot execute ... in a read-only
  transaction"), reads work, the cluster stays `RUNNING` and its state is `STATE_DEGRADED`. A real full disk would fail
  with `ENOSPC` / `disk_full` (`53100`) instead. A session can `SET default_transaction_read_only = off` to delete
  data: the mock's escape hatch.
- **Recovery**: growing the disk (an update) or shrinking the data (below the limit at the next check) resets the
  setting, ends the sessions once more and returns the state to `STATE_FINISHED`. The state is kept nowhere but in the
  databases' settings, so a check is idempotent.
- A `DEGRADED` cluster still counts as running (backups, updates, stop). The `mock` backend stores no data and never
  fills.

Connect as the docs show (`docs.nebius.com/postgresql/databases/connect`), with the mocker's CA as the root certificate:

```bash
cp "$NEBIUS_MOCKER_MSP_TLS_DIR/ca.pem" ~/.postgresql/root.crt   # default: <user config dir>/nebius-mocker/ca
ep=$(nebius msp postgresql v1alpha1 cluster get-by-name --name pg --format jsonpath='{.status.connection_endpoints.private_read_write}')
psql "host=${ep%:*} port=${ep##*:} sslmode=verify-full dbname=app user=app"
```

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
