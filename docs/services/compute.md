# Compute

> Module: `services/compute` · Image: `nebius-mocker-compute` · `NEBIUS_MOCKER_SERVICES=compute`
> M1 tier: **T2** · Endpoint: `compute.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.compute.v1.ImageService`: Get, GetByName, GetLatestByFamily, List (paged), ListPublic (per region, paged), Create, Update (reset mask, `resource_version`, immutable fields), Delete, ListOperationsByParent | ✅ T2 |
| Public image catalog per region in `project-<routing code>public-images`, seeded from a built-in or file catalog, payloads in the object store | ✅ |
| Custom images imported from Object Storage (`spec.source_storage`), payload copied into the object store; size, checksum, format and minimum disk size derived from the bytes | ✅ |
| `spec.source_disk_id` / `spec.source_disk_snapshot_id` | Deferred: disks and snapshots are not implemented, so every disk/snapshot ID is `NOT_FOUND` |
| Authorization: public images readable by any authenticated principal; custom images by project roles (`compute.images.*`) | ✅ |
| `nebius_compute_v1_image` data source (the provider has no image resource) with the official Terraform provider | ✅ e2e: public images in `test/terraform/compute` (`make e2e`); custom images in the same package under `make e2e-images` |
| `nebius compute image …` CLI flows (list-public, get, get-by-name, get-latest-by-family, create from a bucket, list, update, list-operations-by-parent, delete) on RustFS | ✅ e2e: `test/compute` (`make e2e-images`) |
| Instances, disks, snapshots, filesystems, GPU clusters, NVL groups, platforms, maintenance, nodes | Planned |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100.

Tests: domain unit tests with fakes (`services/compute/domain`, incl. a fuzzed payload inspector), DAL tests on the
memory store, the blob adapter on the memory object store, gosdk contract tests (`test/contract/compute_image_test.go`,
memory object store, in `make check`), the Terraform data-source suites, and the same gosdk scenarios plus the nebius
CLI against RustFS (`make e2e-images`, Docker).

The e2e suites put image files in the way users do (docs: compute/storage/import-image, object-storage/quickstart,
object-storage/objects/upload-download); they never write into the object store, only read it to verify payloads:
- CLI (`TestCLICustomImage`): `nebius storage bucket create --name`, then `nebius iam service-account create`, `nebius
  iam group get-by-name --name editors`, `nebius iam group-membership create`, `nebius iam v2 access-key create
  --account-service-account-id`, `nebius iam v2 access-key get` (secret), then the AWS CLI configured with `aws
  configure set aws_access_key_id/aws_secret_access_key/region/endpoint_url` and `aws s3 cp`, then `nebius compute image
  create --source-storage-bucket-name … --source-storage-object-name …` (`test/internal/awscli` sandboxes `~/.aws`).
- Go SDK and Terraform: the bucket via `BucketService.Create`, an access key via `iam.v2 AccessKeyService`, the upload via
  the AWS SDK for Go v2 against the S3 front (`test/internal/s3upload`), then `ImageService.Create`.

### Image storage design

- **Metadata** (the `Image` proto) lives in `pkg/store` through `services/compute/dal`, like every other resource.
- **Payloads** live in the shared object store (`pkg/objstore`: RustFS with `NEBIUS_MOCKER_OBJSTORE=rustfs`, memory by
  default), bucket `nebius-mocker-compute-images` (the `nebius-mocker-` prefix is reserved for internal buckets), key
  `images/<image-id>`. Each object carries user metadata `sha256`, `format` (`qcow2|vmdk|vhd|raw`), `virtual-size`,
  `image-id` and, for imports, `source` (`s3://bucket/object`). The adapter is `services/compute/backend/blobstore`; the
  domain only sees its `Blobs` port.
- **Import** (`Create` with `source_storage`): `Prepare` checks that the source object exists (otherwise
  `INVALID_ARGUMENT` on `spec.source_storage.object_name`) and stores the image as `CREATING` (`reconciling=true`). The
  operation's `Work` reads the source once to hash and inspect it, then streams it into the image bucket with that
  metadata while hashing again; a source that changed in between fails the import. The image becomes `READY` with
  `storage_size_bytes` = payload size and `min_disk_size_bytes` = virtual size (from the qcow2/vmdk/vhd header; the byte
  size for raw) rounded up to a GiB. A failed import marks the image `ERROR` with `state_description` and fails the
  operation. User buckets map 1:1 to object-store buckets, so an object uploaded through Object Storage (or straight to
  RustFS) is importable. Delete removes the payload, then the record.
