# mk8s — Managed Service for Kubernetes

> Module: `services/mk8s` · Image: `nebius-mocker-mk8s` · `NEBIUS_MOCKER_SERVICES=mk8s`
> M1 tier: **T2** (`mock` backend) · **T3**: `k3s` backend (Phase B, in progress) · Endpoint: `mk8s.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.mk8s.v1.ClusterService`: Create, Get, GetByName, List (paged), Update (reset mask, `resource_version`), Delete, ListControlPlaneVersions | ✅ T2 |
| `nebius.mk8s.v1.NodeGroupService`: Create, Get, GetByName, List (paged), Update (reset mask, `resource_version`), Delete, Upgrade, PreflightCheck, GetCompatibilityMatrix | ✅ T2 |
| `nebius.mk8s.v1alpha1.ClusterService` and `NodeGroupService` (deprecated): every RPC, as a second view of the same stored v1 resources (same IDs; `common.v1alpha1` operations) | ✅ T2 |
| Node instances: every node of a node group is an ordinary compute instance (`RUNNING`, boot disk named `<node>-boot`), visible in `InstanceService.Get`/`List`; user updates, start, stop and delete are refused with `FAILED_PRECONDITION` | ✅ |
| Backend port (`domain.Backend` in `services/mk8s/domain/ports.go`); `mock` is the default | ✅ |
| Control-plane version catalog, node group version skew, node image compatibility matrix | ✅ |
| `NodeGroupService.PreflightCheck` (create, update, recreate, delete; `unknown_paths`; `paths_require_recreate`) | ✅ |
| Defaults filled like the service (control-plane subnet, etcd size, node boot disk, network interface subnet, deployment strategy) | ✅ |
| Status counts (`node_count`, `ready_node_count`, `target_node_count`, `outdated_node_count`), not-ready nodes reported honestly | ✅ |
| Cluster delete cascades: node groups and their node instances, Applications releases, the VPC network binding | ✅ |
| `nebius mk8s cluster get-credentials` kubeconfig and the `get-token` exec plugin | ✅ e2e: `test/cli/mk8s` (with the mock backend nothing serves the Kubernetes API) |
| Authorization: rules for every RPC (`mk8s.clusters.*`, `mk8s.nodeGroups.*`); viewers read clusters, node groups and node instances | ✅ |
| `nebius_mk8s_v1_{cluster,node_group}` and `nebius_mk8s_v1alpha1_{cluster,node_group}` resources and data sources with the official provider | ✅ e2e: `test/terraform/mk8s` |
| A working Kubernetes API (`kubectl`, Helm, the Kubernetes provider) | `k3s` backend, Phase B |
| Rolling node replacement in batches that honour `strategy` (`max_surge`, `max_unavailable`), progress in `status` | ✅ |
| Quotas, audit logs, Karpenter, OIDC, NVLink, reservations, local disks, spot, auto-repair conditions, `max_pods`, container runtime | Not yet (see [Not yet](#not-yet)) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/mk8s/{domain,dal,api,backend/mock}`, `services/compute/domain/nodes_test.go`), gosdk
contract tests (`test/contract/mk8s_*`), the Terraform e2e suite
(`test/terraform/mk8s`) and the CLI e2e suite (`test/cli/mk8s`), all run by `make e2e`.

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Cluster | `mk8s.v1.Cluster` | `mk8scluster` | project | `PROVISIONING`, `RUNNING`, `DELETING` (+ `reconciling`) |
| Node group | `mk8s.v1.NodeGroup` | `mk8snodegroup` | cluster | `PROVISIONING`, `RUNNING`, `DELETING` (+ `reconciling`) |

### Behavior notes

These follow [docs.nebius.com/kubernetes](https://docs.nebius.com/kubernetes) and the protos. Where neither says what
happens, the mock's choice is marked *(mock)*.

#### Clusters

- **Create** is an operation: the cluster is stored `PROVISIONING` (`reconciling = true`) and the backend creates the
  control plane; it then becomes `RUNNING` with `status.control_plane.endpoints`, the cluster CA in
  `auth.cluster_ca_certificate` and `version` (`<major>.<minor>.<patch>-nebius-cp.1`). A failed backend call leaves the
  cluster `PROVISIONING` with an `ERROR` event and fails the operation.
- **Defaults** (the service fills them, so `terraform plan` stays empty): `control_plane.subnet_id` is the project's
  default subnet when unset (`INVALID_ARGUMENT` if the project has none); `etcd_cluster_size` 3; `version` the newest
  catalog version. The subnet must exist and be in the cluster's project. The cluster binds its VPC network, so the
  network cannot be deleted under it; the binding is released at cluster delete even when the subnet was deleted first.
  `subnet_id` and `kube_network.service_cidrs` are immutable (an empty value on update keeps the stored one).
- **Service CIDRs.** A `kube_network.service_cidrs` CIDR that overlaps the private CIDRs of the cluster's VPC network (its
  private pools and the subnet's own) is `INVALID_ARGUMENT` naming `spec.kube_network.service_cidrs[0]`. A bare prefix
  length (`/16`, the default) is allocated by the service and cannot overlap.
- **Versions.** `control_plane.version` must be in the catalog (`NEBIUS_MOCKER_MK8S_VERSIONS`, default `1.34,1.35,1.36`,
  the versions the Nebius docs list; `ListControlPlaneVersions` returns the catalog). An update can keep the version or
  raise it by one minor; skipping a minor or a downgrade is `INVALID_ARGUMENT`. The backend must also support the version
  (`Backend.CheckVersion`).
- **Endpoints** *(mock)*: with the `mock` backend the private endpoint is
  `https://<cluster-id>.private.mk8s.mocker.localhost` and, when `endpoints.public_endpoint` is set, the public one is
  `https://<cluster-id>.mk8s.mocker.localhost`. `.localhost` names resolve to the loopback address and **nothing
  listens**: use `NEBIUS_MOCKER_MK8S_BACKEND=k3s` (Phase B, in progress) for a working Kubernetes API. The CA is a
  per-cluster self-signed certificate, stable for the process. The real endpoint and CA formats are undocumented.
- **Delete** sets `DELETING`, then deletes every node group (and its node instances), the cluster's Applications releases,
  the backend control plane and the network binding.
- **Update** with nothing changed keeps `resource_version` and `updated_at`.

#### Node groups

- **Create** needs a `RUNNING` cluster (`FAILED_PRECONDITION` otherwise) and a name unique in the cluster. The group is
  stored `PROVISIONING`; the operation creates the node instances, hands them to the backend and sets `RUNNING`.
  `target_node_count` is `fixed_node_count`, or `autoscaling.initial_node_count` (default `min_node_count`).
  `node_count`, `ready_node_count` and `outdated_node_count` follow what exists and what the backend reports ready.
- **Defaults**: boot disk `NETWORK_SSD`, 128 GiB; one network interface in the cluster's subnet; `os` `ubuntu24.04`;
  `version` the cluster's minor with the latest node image (`-nebius-node.1`); strategy `max_unavailable` 1, `max_surge` 0,
  `drain_timeout` 10m, reported in `status.strategy` (both 0 is `INVALID_ARGUMENT`).
- **Validation.** `template.resources` must be a platform and preset of the compute catalog; the (platform, OS, drivers
  preset) triple must be a node image of the compatibility matrix; node subnets must be in the cluster's network; boot
  disk, security groups, filesystems, GPU cluster and service account go through the compute instance checks, with
  violations renamed to the node template's field names.
- **Version skew.** A node group version must be in the catalog, not newer than the cluster and at most 2 minors behind
  it (`spec.version` accepts `1.35` or `1.35-nebius-node.1`; node image numbers above 1 do not exist). A cluster upgrade
  does not move a node group whose `spec.version` is empty. `GetCompatibilityMatrix(cluster_kubernetes_version[, platform])`
  returns the node group versions allowed for that cluster version and their images: `ubuntu24.04` and `ubuntu22.04`
  driverless on CPU and GPU platforms, `cuda12` on GPU platforms, `cuda12.8` on Blackwell platforms *(mock: the images are
  the mock's reading of the docs, the same for every minor)*.
- **Update** scales the group (the fixed count, or the current count clamped to the autoscaling bounds) and **rolls the
  nodes out** when the template or the version changes. `taints` and `metadata` (the Kubernetes node labels) do not roll
  out, as the proto says. `Upgrade` moves the group to the latest node image and the cluster's patch version, rolling its
  nodes out; it does nothing when the group is already there.
- **Rollouts** replace the outdated nodes in batches, like a Kubernetes rolling update, per `status.strategy`:
  up to `max_surge` new nodes above the target are added first (a percentage rounds up), and old nodes are removed
  while at least target − `max_unavailable` nodes are ready (a percentage rounds down; both 0 after rounding allows one
  unavailable node). Each batch creates the new compute instances, hands the node set to the backend, takes the removed
  nodes out of the backend and only then deletes their instances, and records `node_count`, `ready_node_count` and
  `outdated_node_count` (counting down; `reconciling` is true). Batches are `NEBIUS_MOCKER_MK8S_ROLLOUT_STEP` apart on the
  mocker's clock (default 0: one after the other); with a step, the operation runs detached and finishes with the last
  batch. A rollout that can neither add nor remove a node (new nodes never get ready) fails after ten checks, leaving the
  counts as they are. `NodeGroupStatus` has no progress tracker field; the counts are the progress.
- **Draining** (`drain_timeout`): removed nodes leave the backend's node set before their instances are deleted, and
  `BackendNodeGroup.DrainTimeout` carries the effective `status.strategy.drain_timeout` (0: no limit), so a Kubernetes
  backend drains them inside `EnsureNodes`, bounded by it. The `mock` backend runs no pods and has nothing to drain.
- **Delete** sets `DELETING`, deletes the node instances and empties the group in the backend.

#### Node instances

Each node is a compute instance named `<node group name>-<5 characters>`, in the cluster's project, created `RUNNING`
without a visible operation, with a managed boot disk. Labels *(mock names: the real label keys are undocumented)*:

| Label | Value |
|---|---|
| `mk8s.nebius.ai/cluster-id` | cluster ID |
| `mk8s.nebius.ai/node-group-id` | node group ID |
| `mk8s.nebius.ai/node-group-name` | node group name |

They win over `template.instance_metadata.labels`. The same three labels go on the Kubernetes Nodes, with
`template.metadata.labels` (keys containing `kubernetes.io` or `k8s.io` are dropped, per the proto). Users cannot Update,
Start, Stop or Delete a node instance: those fail with `FAILED_PRECONDITION`, "… is a node of mk8s node group …: change
the node group instead".

#### Credentials

`nebius mk8s cluster get-credentials --id <id> --external|--internal` writes the kubeconfig Nebius writes: cluster,
context and user named `nebius-mk8s-<cluster name>-<ID suffix>` (the cluster ID without `mk8scluster-`), `server` the public
(`--external`) or private (`--internal`) endpoint verbatim, the CA embedded, and an exec plugin
(`client.authentication.k8s.io/v1beta1`) that runs `nebius mk8s v1 cluster get-token --profile <profile> --format json`.
`get-token` returns an IAM token from the mock. With the mock backend `kubectl` cannot connect: the endpoint resolves to
loopback and nothing listens. `test/cli/mk8s` checks the kubeconfig and runs the exec command as `kubectl` would.

#### Backends

`domain.Backend` (`services/mk8s/domain/ports.go`) is the Kubernetes data plane: `CheckVersion`, `EnsureCluster`,
`DeleteCluster`, `EnsureNodes`, `Close`. The control plane (resources, validation, operations, node instances) is the
mock's; a backend only runs Kubernetes. Calls come from operation work, never inside a store transaction, one at a time
per cluster; every method is idempotent. `EnsureNodes` reports each node's readiness: a node the backend cannot run is
**not ready, with a reason**, instead of an error, so the group honestly shows `node_count > ready_node_count`,
`reconciling = true` and a `WARN` event `NodesNotReady`. A failed reconciliation records an `ERROR` event
(`ReconciliationFailed`) and fails the operation.

| `NEBIUS_MOCKER_MK8S_BACKEND` | Behavior |
|---|---|
| `mock` (default) | No Kubernetes. Endpoints that resolve to loopback, a self-signed CA, every node ready at once. Operations do their work inside the request |
| `k3s` | Accepted by the configuration, **not available yet**: the module fails to start with `backend "k3s" is not available` until Phase B lands |

Every backend but `mock` starts containers, so its operations run **detached**: Create, Update and Delete return the
operation at once and the work finishes on its own goroutine (`pkg/operations`, `Detached`) while clients poll it.

#### Preflight check

`NodeGroupService.PreflightCheck` runs the validation of Create or Update in dry-run mode and returns **diagnostics**
(`ERROR`, with the field path) instead of failing, whatever the problem (unknown cluster, bad version, unknown platform,
unimplemented field, duplicate name). `context.action` is `CREATE`, `RECREATE`, `UPDATE` or `DELETE` (nothing to check);
unspecified means `UPDATE` when `metadata.id` is set, else `CREATE`. `context.unknown_paths` (a Nebius mask) skips the
checks of values Terraform does not know yet. An `UPDATE` that changes an immutable path (`metadata.parent_id`,
`template.nvlink.nvl_instance_group_id`) returns it in `paths_require_recreate` and is evaluated as a recreate. An update
preflight is a patch with the request's reset mask, like the Update it previews. The provider calls it on every plan.

#### Fields rejected as not implemented

A field the mock cannot honor is `INVALID_ARGUMENT`, "… is not implemented in NebiusMocker yet", naming the path, on
Create, Update and PreflightCheck. Nothing is silently ignored:

- Cluster: `spec.control_plane.audit_logs`, `spec.control_plane.karpenter`, `spec.control_plane.authentication.oidc`.
- Node group, under `spec.template.`: `nvlink`, `reservation_policy` (anything but `AUTO` with no reservation IDs),
  `local_disks`, `spot_pricing_policy`, `follows_spot_price`, `container_runtime.soci_pull_mode`, `max_pods`,
  `gpu_settings.dra`; and `spec.auto_repair.conditions`.

#### Operations and errors

- `common.v1.Operation` for v1, `common.v1alpha1.Operation` for v1alpha1. Operations complete at once (or after
  `NEBIUS_MOCKER_OP_DELAY`); only a detached backend defers the work.
- An Update whose `resource_version` does not match is `FAILED_PRECONDITION`, and so is updating or upgrading something
  that is being deleted.
- v1alpha1 is a field-number-compatible subset of v1, so requests are converted by a protobuf round trip and no input
  field is dropped. v1-only output fields are absent from v1alpha1 responses.

#### Authorization

Every RPC has a rule: `mk8s.clusters.{get,list,create,update,delete}` on the cluster (or its project) and
`mk8s.nodeGroups.*` on the node group (or its cluster). `NodeGroupService.Upgrade` needs `mk8s.nodeGroups.update`.
`ListControlPlaneVersions` and `GetCompatibilityMatrix` are open to any authenticated principal, and so is a
`PreflightCheck` with neither an ID nor a parent (Terraform planning a node group whose cluster does not exist yet);
otherwise it needs `mk8s.nodeGroups.get` on the node group or its cluster. v1alpha1 uses the same permissions. `viewer`
reads clusters, node groups, the catalog, preflight results and the node instances, and is `PERMISSION_DENIED` on every
write (`test/contract/mk8s_authz_test.go`).

### Not yet

- Quotas (`mk8s.cluster.count`, `mk8s.nodegroup.count`) and node-level compute quotas.
- Audit logs, Karpenter and OIDC authentication; NVLink, capacity reservations, local disks, spot pricing, SOCI pull mode,
  `max_pods` and DRA (all rejected, see above).
- Auto-repair: the setting is stored, but nodes never fail in the mock, so nothing is repaired (like compute's
  `recovery_policy`); `auto_repair.conditions` is rejected.
- Real node images and GPU drivers: nodes are management-plane compute instances (no VM boots), and Kubernetes nodes
  exist only with a Kubernetes backend.
- The `k3s` backend and its Docker-backed tests (Phase B), and mocker-issued IAM tokens at the Kubernetes API.

## Terraform

All four resources and data sources run against the real provider in `test/terraform/mk8s`: create, an empty plan, an
in-place update, another empty plan, a template change that rolls every node instance out in batches, replacement on
immutable attributes, and destroy, plus a `viewer` that can read but
not create.

| Kind | Terraform type | Service | RPCs called | Tested |
|---|---|---|---|---|
| Resource | `nebius_mk8s_v1_cluster` | `nebius.mk8s.v1.ClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | ✅ |
| Resource | `nebius_mk8s_v1_node_group` | `nebius.mk8s.v1.NodeGroupService` | Get, GetByName, Create, Update, Delete, PreflightCheck + OperationService.Get | ✅ |
| Resource | `nebius_mk8s_v1alpha1_cluster` | `nebius.mk8s.v1alpha1.ClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | ✅ |
| Resource | `nebius_mk8s_v1alpha1_node_group` | `nebius.mk8s.v1alpha1.NodeGroupService` | Get, GetByName, Create, Update, Delete + OperationService.Get | ✅ |
| Data source | `nebius_mk8s_v1_cluster` | `nebius.mk8s.v1.ClusterService` | Get, GetByName | ✅ |
| Data source | `nebius_mk8s_v1_node_group` | `nebius.mk8s.v1.NodeGroupService` | Get, GetByName | ✅ |
| Data source | `nebius_mk8s_v1alpha1_cluster` | `nebius.mk8s.v1alpha1.ClusterService` | Get, GetByName | ✅ |
| Data source | `nebius_mk8s_v1alpha1_node_group` | `nebius.mk8s.v1alpha1.NodeGroupService` | Get, GetByName | ✅ |

The provider has no kubeconfig resource: build a kubeconfig from `status.control_plane.endpoints` and
`auth.cluster_ca_certificate`, or run `nebius mk8s cluster get-credentials`.

## CLI

`test/cli/mk8s` runs the official `nebius` CLI through the flows of docs.nebius.com/kubernetes: `mk8s cluster
list-control-plane-versions`, `create` (public endpoint), `get-by-name`, `get-credentials` and the `get-token` exec
command; `mk8s node-group create` (fixed node count, platform and preset), `get-compatibility-matrix`, `update`, `upgrade`,
`list` and `delete`; the node instances in `compute instance list`, with `compute instance delete` and `stop` refused
(`FailedPrecondition`); a `viewer` that reads but cannot delete (`PermissionDenied`); and the Applications release
commands (see [applications.md](applications.md)).

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_MK8S_BACKEND` | `mock` | `mock` \| `k3s` (`k3s` is not available yet) |
| `NEBIUS_MOCKER_MK8S_ROLLOUT_STEP` | `0` | Pause between the batches of a node group rollout, on the mocker's clock |
| `NEBIUS_MOCKER_MK8S_VERSIONS` | `1.34,1.35,1.36` | Control-plane version catalog, `<major>.<minor>`, comma-separated; the newest is the default for new clusters |

## k3s backend

Documented by the k3s backend (Phase B).

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.mk8s.v1.ClusterService`

Endpoint: `mk8s.api.nebius.cloud:443` · Proto: [`nebius/mk8s/v1/cluster_service.proto`](https://github.com/nebius/api/blob/main/nebius/mk8s/v1/cluster_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetClusterRequest` | `Cluster` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Cluster` | ✅ |
| `List` | `ListClustersRequest` | `ListClustersResponse` |  |
| `Create` | `CreateClusterRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateClusterRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteClusterRequest` | `common.v1.Operation` | ✅ |
| `ListControlPlaneVersions` | `ListClusterControlPlaneVersionsRequest` | `ListClusterControlPlaneVersionsResponse` |  |

#### `nebius.mk8s.v1.NodeGroupService`

Endpoint: `mk8s.api.nebius.cloud:443` · Proto: [`nebius/mk8s/v1/node_group_service.proto`](https://github.com/nebius/api/blob/main/nebius/mk8s/v1/node_group_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetNodeGroupRequest` | `NodeGroup` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `NodeGroup` | ✅ |
| `List` | `ListNodeGroupsRequest` | `ListNodeGroupsResponse` |  |
| `Create` | `CreateNodeGroupRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateNodeGroupRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteNodeGroupRequest` | `common.v1.Operation` | ✅ |
| `Upgrade` | `UpgradeNodeGroupRequest` | `common.v1.Operation` |  |
| `PreflightCheck` | `PreflightCheckNodeGroupRequest` | `PreflightCheckNodeGroupResponse` | ✅ |
| `GetCompatibilityMatrix` | `GetNodeGroupCompatibilityMatrixRequest` | `NodeGroupCompatibilityMatrix` |  |

#### `nebius.mk8s.v1alpha1.ClusterService` *(deprecated)*

Endpoint: `mk8s.api.nebius.cloud:443` · Proto: [`nebius/mk8s/v1alpha1/cluster_service.proto`](https://github.com/nebius/api/blob/main/nebius/mk8s/v1alpha1/cluster_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetClusterRequest` | `Cluster` | ✅ |
| `GetByName` | `GetClusterByNameRequest` | `Cluster` | ✅ |
| `List` | `ListClustersRequest` | `ListClustersResponse` |  |
| `Create` | `CreateClusterRequest` | `common.v1alpha1.Operation` | ✅ |
| `Update` | `UpdateClusterRequest` | `common.v1alpha1.Operation` | ✅ |
| `Delete` | `DeleteClusterRequest` | `common.v1alpha1.Operation` | ✅ |
| `ListControlPlaneVersions` | `ListClusterControlPlaneVersionsRequest` | `ListClusterControlPlaneVersionsResponse` |  |

#### `nebius.mk8s.v1alpha1.NodeGroupService` *(deprecated)*

Endpoint: `mk8s.api.nebius.cloud:443` · Proto: [`nebius/mk8s/v1alpha1/node_group_service.proto`](https://github.com/nebius/api/blob/main/nebius/mk8s/v1alpha1/node_group_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetNodeGroupRequest` | `NodeGroup` | ✅ |
| `GetByName` | `GetNodeGroupByNameRequest` | `NodeGroup` | ✅ |
| `List` | `ListNodeGroupsRequest` | `ListNodeGroupsResponse` |  |
| `Create` | `CreateNodeGroupRequest` | `common.v1alpha1.Operation` | ✅ |
| `Update` | `UpdateNodeGroupRequest` | `common.v1alpha1.Operation` | ✅ |
| `Delete` | `DeleteNodeGroupRequest` | `common.v1alpha1.Operation` | ✅ |
| `Upgrade` | `UpgradeNodeGroupRequest` | `common.v1alpha1.Operation` |  |
