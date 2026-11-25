# Compute

> Module: `services/compute` · Image: `nebius-mocker-compute` · `NEBIUS_MOCKER_SERVICES=compute`
> M1 tier: **T2** · Endpoint: `compute.api.nebius.cloud`

## Overview

Compute covers VM and GPU **instances**, block **disks** and **disk snapshots**, shared **filesystems**, **images**
(public catalog and custom), **GPU clusters** (InfiniBand fabrics), **NVLink instance groups**, the **platform**
catalog (CPU/GPU types and presets), host **maintenance** events and bare-metal **nodes**.

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Instance | `Instance` | `computeinstance` | project | `CREATING, UPDATING, STARTING, RUNNING, STOPPING, STOPPED, DELETING, ERROR` |
| Disk | `Disk` | `computedisk` | project | `CREATING, READY, UPDATING, DELETING, ERROR, BROKEN` |
| Disk snapshot | `DiskSnapshot` | `computedisksnapshot` | project | |
| Filesystem | `Filesystem` | `computefilesystem` | project | `CREATING, READY, UPDATING, DELETING, ERROR`; types `NETWORK_SSD, NETWORK_HDD, WEKA, VAST` |
| Image | `Image` | `computeimage` | project (`project-{region}public-images` for public) | |
| GPU cluster | `GpuCluster` | `computegpucluster` | project | `spec.infiniband_fabric*`; `status.instances[]` |
| NVL instance group | `NVLInstanceGroup` | `computenvlinstancegroup` | project | |
| Platform | `Platform` | `computeplatform` | — (catalog) | read-only |
| Maintenance | `MaintenanceEvent` | `computemaintenance` | instance | read-only |
| Node | `Node` | — | — | `SetUnhealthy` |

## Emulation design

### Catalogs (seed)
- **Platforms** (`PlatformService.List/Get`): name, presets (`vcpu_count`, `memory_gibibytes`, `gpu_count`), `allow_gpu_clustering`,
  `allowed_for_preemptibles`, `gpu_count_quota_type`, `allow_preset_change`. Defaults include `cpu-e2`, `cpu-d3`,
  `gpu-h100-sxm`, `gpu-h200-sxm`, `gpu-b200-sxm`, `gpu-l40s-a`, `gpu-l40s-d`, with the presets the public docs list. These
  values are **illustrative**, and parity recording replaces them with live data.
- **Public images**: a catalog under `project-<routing>public-images` with families (`ubuntu22.04-driverless`,
  `ubuntu22.04-cuda12`, `ubuntu24.04-driverless`, `mk8s-worker-node-…`). Every family has at least one version.
  `GetLatestByFamily` returns the newest. `ListPublic` lists the catalog. Each image has a CPU architecture, a minimum disk
  size, and a status of `READY`.

### Instances
- `resources.platform` (immutable) + `resources.preset` must exist in the platform catalog. Changing the preset needs the
  instance to be `STOPPED` and the platform's `allow_preset_change`.
- **Boot disk**: exactly one of `existing_disk{id}` or `managed_disk{name, spec}`. A managed disk is created by compute as
  a real `computedisk` (same project, `status.managed_by = instance id`) and deleted with the instance. An existing disk
  must be `READY`, must not have a read-write attachment, and must be at least the image's minimum size.
- **Secondary disks / filesystems**: attachment bookkeeping in `Disk.status.read_write_attachment` / `read_only_attachments`.
  Attaching a disk that is already attached read-write elsewhere → `FAILED_PRECONDITION`.
- **Network interfaces**: `subnet_id` is checked via `peer` → vpc. `ip_address.allocation_id` binds an existing
  allocation, or the module requests an ephemeral one. `public_ip_address` draws from the public pool.
  `status.network_interfaces[*]` gets the IPs and a deterministic MAC (`02:4e:42:…`).
- **GPU cluster**: `spec.gpu_cluster.id` requires a GPU platform with `allow_gpu_clustering`. The instance is added to
  `GpuCluster.status.instances` and gets a synthetic `infiniband_topology_path`.
- `spec.stopped=true` on Create → final state `STOPPED`. Toggling it on Update triggers the Stop/Start paths.
  `Start`/`Stop` RPCs have state guards.
