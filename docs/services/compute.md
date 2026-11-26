# Compute

> Module: `services/compute` · Image: `nebius-mocker-compute` · `NEBIUS_MOCKER_SERVICES=compute`
> M1 tier: **T2** · Endpoint: `compute.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.compute.v1.ImageService`: Get, GetByName, GetLatestByFamily, List (paged), ListPublic (per region, paged), Create, Update (reset mask, `resource_version`, immutable fields), Delete, ListOperationsByParent | ✅ T2 |
| Public image catalog per region in `project-<routing code>public-images`, seeded from a built-in or file catalog, payloads in the object store | ✅ |
| Custom images imported from Object Storage (`spec.source_storage`), payload copied into the object store; size, checksum, format and minimum disk size derived from the bytes | ✅ |
| `spec.source_disk_id` / `spec.source_disk_snapshot_id` | ✅ The source must exist, be `READY` and be in the image's project. `min_disk_size_bytes` = disk `size_bytes` (snapshot: its source disk's size if that still exists, else `content_size_bytes`), rounded up to a GiB. `cpu_architecture` defaults to the source's. The payload is a 64 KiB qcow2 declaring that size (disks keep no data). While the image is `CREATING` the source disk carries it in `status.lock_state.images` (disk.proto: "locked for deletion and for read-write operations while image is being created"), so the disk cannot be deleted; the lock is released when the image becomes `READY` or `ERROR`, and on image delete. Snapshot sources are not locked (`DiskSnapshot.lock_state` only lists disks). Tests: domain, contract, SDK and CLI on RustFS, Terraform (`nebius_compute_v1_disk` resource + disk and image data sources) |
| `nebius.compute.v1.DiskService`: Get, GetByName, List (paged; deprecated `filter` rejected), Create, Update (reset mask, `resource_version`, grow-only size, immutable type/block size/source/encryption), Delete, ListOperationsByParent | ✅ T2 (control plane only: no storage is allocated, nothing is attached) |
| `nebius.compute.v1.DiskSnapshotService`: Get, GetByName, List, ListByDisk (paged), Create, Update, Delete | ✅ T2 (no data is copied) |
| Disk/snapshot authorization: `compute.disks.*`, `compute.disksnapshots.*` on the project; `ListByDisk` checks `compute.disksnapshots.list` on the disk's project | ✅ |
| `nebius_compute_v1_disk`, `nebius_compute_v1_disk_snapshot` resources and data sources with the official Terraform provider | ✅ e2e: `test/terraform/compute/disk_test.go` (`make e2e`) |
| `nebius compute disk …` / `nebius compute disk-snapshot …` CLI flows from the storage docs | ✅ e2e: `test/computedisk` (`make e2e`, no Docker) |
| Authorization: public images readable by any authenticated principal; custom images by project roles (`compute.images.*`) | ✅ |
| `nebius_compute_v1_image` data source (the provider has no image resource) with the official Terraform provider | ✅ e2e: public images in `test/terraform/compute` (`make e2e`); custom images in the same package under `make e2e-images` |
| `nebius compute image …` CLI flows (list-public, get, get-by-name, get-latest-by-family, create from a bucket, list, update, list-operations-by-parent, delete) on RustFS | ✅ e2e: `test/compute` (`make e2e-images`) |
| `nebius.compute.v1.GpuClusterService`: Get, GetByName, List (paged; deprecated `filter` rejected), Create (fabric checked against the catalog and the project's region), Update (reset mask, `resource_version`; name and labels mutable, `infiniband_fabric` immutable), Delete (`FAILED_PRECONDITION` while `status.instances` is non-empty), ListOperationsByParent | ✅ T2 (inventory only: no GPUs, fabric or topology are simulated) |
| GPU cluster membership port (`domain.GpuClusterMembership`: Join/Leave) for InstanceService `spec.gpu_cluster`: same project, fabric accepts the instance's platform, synthetic deterministic `infiniband_topology_path`; Join and cluster Delete are atomic against each other | ✅ (domain + DAL tests; reachable over the API once InstanceService serves `gpu_cluster`) |
| GPU cluster authorization: `compute.gpuClusters.*` on the project | ✅ |
| `nebius_compute_v1_gpu_cluster` resource and data source with the official Terraform provider | ✅ e2e: `test/terraform/compute/gpucluster_test.go` (`make e2e`) |
| `nebius compute gpu-cluster …` CLI flows | ✅ e2e: `test/computegpucluster` (`make e2e`, no Docker) |
| `nebius.compute.v1.NVLInstanceGroupService`: Get, GetByName, List (paged), Create (`spec.type` GB200/GB300 required, `spec.size` > 0), Update (reset mask, `resource_version`; name and labels mutable, `type` and `size` immutable), Delete (`FAILED_PRECONDITION` while `status.instances` is non-empty) | ✅ T2 (inventory only: no NVLink domain is simulated; see [NVL instance groups](#nvl-instance-groups)) |
| NVL membership port (`domain.NVLMembership`: Join/Leave/Members) for InstanceService `spec.nvl_instance_group_id`: same project, platform of the group's type, at most `spec.size` members; Join and group Delete are atomic against each other. `status.instances[id].instance_state` is read from the instances | ✅ domain, DAL, contract (`test/contract/compute_nvl_test.go`, `compute_instance_nvl_test.go`) |
| NVL instance group authorization: `compute.nvlInstanceGroups.*` on the project; `ListInstancesByNVLInstanceGroup` needs `compute.instances.list` on the group's project | ✅ |
| `nebius_compute_v1_nvl_instance_group` resource and data source, and instances in a group, with the official Terraform provider | ✅ e2e: `test/terraform/compute/nvl_test.go` (`make e2e`) |
| `nebius compute nvl-instance-group …` CLI flows | ✅ e2e: `test/computenvl` (`make e2e`, no Docker) |
| `nebius.compute.v1.InstanceService`: Get, GetByName, BatchGet (per-ID authorization, error entries), List (paged), ListInstancesByNVLInstanceGroup (members in creation order, paged), Create, Update (reset mask, `resource_version`, immutable fields, stop/start by `spec.stopped`, preset change while stopped), Delete, Start, Stop, ListOperationsByParent | ✅ T2 (management plane only: nothing boots; see [Instances](#instances-implemented)) |
| Instance disks: managed boot/secondary disks created and deleted with the instance (`status.managed_by`), existing disks attached with `read_write_attachment` / `read_only_attachments` and protected from deletion, existing ↔ managed switches | ✅ |
| Instance network interfaces: VPC allocations assigned through `peer.NetworkInterfaces` (dynamic or user allocations, public and alias addresses, `status.assignment`, released on delete), default security group, MAC and FQDN in status | ✅ |
| Instances in GPU clusters (`spec.gpu_cluster`, `allow_gpu_clustering` presets, `status.infiniband_topology_path`; cluster delete refused while it has instances) | ✅ contract (`test/contract/compute_instance_gpucluster_test.go`) and Terraform |
| Instances in NVL instance groups (`spec.nvl_instance_group_id` on `gpu-gb200` / `gpu-gb300`, immutable; `recovery_policy = ALWAYS` only in a group; group delete refused while it has instances) | ✅ domain, contract and Terraform |
| Instances with shared filesystems (`spec.filesystems` through `FilesystemAttachments`; adding needs STOPPED, unique `mount_tag`, READ_WRITE only; removal and instance delete detach) | ✅ domain unit tests, contract (`test/contract/compute_instance_filesystem_test.go`), Terraform e2e (`test/terraform/compute/instance_filesystem_test.go`) |
| Instances on capacity reservations (`spec.reservation_policy` `AUTO`/`FORBID`/`STRICT`, `reservation_ids` = capacity block group IDs, `status.reservation_id`; drawn on create/start through `peer.CapacityReservations`, held while stopped, released on delete) | ✅ domain, contract (`test/contract/compute_instance_reservation_test.go`), Terraform (`test/terraform/compute/reservation_test.go`), CLI (`test/computeinstance/reservation_e2e_test.go`) |
| `nebius.compute.v1.PlatformService`: GetByName, List (paged) from a per-region catalog (`NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE`) | ✅ T2 |
| Instance/platform authorization: `compute.instances.{get,list,create,update,delete,start,stop}`, `compute.platforms.{get,list}` on the project | ✅ |
| `nebius_compute_v1_instance` resource and data source (CPU and GPU platforms, GPU cluster) with the official Terraform provider | ✅ e2e: `test/terraform/compute/instance_test.go` (`make e2e`) |
| `nebius compute instance …` / `nebius compute platform list` CLI flows | ✅ e2e: `test/computeinstance` (`make e2e`, no Docker) |
| `nebius.compute.v1.FilesystemService`: Get, GetByName, List (paged; deprecated `filter` rejected), Create, Update (reset mask, `resource_version`, grow-only hot resize; name, type and block size immutable), Delete (`FAILED_PRECONDITION` while `forbid_deletion`, attached to instances or exposed by filesystem buckets), ListOperationsByParent | ✅ T2, with a real data plane: every filesystem is an object-store bucket (RustFS) |
| Filesystem attachment port (`domain.FilesystemAttachments`: Attach/Detach) for InstanceService `spec.filesystems`: exists, same project, `READY`, `READ_WRITE` only; recorded in `status.read_write_attachments` in the same store transaction as the checks, so Attach and filesystem Delete never both win | ✅ (domain + DAL tests incl. an Attach-vs-Delete race; used by InstanceService) |
| Mounting: Object Storage **filesystem buckets** (`storage.v1` `spec.filesystem_bucket`) expose a filesystem through the S3 front; users mount them with rclone (FUSE). Writes beyond the filesystem size fail | ✅ e2e: `test/filesystems` (`make e2e-filesystems`, Docker: RustFS + `rclone/rclone`) |
| `nebius_compute_v1_filesystem` resource + data source (with a `nebius_storage_v1_bucket` filesystem bucket in the same config) with the official Terraform provider | ✅ e2e: `test/terraform/compute/filesystem_test.go` (`make e2e`) |
| `nebius compute filesystem create/get/get-by-name/list/update/delete` CLI flows | ✅ e2e: `test/computefilesystem` (`make e2e`, no Docker) |
| Maintenance, nodes | Planned |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100 (filesystems: RustFS 1.0.6, rclone 1.71.2, nebius CLI 0.12.312).

Tests: domain unit tests with fakes (`services/compute/domain`, incl. a fuzzed payload inspector and the disk size/type/state rules), DAL tests on the
memory store, the blob adapter on the memory object store, gosdk contract tests (`test/contract/compute_image_test.go`,
memory object store, in `make check`; disks and snapshots in `test/contract/compute_disk*_test.go`), the Terraform suites, and the same gosdk scenarios plus the nebius
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
- No quota accounting for image storage (the public API cannot create images).
- Disks and snapshots: no `ERROR`/`BROKEN` states,
  because nothing can fail without a data plane.
- Instances: nothing boots or runs (no VM, no container, cloud-init is never executed); no `ERROR` state is ever
  reached; no
  capacity simulation (`NotEnoughResources`, `RESOURCE_EXHAUSTED`); no preemption, maintenance events
  (`MaintenanceService`, `status.maintenance_event_id`) or host recovery (so `recovery_policy = ALWAYS` has nothing to
  retry); `NodeService`, spot pricing policies and local disk passthrough are not implemented (their spec fields are
  rejected by name, see above). Capacity reservations are implemented (see Scheduling fields), but reserved intervals
  that end while an instance holds GPUs do not evict or stop it.
- Platform presets, GPU memory, quota type names and region availability are illustrative.
- Filesystems:
  - no VM mounts (virtiofs with the `mount_tag`): there are no VMs; mounts go through filesystem buckets and rclone;
  - no IOPS/throughput limits, no inode or max-file-size limits, no encryption settings (always on upstream);
  - `READ_ONLY` attachments (upstream supports only `READ_WRITE`);
  - filesystem buckets: only the root `directory` and default `uid`/`gid`/`file_mode`/`directory_mode` (POSIX
    ownership and permissions are not emulated), and only in the filesystem's project. Versioning, lifecycle rules,
    object tagging and anonymous access are refused for filesystem buckets, as the Nebius S3 compatibility page lists
    them as unsupported; checksums and ETags behave like regular buckets (RustFS);
  - `NETWORK_HDD`, `WEKA`, `VAST` types (undocumented limits).
- NVL instance groups: no NVLink domain, rack or GPU is simulated, and no capacity: a group of any `size` can be
  created. Nebius does not publish the GB200/GB300 platform and preset names, so `gpu-gb200` / `gpu-gb300` with preset
  `4gpu-128vcpu-1600gb` (one compute tray: 4 GPUs) and their regions are **illustrative**; replace them with
  `NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE` (`nvl_instance_group_type`). Assumptions where the docs are silent: `spec.type`
  is required (`UNSPECIFIED` is `INVALID_ARGUMENT`), `spec.nvl_instance_group_id` is immutable on an instance (like
  `gpu_cluster`, placement is decided at creation), instances of NVL platforms may also run outside a group, and a
  full group is `FAILED_PRECONDITION`. mk8s node groups with `template.nvlink` are still rejected by the mk8s module.
- GPU clusters: no fabric or topology simulation (`infiniband_topology_path` is synthetic), no capacity modeling. The exact upstream error for an instance whose platform does not match
  the cluster's fabric is undocumented; the mock answers `INVALID_ARGUMENT` on `spec.gpu_cluster.id`.

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
- **Platforms** (`PlatformService.GetByName/List`, implemented): the catalog of a project's region. The region is the
  project's `spec.region` (from IAM, `peer.ProjectRegions`); without IAM, the NID routing code. A platform that is not
  offered in that region is `NOT_FOUND` on `GetByName`, absent from `List`, and `INVALID_ARGUMENT` on
  `spec.resources.platform` when an instance asks for it. Names and presets follow docs `compute/virtual-machines/types`:
  `cpu-e2`, `cpu-d3` (160 vCPU and up not in eu-north1), `gpu-h100-sxm`, `gpu-h200-sxm`, `gpu-b200-sxm`,
  `gpu-b200-sxm-a`, `gpu-b300-sxm`, `gpu-gb200` and `gpu-gb300` (illustrative names, see
  [NVL instance groups](#nvl-instance-groups)), `gpu-rtx6000`, `gpu-rtx6000-a`, `gpu-l40s-a`, `gpu-l40s-d`, presets like
  `2vcpu-8gb` and `8gpu-128vcpu-1600gb`. Only the 8-GPU presets of H100/H200/B200/B300 have `allow_gpu_clustering`;
  GPU platforms have `status.allowed_for_preemptibles`; every platform has `allow_preset_change`. GPU memory, quota
  type names and region lists are **illustrative**. Platform IDs are stable per (region, name); `metadata.parent_id`
  is the requested project. The built-in catalog is `services/compute/domain/platforms.yaml`; format of
  `NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE` (YAML or JSON, replaces it):

  ```yaml
  platforms:
    - name: gpu-h100-sxm
      human_readable_name: NVIDIA H100 NVLink with Intel Sapphire Rapids
      short_human_readable_name: H100
      gpu_count_quota_type: gpu-h100
      gpu_memory_gigabytes: 80
      allow_preset_change: true
      allowed_for_preemptibles: true
      nvl_instance_group_type: ""          # GB200 or GB300: instances may join NVL groups of that type
      regions: [eu-north1]                 # empty: every configured region
      presets:
        - { name: 8gpu-128vcpu-1600gb, vcpu_count: 128, memory_gibibytes: 1600, gpu_count: 8, allow_gpu_clustering: true }
        - { name: 1gpu-16vcpu-200gb, vcpu_count: 16, memory_gibibytes: 200, gpu_count: 1, exclude_regions: [] }
  ```
- **Public images**: a catalog under `project-<routing>public-images` with families (`ubuntu22.04-driverless`,
  `ubuntu22.04-cuda12`, `ubuntu24.04-driverless`, `mk8s-worker-node-…`). Every family has at least one version.
  `GetLatestByFamily` returns the newest. `ListPublic` lists the catalog. Each image has a CPU architecture, a minimum disk
  size, and a status of `READY`. Implemented; see [Image storage design](#image-storage-design). The built-in catalog is
  `services/compute/domain/catalog.yaml`. The upstream Terraform example uses `image_family = "ubuntu22.04"`, which is
  not a real Nebius family (docs `compute/storage/boot-disk-images`), so the mock answers `NOT_FOUND` for it, as the
  cloud would; no alias is seeded. The example works verbatim with real IDs and a real family (e.g.
  `ubuntu24.04-driverless`), which `test/terraform/compute` proves.

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

### Instances (implemented)

Management plane only: an instance is a record with a lifecycle, disk attachments and VPC addresses; nothing boots.
Rules come from docs `compute/virtual-machines/{lifecycle,update,fqdn,network,preemptible,reservations,delete}` and
`compute/storage/{use,detach-volume}`, and from the proto comments. The spec is echoed exactly as sent (the provider
marks no spec attribute computed); everything synthesized lives in `status`.

- **Lifecycle**: Create → `CREATING` → `RUNNING`, or `STOPPED` with `spec.stopped`. `Stop` (`RUNNING` → `STOPPING` →
  `STOPPED`) and `Start` (`STOPPED` → `STARTING` → `RUNNING`) are refused in other states (`FAILED_PRECONDITION`); they
  do not change `spec.stopped`. On Update, toggling `spec.stopped` stops or starts the instance; other spec changes run
  as `UPDATING`; a metadata-only change has no transition. Only `RUNNING`/`STOPPED` instances can be updated, and
  `RUNNING`/`STOPPED`/`ERROR` ones deleted. `NEBIUS_MOCKER_OP_DELAY` makes the transitional states observable.
- **Resources**: platform and preset must be offered in the project's region (`INVALID_ARGUMENT` on
  `spec.resources.platform` / `.preset`). The platform is immutable. The preset can change only while the instance is
  `STOPPED` (`FAILED_PRECONDITION` otherwise) and the platform has `allow_preset_change`.
- **Immutable** (`INVALID_ARGUMENT` "field is immutable"): `service_account_id`, `resources.platform`, `gpu_cluster`,
  `recovery_policy`, `preemptible` (docs: it cannot be added or removed), and per interface (matched by name)
  `subnet_id` and `ip_address`. `service_account_id` must be an existing service account of the instance's project.
- **Disks**: the boot disk and `secondary_disks` are `existing_disk{id}` or `managed_disk{name, labels, spec}`.
  - A managed disk is a real `computedisk` created with the DiskService rules (type, size, source image/family/snapshot),
    `READY` at once (no data to copy), with `status.managed_by` = the instance. A name already used by a disk in the
    project fails with `ALREADY_EXISTS`. Managed disks are matched by name on update: a new name creates a disk, a
    removed or renamed one is deleted (renaming replaces, as the proto says). Size may grow, `forbid_deletion` and labels
    may change; type, block size, source and encryption are immutable. DiskService.Update of a managed disk is
    `FAILED_PRECONDITION` ("Updates via DiskService are not allowed").
  - An existing disk must be in the project and `READY`, and not managed by another instance. `READ_WRITE` needs no
    other attachment; `READ_ONLY` needs no read-write attachment elsewhere (`FAILED_PRECONDITION`). The disk's
    `status.read_write_attachment` / `read_only_attachments` record the instance, so it cannot be deleted while attached.
  - Switching `existing_disk` → `managed_disk` of the same disk needs the disk's name and its spec copied exactly
    (`INVALID_ARGUMENT` otherwise) and no `forbid_deletion`. Switching back (by the ID in `status.disk_attachments`)
    clears `managed_by`.
  - Omitting a secondary disk detaches it (existing) or deletes it (managed), in any state. The boot disk cannot be
    detached or replaced (`INVALID_ARGUMENT` on `spec.boot_disk`). `device_id` and disk IDs are unique per instance.
  - `status.disk_attachments` lists `{id, name, is_managed}`, boot disk first.
- **Network interfaces** (1–8, unique names): addresses are VPC allocations, assigned through `peer.NetworkInterfaces`
  (provided by vpc) with `status.assignment.network_interface{instance_id, name, type PRIMARY|PUBLIC|ALIAS}`, state
  `ASSIGNED`. `ip_address = {}` creates a dynamic allocation (lowest free address in the subnet, named
  `<instance>-<nic>-private`, `status.static=false`); `ip_address.allocation_id` binds a user allocation (same project,
  in the subnet's space, unassigned). `public_ip_address = {}` is a dynamic public address. Per docs
  `compute/virtual-machines/network` it is returned to the range once the instance has been `STOPPED` for more than
  one hour: until then the stopped instance keeps it (status shows it, the allocation stays `ASSIGNED`) and a Start
  reuses it; after that it is released (the allocation is deleted, status has no public address) and the next Start
  assigns a new one. The period is `NEBIUS_MOCKER_COMPUTE_PUBLIC_IP_RELEASE_AFTER` (default `1h`), counted on the mock
  clock from when the instance settles in `STOPPED` (updates while stopped do not restart it); a sweep releases due
  addresses every minute of that clock, and a Start or Update after the period releases them first. `static = true`
  keeps it across stops; an `allocation_id` binds a user allocation. Aliases bind
  user allocations. Security groups must belong to the subnet's network; an empty list means the network's default
  group, shown in status only. Delete releases everything: dynamic allocations are deleted, user allocations become
  `ALLOCATED` again and survive. Interfaces may be added or removed on update.
- **Status**: per interface `ip_address`, `public_ip_address{address, allocation_id, static}`, `aliases.cidrs`, a stable
  locally administered MAC `02:4e:42:xx:xx:xx` (from the instance and interface), `fqdn`
  `<hostname or instance ID>.<network ID>.compute.internal.` (docs `compute/virtual-machines/fqdn`; a hostname change
  updates it) and the effective `security_groups`.
- **GPU clusters**: `spec.gpu_cluster.id` needs a preset with `allow_gpu_clustering` (`INVALID_ARGUMENT` on
  `spec.gpu_cluster.id`); Create joins the cluster (`GpuClusterMembership`: same project, fabric accepts the platform)
  and records `status.infiniband_topology_path`; Delete leaves it.
- **Filesystems**: `existing_filesystem.id` with a `mount_tag` unique per instance; attached on Create
  (`FilesystemAttachments`: same project, `READY`, `READ_WRITE` only). Adding one on Update needs the instance
  `STOPPED` (docs `compute/storage/use`); removing one, or deleting the instance, detaches it (never deletes it).
- **Scheduling fields**: `preemptible` needs a platform with `allowed_for_preemptibles` and `recovery_policy = FAIL`
  (docs); `on_preemption` is `STOP`. Nothing is ever preempted (no capacity simulation). `on_demand` /
  `follows_spot_price` are echoed (no billing). `recovery_policy` `RECOVER`/`FAIL` are echoed (hosts never fail).
- **Capacity reservations** (`reservation_policy`, docs `compute/virtual-machines/reservations`;
  `domain/instance_reservation.go`): `reservation_ids` are capacity block group IDs (services/capacity), tried in order.
  GPUs are drawn through `peer.CapacityReservations`, which enforces the group's current limit and the project's
  capacity allowance, and shows up in the group's `status.usage` and `ListResources`.
  - `AUTO` (default) draws from a reservation that fits (the named ones, else any active matching group of the tenant:
    region, platform, and the GPU cluster's fabric) and otherwise runs in the common pool. `FORBID` uses only the pool;
    `reservation_ids` with `FORBID` is `INVALID_ARGUMENT`. `STRICT` fails the request with `RESOURCE_EXHAUSTED` when no
    reservation fits (`FAILED_PRECONDITION` when the capacity module is disabled).
  - An unknown ID is `NOT_FOUND` and one for another platform, region or fabric is `INVALID_ARGUMENT` on
    `spec.reservation_policy.reservation_ids[i]`, under `AUTO` too: the docs only describe falling back for exhausted or
    inactive reservations.
  - Only regular GPU instances use reservations: `STRICT` or `reservation_ids` on a CPU or preemptible instance is
    `INVALID_ARGUMENT`. Quantity is the preset's GPU count.
  - Drawn on Create and on Start (also an Update that starts the instance) when the instance holds none. Held while
    `STOPPED` (docs: reserved GPUs "remain available, even if a VM is stopped"), released on Delete and on a failed
    Create. `status.reservation_id` is the block group drawn from (empty: the common pool).
  - `reservation_policy` can change only while `STOPPED` (`FAILED_PRECONDITION` otherwise, docs: stop the VM first);
    changing it or the preset drops the held reservation, and the next start draws again.
- **NVL instance groups**: `spec.nvl_instance_group_id` joins the group on Create (`NVLMembership`: same project, the
  platform's `nvl_instance_group_type` equals the group's `spec.type`, fewer than `spec.size` members, else
  `NOT_FOUND` / `INVALID_ARGUMENT` on `spec.nvl_instance_group_id` / `FAILED_PRECONDITION`); Delete leaves it. The field
  is immutable on Update. `recovery_policy = ALWAYS` is `INVALID_ARGUMENT` without a group (instance.proto: "Available
  only for instances in nvlinstancegroup").
- **Rejected by name** (`INVALID_ARGUMENT`, "… is not implemented in NebiusMocker yet"):
  `spot_pricing_policy`, `local_disks.passthrough_group.requested = true`.
- `cloud_init_user_data` (sensitive) is stored and echoed, redacted in audit events, and never executed.
- `BatchGet` (up to 1000 IDs) authorizes each ID (`compute.instances.get`) and returns a `google.rpc.Status`
  (`NOT_FOUND`, `PERMISSION_DENIED`) in place of each unreadable instance. `ListInstancesByNVLInstanceGroup` lists a
  group's members in creation order (`NOT_FOUND` for an unknown group). `ListOperationsByParent` lists a project's instance operations,
  newest first, including deleted instances.
- Ordering: NIC addresses are bound first (VPC), then the GPU cluster, the NVL group and filesystems, then the capacity
  reservation, then disks and the instance in one store transaction; a failure undoes the earlier steps. The operations engine serializes per instance.

#### Managed node instances (mk8s)

The nodes of an mk8s node group are ordinary instances in the cluster's project that the node group owns. The mk8s
module creates and deletes them through the in-process capability `peer.ComputeNodes` (provided by this module, no RPC
and no IAM check: mk8s authorized its own request):

- `CreateNode` builds the instance spec a node template implies (preset, managed boot disk `<node>-boot` from an image
  family, one NIC `eth0` with a dynamic private address and optionally a dynamic public one, service account, GPU
  cluster, filesystems, cloud-init, preemptible) and creates it RUNNING at once, with no operation: the node group's
  operation is the visible one. It is idempotent per (node group, node name). Labels (the node-group and cluster labels
  are set by mk8s) are the instance labels. Ownership is a store index (`computenode`: instance to node group and
  cluster).
- `CheckTemplate` runs the same validation as `Create` without storing anything and reports violations under the mk8s
  NodeTemplate's field names (`resources.preset`, `boot_disk.size_bytes`, `network_interfaces[0].subnet_id`,
  `service_account_id`, `gpu_cluster.id`, `filesystems[i].existing_filesystem.id`). A READ_ONLY filesystem is rejected
  (`filesystems[i].attach_mode`): compute attaches READ_WRITE only. Whether the GPU cluster or filesystem exists is
  checked when a node is created, not by `CheckTemplate`.
- `DeleteNode` removes the instance with its boot disk, addresses, GPU cluster and filesystem attachments; a missing
  instance is not an error.
- Reads (`Get`, `GetByName`, `List`, `BatchGet`) are unchanged. `Update`, `Delete`, `Start` and `Stop` of a node fail
  with `FAILED_PRECONDITION` before an operation is created: `instance <id> is a node of mk8s node group <node-group-id>
  (cluster <cluster-id>) and is managed by it: change the node group instead`.

### Disks and snapshots (implemented)

Management APIs only: a disk is a record with a lifecycle. No storage is allocated. Instances set
`status.read_write_attachment` / `read_only_attachments` / `managed_by` (see [Instances](#instances-implemented)). Rules come from docs `compute/storage/types`, `compute/storage/manage` and
`compute/storage/disk-snapshots`, and from the proto comments:

- **Types**: `NETWORK_SSD` 1–8192 GiB; `NETWORK_SSD_NON_REPLICATED` and `NETWORK_SSD_IO_M3` 93–262,074 GiB in multiples
  of 93 GiB. `NETWORK_HDD` is in the enum, but the docs list no HDD disk type, so it is rejected with `INVALID_ARGUMENT`
  ("not available") rather than given invented limits. `UNSPECIFIED` is `INVALID_ARGUMENT`.
- **Size**: exactly one of `size_bytes|kibibytes|mebibytes|gibibytes`, normalized into `status.size_bytes`. The spec is
  echoed as sent, so Terraform round-trips without drift. Sizes are multiples of 4 MiB and of the block size. A disk
  holds at most 2^32 blocks (16 TiB with 4 KiB blocks). Boot disks (from an image) are at most 30,720 GiB. Size only
  grows on Update (`INVALID_ARGUMENT` otherwise). A grow runs as `UPDATING` → `READY`.
- **Block size**: a power of two from 4 KiB to 128 KiB. It is immutable. If unset, `spec.block_size_bytes` stays 0 and
  `status.block_size_bytes` is 4096.
- **Encryption**: `DISK_ENCRYPTION_MANAGED` is accepted for NRD and IO M3 only (proto). Network SSD is always encrypted
  by the platform, so `MANAGED` on it is `INVALID_ARGUMENT`. The setting is stored, echoed and immutable.
- **Sources** (immutable):
  - `source_image_id` must exist (`NOT_FOUND`), be `READY` (`FAILED_PRECONDITION`) and be public or in the disk's
    project (`INVALID_ARGUMENT`).
  - `source_image_family` resolves through `ImageService.GetLatestByFamily` (default parent: the first region's
    public-images project). A deprecated family is `FAILED_PRECONDITION`.
  - `source_snapshot_id` must exist, be in the same project (docs: snapshots are usable only within their project) and
    be `READY`.
  - The size must be at least the image's `min_disk_size_bytes` or the snapshot's `content_size_bytes`.
  - `status.source_image_id` and `status.source_image_cpu_architecture` record the image used.
  - While a disk is `CREATING` from a snapshot, the snapshot's `status.lock_state.disks` holds the disk, and the
    snapshot can't be deleted.
- **Delete**: `FAILED_PRECONDITION` with `disk cannot be deleted because forbid_deletion is set` (the docs' wording), or
  while the disk is attached, managed by an instance, or locked (`status.lock_state.images|snapshots`). Deleting a disk
  that has snapshots is **allowed**, and the snapshots stay `READY` with a stale `source_disk_id`. The proto says this
  ID "may become stale if the disk is deleted", and the docs describe no cascade or block.
- **forbid_deletion** can be changed only on a standalone disk (docs). A disk with `managed_by` is `FAILED_PRECONDITION`.
- **Snapshots**:
  - `source_disk_id` must exist (`NOT_FOUND`) and be `READY` (`FAILED_PRECONDITION`).
  - `parent_id` must be the disk's project (docs). A tenant parent is therefore `INVALID_ARGUMENT`.
  - While `CREATING`, the disk's `status.lock_state.snapshots` holds the snapshot, and the disk can't be deleted.
  - Status: `content_size_bytes = storage_size_bytes =` the disk's `status.size_bytes` (no incremental snapshots), and
    `source_cpu_architecture` comes from the disk. The proto has no type field to copy.
  - Name, labels and `description` (≤ 1024 characters) are mutable.
  - `ListByDisk` needs an existing disk and lists its snapshots in the disk's project.
- Cross-resource checks and locks run in one store transaction (`Mutate` in the DAL), so they don't race with a
  concurrent delete. The operations engine only serializes per resource ID.
- `DiskReader` (`domain/ports.go`, `dal.NewDiskReader`) gives images read access for image-from-disk/snapshot.

### Shared filesystems (implemented)

Rules from https://docs.nebius.com/compute/storage/types and /compute/storage/manage (read 2026-11-25):
- **Types**: only `NETWORK_SSD` is documented. `NETWORK_HDD`, `WEKA` and `VAST` are in the proto enum ("the list of
  available types will be clarified later") but have no documented limits, so they are `INVALID_ARGUMENT` ("not
  available") rather than given invented ones.
- **Size**: 1 GiB to 100 PiB (104,857,600 GiB), any unit of the `size` oneof. The docs give no size multiple, so none is
  enforced. `status.size_bytes` is the normalized size. Sizes only grow; a grow is a hot resize (`UPDATING`, then
  `READY` with the new `status.size_bytes`).
- **Block size**: a power of two from 4 KiB to 128 KiB, default 4096 in `status.block_size_bytes`; `spec` echoes what
  was sent (no drift).
- **Immutable**: type and block size (Terraform replaces), and the name (docs: "cannot be changed"; an Update with a
  different name is `INVALID_ARGUMENT`). Labels and `forbid_deletion` are mutable.
- **Delete** is `FAILED_PRECONDITION` while `forbid_deletion` is set, while instances are attached
  (`status.read_write_attachments`), and while an Object Storage filesystem bucket exposes it (the docs don't say what
  upstream does; refusing keeps the bucket from silently pointing at nothing). The operation then purges every file.
  Both references are atomic with deletion: bucket Create records itself on the filesystem (`Expose`, store kind
  `computefilesystem.exposure`) in the same compute store transaction as the filesystem's READY/project checks, and
  Delete reads those records in the transaction that marks the filesystem `DELETING`, so a racing bucket Create and
  filesystem Delete never both succeed (DAL race tests, like Attach vs Delete). A bucket whose record cannot be stored
  is unexposed again; bucket delete unexposes.
- **Attachments** (docs: compute/storage/use): one filesystem attaches to many VMs of its project, `READ_WRITE` only.
  InstanceService checks that the VM is stopped and that `mount_tag` is unique per VM, then calls
  `FilesystemAttachments.Attach`. Instance delete detaches.

#### Data plane design

- Each filesystem is backed by its own internal object-store bucket `nebius-mocker-fs-<filesystem-id>` (the
  `nebius-mocker-` prefix is reserved for internal buckets; `services/compute/backend/fsstore`). The Create operation
  provisions it in its `Work` (a failure leaves the filesystem `ERROR` with `state_description`); the Delete operation
  removes it with every file. A file is one object keyed by its path. With `NEBIUS_MOCKER_OBJSTORE=rustfs` this is
  RustFS; the default memory store keeps the same semantics for Docker-free tests.
- **Access path**: Nebius exposes a filesystem over S3 with **filesystem buckets**
  (https://docs.nebius.com/object-storage/buckets/filesystem-buckets: "the Object Storage interface to an existing
  shared filesystem", used with "AWS CLI, rclone, or s5cmd"). The mocker implements exactly that: a `storage.v1`
  bucket with `spec.filesystem_bucket.filesystem_id` is a view of the filesystem's store bucket. The S3 front
  (`:4580`) authenticates and authorizes it like any bucket (SigV4 with an iam.v2 access key, `storage.*` permits), then
  relays to the store bucket; response bodies name the Nebius bucket and report the `FILESYSTEM` storage class.
  Storage reaches compute only through `peer.FilesystemsKey` (`pkg/peer/filesystems.go`: `Filesystem`, `Expose`,
  `Unexpose`); compute never calls storage.
- **Capacity**: every write that adds bytes (PutObject, CopyObject, UploadPart, UploadPartCopy) is refused with S3
  `QuotaExceeded` when the stored bytes of the filesystem (all files plus in-flight multipart parts, shared by every
  bucket of the filesystem) plus the request would exceed `status.size_bytes`. An overwrite counts only the size
  difference, and deletes free space immediately (the store bucket is never versioned). Concurrent writes are not
  serialized, so the limit can be overshot by in-flight requests, as for `max_size_bytes`.
- Deleting a filesystem bucket is immediate (no delayed deletion or restore) and keeps the files; a new filesystem
  bucket on the same filesystem shows them.

#### Mounting a shared filesystem (user guide)

There are no VMs, so you mount the filesystem from your machine or a container with rclone, through a filesystem
bucket. The mocker must run with `NEBIUS_MOCKER_OBJSTORE=rustfs` (the S3 front needs RustFS).

1. Create the filesystem and a filesystem bucket (Terraform, or `nebius compute filesystem create --parent-id <project>
   --name shared --type network_ssd --size-gibibytes 1` and `nebius storage bucket create --parent-id <project> --name
   shared-fs --filesystem-bucket-filesystem-id <filesystem-id>`):

   ```hcl
   resource "nebius_compute_v1_filesystem" "shared" {
     name             = "shared"
     type             = "NETWORK_SSD"
     size_gibibytes   = 1
     block_size_bytes = 4096
   }

   resource "nebius_storage_v1_bucket" "shared" {
     name              = "shared-fs"
     filesystem_bucket = { filesystem_id = nebius_compute_v1_filesystem.shared.id }
   }
   ```

   Only the filesystem root with default ownership can be exposed: `directory` may be empty or `"/"`, `uid` and `gid`
   0, `file_mode` empty or `"644"`, `directory_mode` empty or `"755"` (other values are `INVALID_ARGUMENT`, see Not
   yet). The filesystem must be `READY` and in the bucket's project, and the caller needs
   `compute.filesystems.useViaObjectStorage` on it. The roles are Nebius's (IAM roles reference: `editor`, `admin` and
   `compute.use-via-object-storage`, "access data stored on shared filesystems through filesystem buckets in Object
   Storage", grant "Use through Object Storage"); the permission identifier is mock-specific, since the docs list
   only role names and permission labels.
2. Create a service account in the project's `editors` group and an access key (`nebius iam service-account create`,
   `nebius iam group-membership create --parent-id <editors-group-id> --member-id <sa-id>`, `nebius iam v2 access-key
   create --parent-id <project> --account-service-account-id <sa-id>`), and read the secret with `nebius iam v2
   access-key get-secret --id <key-id>` (or `secret_delivery_mode = "EXPLICIT"` in Terraform and the
   `nebius_iam_v2_access_key_secret` ephemeral resource).
3. Configure rclone (`~/.config/rclone/rclone.conf`; in the official image `/config/rclone/rclone.conf`):

   ```ini
   [nebius]
   type = s3
   provider = Other
   access_key_id = <aws_access_key_id>
   secret_access_key = <secret>
   endpoint = http://localhost:4580          # from a container on Docker Desktop: http://host.docker.internal:4580
   region = eu-north1
   force_path_style = true
   ```

4. Mount:
   - in a container (any host with Docker; this is what `make e2e-filesystems` runs):
     `docker run --rm -it --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor:unconfined -v
     ~/.config/rclone:/config/rclone --entrypoint sh rclone/rclone:1.71.2 -c 'mkdir -p /mnt/fs && rclone mount
     nebius:shared-fs /mnt/fs --daemon && sh'`. Unmount with `umount /mnt/fs` before the container exits so pending
     writes finish;
   - on a Linux host with FUSE: `rclone mount nebius:shared-fs /mnt/fs` (same remote; not covered by the e2e suite);
   - on macOS, rclone documents `rclone nfsmount` as the variant that needs no macFUSE (not covered by the e2e suite).

   Files written through any mount (or with `aws s3 cp` to the bucket) are visible to every other mount. A write that
   would exceed the filesystem size fails (`QuotaExceeded` from the S3 front; an I/O error in the mount).

### GPU clusters (implemented)

Management plane only (docs: compute/clusters/gpu). A cluster holds `spec.infiniband_fabric` and the IDs of its member
instances; nothing is allocated.
- **Create**: `spec.infiniband_fabric` is required and must be a catalog fabric in the region of the parent project
  (`peer.ProjectRegions`, provided by IAM). Unknown or other-region fabrics → `INVALID_ARGUMENT` on
  `spec.infiniband_fabric`, listing the fabrics of the project's region. The spec is echoed exactly as sent;
  `status.reconciling` is true until the operation completes.
- **Update**: name and labels change in place (reset-mask semantics, `resource_version` checked and bumped). A different
  `infiniband_fabric` → `INVALID_ARGUMENT` ("field is immutable"); an unset one keeps the stored fabric. Terraform
  replaces the cluster on a fabric change (`RequiresReplace`). Status is server-owned and kept.
- **Membership**: instances join only at creation through `GpuClusterMembership.Join` (InstanceService). The cluster
  must exist (`NOT_FOUND`), be in the instance's project, and its fabric must accept the instance's platform (one fabric
  = one platform) — otherwise `INVALID_ARGUMENT` on `spec.gpu_cluster.id`. Join appends to `status.instances` and
  records a deterministic path `[<fabric>, <fabric>-spine-NN, <fabric>-leaf-NNN]` (derived from the instance ID) in
  `status.infiniband_topology_path`. Leave (instance delete) is idempotent; the topology is cleared with the last member.
  The "8-GPU presets only" rule is checked by InstanceService against the platform catalog.
- **Delete**: `FAILED_PRECONDITION` while `status.instances` is non-empty ("deleted or moved to another cluster"). The
  check and the removal run in one store transaction, so a Join racing a Delete either blocks it or fails with
  `NOT_FOUND`.

Fabric catalog file (`NEBIUS_MOCKER_COMPUTE_FABRICS_FILE`, YAML or JSON; it replaces the built-in
`services/compute/domain/fabrics.yaml`, taken from the public docs' fabric table on 2026-11-25). Unknown keys are
rejected. Fabrics of regions the mocker does not emulate are kept but unusable:

```yaml
fabrics:
  - name: fabric-3          # spec.infiniband_fabric
    region: eu-north1       # only projects of this region can use it
    platforms: [gpu-h100-sxm]  # platforms whose instances may join
```

### NVL instance groups

Management plane only (`nebius.compute.v1.NVLInstanceGroupService`; proto: "NVLink InstanceGroup", `spec.type`
"corresponds to the Compute platform", `spec.size` "maximum number of instances"). A group is a GB200 or GB300 NVL72
NVLink domain; nothing is allocated.
- **Create**: `spec.type` must be `GB200` or `GB300` and `spec.size` > 0 (`INVALID_ARGUMENT` otherwise). The spec is
  echoed exactly as sent; `status.reconciling` is true until the operation completes.
- **Update**: name and labels change in place (reset-mask semantics, `resource_version` checked and bumped); a
  different `type` or `size` → `INVALID_ARGUMENT` ("field is immutable"), an unset one keeps the stored value. The
  Terraform provider replaces the group on any change (`name`, `labels`, `type` and `size` are `RequiresReplace`).
- **Membership**: an instance joins at creation through `NVLMembership.Join` when `spec.nvl_instance_group_id` is set.
  The group must exist (`NOT_FOUND`) and be in the instance's project, and the instance's platform must have the
  group's type (`nvl_instance_group_type` in the platform catalog: `gpu-gb200` → `GB200`, `gpu-gb300` → `GB300`),
  otherwise `INVALID_ARGUMENT` on `spec.nvl_instance_group_id`. A group with `size` members is full
  (`FAILED_PRECONDITION`). Instance delete leaves the group.
- **Status**: `status.instances` maps each member ID to its current `instance_state`, read from the instance on every
  Get/GetByName/List; `status.reconciling` is also true while any member is in a transitional state.
- **Delete**: `FAILED_PRECONDITION` while the group has instances; the check and the removal run in one store
  transaction, so a racing Join either blocks it or fails with `NOT_FOUND`.
- `InstanceService.ListInstancesByNVLInstanceGroup` lists the members in creation order (paged), authorized with
  `compute.instances.list` on the group's project.

### Integrity
- An attached disk cannot be deleted. An instance in a GPU cluster or an NVL instance group blocks deletion of the
  cluster or group.
- `ListInstancesByNVLInstanceGroup` reads the group's member list; `BatchGet` takes up to the documented max IDs.

### Permissions and quotas
- Permissions: `compute.instances.*` (+ `start`, `stop`), `compute.disks.*`, `compute.disksnapshots.*`, `compute.filesystems.*` (implemented; plus `compute.filesystems.useViaObjectStorage` for filesystem buckets),
  `compute.images.*`, `compute.gpuClusters.*`, `compute.nvlInstanceGroups.*`, `compute.platforms.get/list`.
- Quotas (enforced, see [quotas.md](quotas.md#catalog)), charged to the project from creation until deletion, running
  or stopped (the docs: "VMs count toward quota from creation to deletion"):
  - instances: `compute.instance.count` (or `compute.instance.preemptible.count`), plus
    `compute.instance.non-gpu.vcpu` = preset vCPUs on non-GPU platforms, or `compute.instance.gpu.<family>` = preset GPUs
    on GPU platforms for regular VMs (`gpu-h100-sxm` → `h100`). Preset changes claim the delta. mk8s nodes and NVL group
    members are ordinary instances and count the same way;
  - disks, including managed boot disks: `compute.disk.count` and `compute.disk.size.<type>` (bytes); resizes claim the
    growth;
  - snapshots: `compute.disk.snapshot.count` and `compute.disk.snapshot.size` (the source disk's size);
  - filesystems: `compute.filesystem.count` and `compute.filesystem.size.<type>`; resizes claim the growth;
  - GPU clusters: `compute.gpucluster.count`.

  The check runs in each create's or update's synchronous step, so an exceeded quota is the RPC's `RESOURCE_EXHAUSTED`.
  There is no memory quota (Nebius documents none). GPUs drawn from a capacity reservation (`status.reservation_id`
  set) are not charged to `compute.instance.gpu.<family>` (docs: "GPUs allocated from reservations do not count towards
  quotas on the number of GPUs"); the reservation is decided before the quota claim, and a refused claim releases it.
  The instance and its disks are still charged. Changing the policy of a stopped reserved VM so that it leaves the
  reservation claims its GPUs.

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
| `NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE` | built-in (`services/compute/domain/platforms.yaml`) | Platform/preset catalog file (YAML/JSON, format above); replaces the built-in one. **Implemented** |
| `NEBIUS_MOCKER_COMPUTE_IMAGES_FILE` | built-in (`services/compute/domain/catalog.yaml`) | Public image catalog file (YAML/JSON, format above); replaces the built-in one. **Implemented** |
| `NEBIUS_MOCKER_COMPUTE_FABRICS_FILE` | built-in (`services/compute/domain/fabrics.yaml`) | InfiniBand fabric catalog of GPU clusters (YAML/JSON, [format](#gpu-clusters-implemented)); replaces the built-in one. **Implemented** |
| `NEBIUS_MOCKER_REGIONS` | `eu-north1=e00,eu-west1=e01,us-central1=u00,me-west1=m00` | Regions with a public image catalog (`ListPublic --region`); the first is the `GetLatestByFamily` default. **Implemented** |
| `NEBIUS_MOCKER_OBJSTORE` (+ `_ENDPOINT`, `_ACCESS_KEY`, `_SECRET_KEY`, `_REGION`) | `memory` | Where image payloads live: `memory` or `rustfs` (shared with Object Storage; see [storage.md](storage.md)) |
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