- **Public catalog**: for every region in `NEBIUS_MOCKER_REGIONS` and every family version, a `READY` image named
  `<family>-<version>` in `project-<routing code>public-images` (the upstream default parent of `GetLatestByFamily`
  is `project-{region}public-images`). IDs are derived from (routing code, family, version), so they are stable across
  restarts. The payload is a small (32 KiB) deterministic synthetic qcow2 whose header declares the family's minimum disk
  size; it is a real object with a real checksum. Seeding is idempotent: blobs with the right checksum are kept.
  Public images have no `source`. They cannot be updated or deleted (`PERMISSION_DENIED`), and nobody can create images
  in a public-images project.
- **Families**: `GetLatestByFamily` picks, among `READY` images of the family in the parent (default: the first
  region's public project), the highest `version` (ties: newest `created_at`) whose
  `status.image_family_deprecation.deprecated_at` is not in the past. If every version is deprecated it returns the
  newest one, with its deprecation status, so callers can see the family is deprecated. Custom images may set
  `image_family`/`version`; a (family, version) pair is unique per project (`ALREADY_EXISTS`).
- **Immutability**: `image_family`, `version`, `cpu_architecture`, `image_family_human_readable` and the source are
  `IMMUTABLE`. An Update that leaves them empty keeps them; one that changes them is `INVALID_ARGUMENT`. `cpu_architecture`
  defaults to `AMD64` on Create.
- **Partial updates**: the official CLI's `update` sends only the changed fields with an empty reset mask, so the shared
  validation interceptor skips presence rules (`required`, required oneofs) on `…/Update` RPCs; value rules still apply.
- `List.filter` is deprecated upstream ("not implemented") and rejected with `INVALID_ARGUMENT` naming `filter` when set (never silently ignored). `ListOperationsByParent` lists operations of a
  project's images, newest first, including images deleted since (an index kept by the DAL).
- Operations with `NEBIUS_MOCKER_OP_DELAY` run their `Work` on the engine's single scheduler goroutine, so a large
  import delays other operations. Payloads in tests are small.

### Not yet / approximations

- Catalog contents (versions, labels, sizes, dates, recommended platforms) are **illustrative**. The family names follow
  the docs; the real catalog should come from parity recording.
- The routing codes of regions other than `eu-north1` (`e00`) are placeholders.
- Payloads are never booted or converted; only their headers are read.
- No quota accounting for image storage.

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
  size, and a status of `READY`. Implemented; see [Image storage design](#image-storage-design). The built-in catalog is
  `services/compute/domain/catalog.yaml`. The upstream Terraform examples use `image_family = "ubuntu22.04"`, which is
  not a documented family and is not in the catalog yet (it matters once disks exist).

  Catalog file format (`NEBIUS_MOCKER_COMPUTE_IMAGES_FILE`, YAML or JSON; it replaces the built-in catalog):

  ```yaml
  families:
    - name: acme-os                     # spec.image_family
      human_readable: Acme OS           # spec.image_family_human_readable
      description: Acme OS base image
      cpu_architecture: AMD64           # or ARM64; default AMD64
      min_disk_size_gibibytes: 20       # becomes status.min_disk_size_bytes
      labels: { os_name: Acme }
      recommended_platforms: [cpu-e2]
      unsupported_platforms: { gpu-l40s-a: "no drivers" }
      regions: [eu-north1]              # default: every region
      deprecation: { at: 2026-01-01T00:00:00Z, message: "use acme-os2" }   # whole family
      versions:
        - version: "20250101"           # created_at = this date when it is YYYYMMDD
        - version: "20250601"
          released_at: 2025-06-01T12:00:00Z
          deprecation: { at: 2025-07-01T00:00:00Z, message: "bad build" } # this version only
  ```

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
| `NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE` | built-in | (later) Platform/preset catalog |
| `NEBIUS_MOCKER_COMPUTE_IMAGES_FILE` | built-in (`services/compute/domain/catalog.yaml`) | Public image catalog file (YAML/JSON, format above); replaces the built-in one. **Implemented** |
| `NEBIUS_MOCKER_REGIONS` | `eu-north1=e00,eu-west1=e01,us-central1=u00,me-west1=m00` | Regions with a public image catalog (`ListPublic --region`); the first is the `GetLatestByFamily` default. **Implemented** |
| `NEBIUS_MOCKER_OBJSTORE` (+ `_ENDPOINT`, `_ACCESS_KEY`, `_SECRET_KEY`, `_REGION`) | `memory` | Where image payloads live: `memory` or `rustfs` (shared with Object Storage; see [storage.md](storage.md)) |
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
