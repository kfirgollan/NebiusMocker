# mk8s — Managed Service for Kubernetes

> Module: `services/mk8s` · Image: `nebius-mocker-mk8s` · `NEBIUS_MOCKER_SERVICES=mk8s`
> M1 tier: **T2** (`mock` backend) · M3: **T3** (`k3s` backend) · Endpoint: `mk8s.api.nebius.cloud`

## Overview

Managed Kubernetes **clusters** (control plane in a VPC subnet, with public/private endpoints, etcd size, OIDC
authentication, audit logs, Karpenter) and **node groups** (a templated set of compute instances with fixed or
autoscaled size, a deployment strategy, auto-repair, GPU settings, taints and labels). `v1alpha1` is the deprecated
previous version; it is still supported by the provider and uses `common.v1alpha1.OperationService`.

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Cluster | `mk8s.v1.Cluster` | `mk8scluster` | project | `PROVISIONING, RUNNING, DELETING` (+ `reconciling`) |
| Node group | `mk8s.v1.NodeGroup` | `mk8snodegroup` | cluster | `PROVISIONING, RUNNING, DELETING` |

## Reference: how LocalStack does EKS

LocalStack's EKS emulation ([docs](https://docs.localstack.cloud/aws/services/eks/)) has two parts. The control-plane API
(CreateCluster/DescribeCluster/node groups) is emulated in-process. When a cluster is created, LocalStack can also start a
**real k3s cluster** in a Docker container (or reuse a configured host cluster), so the returned endpoint, CA and kubeconfig
work with `kubectl`. Node groups map onto that cluster. Configuration selects the k8s provider and image.

We follow the same split. It is the clearest example of LocalStack mixing a "mock API" with a "real backend".

## Emulation design

### Backends (`NEBIUS_MOCKER_MK8S_BACKEND`)
| Backend | Cluster status outputs | Use |
|---|---|---|
| `mock` (default, M1) | `status.control_plane.endpoints.public_endpoint = https://<cluster-id>.mk8s.mocker.localhost:6443`, a self-signed CA in `status.control_plane.auth.cluster_ca_certificate`, version echoed | Terraform/IaC tests that don't talk to the k8s API |
| `k3s` (M3) | Starts `rancher/k3s:<version>` through the Docker socket, one container per cluster, labeled `nebius-mocker.cluster=<id>`. The endpoint is the container's published port; the CA comes from the k3s server | Tests that deploy workloads with kubectl, Helm or the Kubernetes provider |
| `external` (M3) | Points every cluster at a user-provided kubeconfig context (namespaced per cluster) | CI that already has kind |

Node groups under the `k3s` backend add k3s **agent containers** (capped by `NEBIUS_MOCKER_MK8S_MAX_AGENTS`), or just label
the server node, following `spec.template.metadata.labels` and `taints`.

### Validation
- `control_plane.subnet_id` (immutable) must be a `READY` subnet in the same project (via `peer` → vpc). The cluster
  registers as a dependent, so the subnet cannot be deleted under it.
- `control_plane.version` must appear in `ListControlPlaneVersions`, which is seeded (for example `1.30`, `1.31`, `1.32`).
  Upgrades may only increase it, by one minor version at a time.
- `etcd_cluster_size` ∈ {1, 3, 5}. `kube_network.service_cidrs` must not overlap the VPC's CIDRs.
- Node group: `version` ≤ the cluster version and ≥ cluster minus 2 minors (`GetCompatibilityMatrix` returns the seeded
  matrix). `fixed_node_count` and `autoscaling{min,max}` are mutually exclusive. `template.resources` must be a valid
  platform/preset (via `peer` → compute). `template.boot_disk`, `network_interfaces[*].subnet_id` and
  `gpu_cluster.id` are reference-checked.

### Lifecycle and side effects
- Cluster Create: `PROVISIONING` (timing key `mk8s.cluster.create`) → `RUNNING`, with endpoints and CA filled when it
  enters `RUNNING`. Cluster Delete cascades to node groups.
- Node group Create/Update: `PROVISIONING` → `RUNNING`. `status.node_count`, `ready_node_count` and `target_node_count`
  converge to the spec over the operation's progress steps (`progress_tracker` filled). `Upgrade` moves `status.version`
  and counts down `outdated_node_count`.
- `NEBIUS_MOCKER_MK8S_MATERIALIZE_NODES=1` creates real `computeinstance` records for nodes, so that compute quotas and
  `compute` List calls include them. Off by default.

### Preflight check
`NodeGroupService.PreflightCheck` runs the full validation above in dry-run mode, honoring `context.unknown_paths`
(unknown values skip their checks). It returns diagnostics instead of failing. When `context.action=UPDATE` and an
immutable path changed, it returns `paths_require_recreate`. This is the only preflight RPC in the API today, and the
provider calls it on every `plan` that touches a node group.

### Permissions and quotas
- Permissions: `mk8s.clusters.*`, `mk8s.nodeGroups.*` (+ `upgrade`).
- Quotas: `mk8s.cluster.count` (tenant), `mk8s.nodegroup.count` (cluster). Node-level compute quotas apply only with
  materialized nodes.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_mk8s_v1_cluster` | `nebius.mk8s.v1.ClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_mk8s_v1_node_group` | `nebius.mk8s.v1.NodeGroupService` | Get, GetByName, Create, Update, Delete, PreflightCheck + OperationService.Get | T2 |
| Resource | `nebius_mk8s_v1alpha1_cluster` | `nebius.mk8s.v1alpha1.ClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_mk8s_v1alpha1_node_group` | `nebius.mk8s.v1alpha1.NodeGroupService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Data source | `nebius_mk8s_v1_cluster` | `nebius.mk8s.v1.ClusterService` | Get, GetByName | T2 |
| Data source | `nebius_mk8s_v1_node_group` | `nebius.mk8s.v1.NodeGroupService` | Get, GetByName | T2 |
| Data source | `nebius_mk8s_v1alpha1_cluster` | `nebius.mk8s.v1alpha1.ClusterService` | Get, GetByName | T2 |
| Data source | `nebius_mk8s_v1alpha1_node_group` | `nebius.mk8s.v1alpha1.NodeGroupService` | Get, GetByName | T2 |

The provider has no kubeconfig resource. Users build kubeconfig from `status.control_plane.endpoints` and
`auth.cluster_ca_certificate`, then authenticate with `nebius mk8s cluster get-credentials` (CLI) or an exec plugin. With
the `k3s` backend, the mocker also accepts mocker-issued IAM tokens at the k3s API through a webhook token authenticator
(M3), so that the exec-plugin flow works.

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_MK8S_BACKEND` | `mock` | `mock` \| `k3s` \| `external` |
| `NEBIUS_MOCKER_MK8S_K3S_IMAGE` | `rancher/k3s:v1.31.x-k3s1` | Per-version image map is configurable |
| `NEBIUS_MOCKER_MK8S_VERSIONS` | `1.30,1.31,1.32` | Control-plane versions catalog |
| `NEBIUS_MOCKER_MK8S_MATERIALIZE_NODES` | `0` | Create compute instances for nodes |
| `NEBIUS_MOCKER_MK8S_MAX_AGENTS` | `3` | k3s agent containers per cluster |
| `NEBIUS_MOCKER_MK8S_EXTERNAL_KUBECONFIG` | — | For `external` backend |

## Open questions
- The exact endpoint format, and whether `private_endpoint` is always present.
- Default `etcd_cluster_size` and `service_cidrs` (these are `NON_EMPTY_DEFAULT` candidates and affect empty-plan fidelity).
- Real version-skew rules for node groups.

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