- `preemptible`, `recovery_policy`, `reservation_policy`, spot fields: stored and validated. `NotEnoughResources` can be
  simulated through the capacity pools ([architecture/09](../architecture/09-quotas-limits-faults.md#capacity-simulation)).
- `cloud_init_user_data` is stored (sensitive, redacted in logs) and never executed in `mock` mode.

### Disks, snapshots and filesystems
- Size is given in exactly one unit field (`size_bytes|kibibytes|mebibytes|gibibytes`). `status.size_bytes` holds the
  normalized value. The size must be a multiple of `block_size_bytes` (default 4096), and may only **grow** on Update.
- The source (`source_image_id`, `source_image_family`, `source_snapshot_id`) is immutable. Family resolution is recorded
  in `status.source_image_id`.
- `forbid_deletion=true` → Delete returns `FAILED_PRECONDITION`.
- `DiskSnapshot` requires a `READY` source disk. `ListByDisk` uses the reference index.

### Integrity
- An attached disk cannot be deleted. An instance in a GPU cluster blocks deletion of the cluster.
- `ListInstancesByNVLInstanceGroup` and `BatchGet` (up to the documented max IDs; missing IDs are omitted) are index queries.

### Permissions and quotas
- Permissions: `compute.instances.*` (+ `start`, `stop`), `compute.disks.*`, `compute.diskSnapshots.*`, `compute.filesystems.*`,
  `compute.images.*`, `compute.gpuClusters.*`, `compute.nvlInstanceGroups.*`, `compute.platforms.get/list`.
- Quotas: `compute.instance.count`, `compute.vcpu`, `compute.memory`, `compute.gpu.<gpu_count_quota_type>`,
  `compute.disk.count`, `compute.disk.size.<type>`, `compute.filesystem.size.<type>`. vCPU/memory/GPU are charged while
  the instance is not `STOPPED`, which mirrors on-demand accounting. That is configurable.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_compute_v1_disk` | `nebius.compute.v1.DiskService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_compute_v1_disk_snapshot` | `nebius.compute.v1.DiskSnapshotService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_compute_v1_filesystem` | `nebius.compute.v1.FilesystemService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_compute_v1_gpu_cluster` | `nebius.compute.v1.GpuClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_compute_v1_instance` | `nebius.compute.v1.InstanceService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_compute_v1_nvl_instance_group` | `nebius.compute.v1.NVLInstanceGroupService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T2 |
| Data source | `nebius_compute_v1_disk` | `nebius.compute.v1.DiskService` | Get, GetByName | T2 |
| Data source | `nebius_compute_v1_disk_snapshot` | `nebius.compute.v1.DiskSnapshotService` | Get, GetByName | T2 |
| Data source | `nebius_compute_v1_filesystem` | `nebius.compute.v1.FilesystemService` | Get, GetByName | T2 |
| Data source | `nebius_compute_v1_gpu_cluster` | `nebius.compute.v1.GpuClusterService` | Get, GetByName | T2 |
| Data source | `nebius_compute_v1_image` | `nebius.compute.v1.ImageService` | Get, GetByName | T2 |
| Data source | `nebius_compute_v1_instance` | `nebius.compute.v1.InstanceService` | Get, GetByName | T2 |
| Data source | `nebius_compute_v1_nvl_instance_group` | `nebius.compute.v1.NVLInstanceGroupService` | Get, GetByName | T2 |

`nebius_compute_v1_image` is a data source only. It calls `Get` / `GetByName`, so public images need stable IDs and names,
plus status fields such as `image_family_human_readable` and `image_family_deprecation`. Family resolution
(`GetLatestByFamily`) is used **server-side**, when a disk or managed disk specifies `source_image_family`. The
upstream example `examples/resources/nebius_compute_v1_instance` (managed boot disk from `image_family = "ubuntu22.04"`)
must work verbatim, so the default image catalog has to include whatever families the upstream examples use.

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE` | built-in | Platform/preset catalog |
| `NEBIUS_MOCKER_COMPUTE_IMAGES_FILE` | built-in | Public image catalog |
| `NEBIUS_MOCKER_COMPUTE_CHARGE_STOPPED` | `0` | Count vCPU/GPU quota for stopped instances |
| `NEBIUS_MOCKER_COMPUTE_BACKEND` | `mock` | (later) `container` to run instances as containers with cloud-init |

## Open questions
- Exact preset names per platform and the GPU quota type names.
- Whether resizing a managed boot disk through the instance spec is allowed in place.
- Instance `UPDATING` vs. a direct transition for label-only updates.
- `BatchGet` limits and behavior on missing IDs.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.compute.v1.DiskService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/disk_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/disk_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetDiskRequest` | `Disk` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Disk` | ✅ |
| `List` | `ListDisksRequest` | `ListDisksResponse` |  |
| `Create` | `CreateDiskRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateDiskRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteDiskRequest` | `common.v1.Operation` | ✅ |
| `ListOperationsByParent` | `ListOperationsByParentRequest` | `common.v1.ListOperationsResponse` |  |

#### `nebius.compute.v1.DiskSnapshotService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/disk_snapshot_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/disk_snapshot_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetDiskSnapshotRequest` | `DiskSnapshot` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `DiskSnapshot` | ✅ |
| `List` | `ListDiskSnapshotsRequest` | `ListDiskSnapshotsResponse` |  |
| `ListByDisk` | `ListDiskSnapshotsByDiskRequest` | `ListDiskSnapshotsByDiskResponse` |  |
| `Create` | `CreateDiskSnapshotRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateDiskSnapshotRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteDiskSnapshotRequest` | `common.v1.Operation` | ✅ |

#### `nebius.compute.v1.FilesystemService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/filesystem_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/filesystem_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetFilesystemRequest` | `Filesystem` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Filesystem` | ✅ |
| `List` | `ListFilesystemsRequest` | `ListFilesystemsResponse` |  |
| `Create` | `CreateFilesystemRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateFilesystemRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteFilesystemRequest` | `common.v1.Operation` | ✅ |
| `ListOperationsByParent` | `ListOperationsByParentRequest` | `common.v1.ListOperationsResponse` |  |

#### `nebius.compute.v1.GpuClusterService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/gpu_cluster_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/gpu_cluster_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetGpuClusterRequest` | `GpuCluster` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `GpuCluster` | ✅ |
| `List` | `ListGpuClustersRequest` | `ListGpuClustersResponse` |  |
| `Create` | `CreateGpuClusterRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateGpuClusterRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteGpuClusterRequest` | `common.v1.Operation` | ✅ |
| `ListOperationsByParent` | `ListOperationsByParentRequest` | `common.v1.ListOperationsResponse` |  |

#### `nebius.compute.v1.ImageService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/image_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/image_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetImageRequest` | `Image` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Image` | ✅ |
| `GetLatestByFamily` | `GetImageLatestByFamilyRequest` | `Image` |  |
| `List` | `ListImagesRequest` | `ListImagesResponse` |  |
| `Create` | `CreateImageRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateImageRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteImageRequest` | `common.v1.Operation` | ✅ |
| `ListOperationsByParent` | `ListOperationsByParentRequest` | `common.v1.ListOperationsResponse` |  |
| `ListPublic` | `ListPublicRequest` | `ListImagesResponse` |  |

#### `nebius.compute.v1.InstanceService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/instance_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/instance_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetInstanceRequest` | `Instance` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Instance` | ✅ |
| `BatchGet` | `BatchGetRequest` | `BatchGetResponse` |  |
| `List` | `ListInstancesRequest` | `ListInstancesResponse` |  |
| `ListInstancesByNVLInstanceGroup` | `ListInstancesByNVLInstanceGroupRequest` | `ListInstancesResponse` |  |
| `Create` | `CreateInstanceRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateInstanceRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteInstanceRequest` | `common.v1.Operation` | ✅ |
| `Start` | `StartInstanceRequest` | `common.v1.Operation` |  |
| `Stop` | `StopInstanceRequest` | `common.v1.Operation` |  |
| `ListOperationsByParent` | `ListOperationsByParentRequest` | `common.v1.ListOperationsResponse` |  |

#### `nebius.compute.v1.MaintenanceService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/maintenance_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/maintenance_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `GetByInstance` | `GetMaintenanceEventByInstanceRequest` | `MaintenanceEvent` |  |
| `ListActive` | `ListMaintenanceEventsRequest` | `ListMaintenanceEventsResponse` |  |

#### `nebius.compute.v1.NVLInstanceGroupService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/nvlinstancegroup_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/nvlinstancegroup_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateNVLInstanceGroupRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetNVLInstanceGroupRequest` | `NVLInstanceGroup` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `NVLInstanceGroup` | ✅ |
| `List` | `ListNVLInstanceGroupsRequest` | `ListNVLInstanceGroupsResponse` |  |
| `Update` | `UpdateNVLInstanceGroupRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteNVLInstanceGroupRequest` | `common.v1.Operation` | ✅ |

#### `nebius.compute.v1.NodeService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/node_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/node_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `SetUnhealthy` | `NodeSetUnhealthyRequest` | `NodeSetUnhealthyResponse` |  |

#### `nebius.compute.v1.PlatformService`

Endpoint: `compute.api.nebius.cloud:443` · Proto: [`nebius/compute/v1/platform_service.proto`](https://github.com/nebius/api/blob/main/nebius/compute/v1/platform_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `GetByName` | `common.v1.GetByNameRequest` | `Platform` |  |
| `List` | `ListPlatformsRequest` | `ListPlatformsResponse` |  |
