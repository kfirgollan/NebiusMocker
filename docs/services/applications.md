# Applications — Marketplace releases and VM app templates

> Module: `services/applications` · Image: `nebius-mocker-applications` · `NEBIUS_MOCKER_SERVICES=applications`
> M1 tier: **T1** k8s release (inventory only) · T0 VM app templates · Endpoints: `deployment-manager.mkt` (K8sRelease), `apps.msp` (VmAppTemplate)

## Implementation status

| Piece | Status |
|---|---|
| `nebius.applications.v1alpha1.K8sReleaseService`: Create, Get, List (paged, by `parent_id` and `cluster_id`), Update, Delete | ✅ T1, **inventory only** |
| Release lifecycle `status.state`: `CREATED` then `DEPLOYED`; `INSTALLING` then `DEPLOYED` on update | ✅ |
| Seeded product catalog (`NEBIUS_MOCKER_APPLICATIONS_PRODUCTS`) | ✅ *(mock slugs)* |
| `values` and `set` are input-only: kept for updates, never returned | ✅ |
| Releases are deleted with their mk8s cluster (`peer.ClusterReleases`) | ✅ |
| Authorization: `applications.k8sReleases.*` for every release RPC | ✅ |
| `nebius.applications.v1alpha1.VmAppTemplateService`: Get, List | `UNIMPLEMENTED` (registered, open to any authenticated principal) |
| `nebius_applications_v1alpha1_k8s_release` resource and data source with the official provider | ✅ e2e: `test/terraform/applications` |
| `nebius applications v1alpha1 k8s-release` CLI flows | ✅ e2e: `test/cli/mk8s` |
| Installing the chart into Kubernetes (Helm), with failures in `status.error_message` | Not yet (needs the mk8s `k3s` backend) |
| `List` `filter` | Rejected with `INVALID_ARGUMENT` |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/applications/{domain,dal,api}`), gosdk contract tests
(`test/contract/applications_release_test.go`: CRUD, validation, cluster-delete cascade, authorization), the Terraform
e2e suite (`test/terraform/applications`) and the CLI e2e suite (`test/cli/mk8s`), all run by `make e2e`.

### Behavior notes

These follow the protos. Where they do not say what happens, the mock's choice is marked *(mock)*.

- **Inventory only.** `Create`, `Update`, `Delete`, `Get` and `List` validate and store releases and walk
  `status.state`. **Nothing is installed into the Kubernetes cluster**, and no chart is pulled.
- **Create** needs `spec.cluster_id` to be an existing mk8s cluster of the same project (`parent_id`) and `RUNNING`
  (`FAILED_PRECONDITION` otherwise), and a different project is `INVALID_ARGUMENT`; the cluster is found through `peer.MK8sClusters`, so the `mk8s` module must be
  enabled. `metadata.id` and `resource_version` must not be set. The release is stored `CREATED` and becomes
  `DEPLOYED` when the operation completes (`NEBIUS_MOCKER_OP_DELAY` makes the `CREATED` state observable).
- **Validation.** `spec.product_slug` must be in the catalog; `spec.namespace` and `spec.application_name` must be
  DNS-1123 labels (at most 63 lowercase alphanumeric characters or `-`); `spec.values` must be valid YAML (the parse
  error is not echoed, since values are sensitive); `spec.set` keys must not be empty. All are `INVALID_ARGUMENT`.
- **Product catalog** *(mock slugs: the real Marketplace slugs are undocumented)*: `NEBIUS_MOCKER_APPLICATIONS_PRODUCTS`,
  comma-separated, default `nebius/soperator`, `nebius/nvidia-gpu-operator`, `nebius/nvidia-network-operator`,
  `nebius/kuberay`, `nebius/jupyterhub`, `nebius/mlflow`. A slug is lowercase letters, digits, `.`, `_`, `-` and `/`.
- **`values` and `set` are input-only.** They are stored for updates and never returned by Get, List or an operation
  (the proto marks them input-only), so Terraform keeps them from its own state.
- **Update** changes `metadata.name`, `labels`, `values` and `set`, bumps `resource_version`, and moves the state to
  `INSTALLING`, then `DEPLOYED`. `cluster_id`, `product_slug`, `namespace`, `application_name` and `parent_id` are
  immutable: sending the stored value again (as Terraform does) is accepted, a different one is `INVALID_ARGUMENT`.
  A `resource_version` that does not match is `FAILED_PRECONDITION`.
- **Cascade.** Deleting an mk8s cluster deletes its releases (the cluster's delete operation calls
  `peer.ClusterReleases`).
- **`List`** takes `parent_id` and `cluster_id`, both required, and pages in creation order. A `filter` is
  `INVALID_ARGUMENT` ("filtering is not implemented in NebiusMocker yet") instead of being ignored.
- **No `GetByName`.** The provider's call set is Create, Get, Update and Delete; releases are effectively unnamed.
- **`VmAppTemplateService`** is registered and every RPC answers `UNIMPLEMENTED`. Its authorization rules let any
  authenticated principal through so that callers see `UNIMPLEMENTED`, not `PERMISSION_DENIED`, under `enforce`.

### Authorization

`applications.k8sReleases.{get,list,create,update,delete}`, checked on the release or, for Create and List, on the
project. `viewer` reads releases and cannot write them (`test/contract/applications_release_test.go`,
`test/cli/mk8s`).

### Not yet

- Installing anything: the Helm install of `product_slug` into the cluster (`k3s` backend, see [mk8s.md](mk8s.md)), the
  per-product chart map, and `status.error_message` for install failures.
- `List` `filter`.
- The VM app template catalog (`VmAppTemplateService`).

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_APPLICATIONS_PRODUCTS` | `nebius/soperator,nebius/nvidia-gpu-operator,nebius/nvidia-network-operator,nebius/kuberay,nebius/jupyterhub,nebius/mlflow` | Marketplace product slugs a release may install |

## Terraform

`test/terraform/applications` runs the real provider: create, an empty plan, an in-place update of `values`, `set` and labels, another empty
plan, replacement on the immutable `namespace`, and destroy.

| Kind | Terraform type | Service | RPCs called | Tested |
|---|---|---|---|---|
| Resource | `nebius_applications_v1alpha1_k8s_release` | `nebius.applications.v1alpha1.K8sReleaseService` | Get, Create, Update, Delete + OperationService.Get | ✅ |
| Data source | `nebius_applications_v1alpha1_k8s_release` | `nebius.applications.v1alpha1.K8sReleaseService` | Get | ✅ |

## CLI

`test/cli/mk8s` runs `nebius applications v1alpha1 k8s-release create` (with `--cluster-id`, `--product-slug`,
`--namespace`, `--application-name`), `list` (as a `viewer`) and `delete` against a cluster created with `nebius mk8s`.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.applications.v1alpha1.K8sReleaseService`

Endpoint: `deployment-manager.mkt.api.nebius.cloud:443` · Proto: [`nebius/applications/v1alpha1/k8s_release_service.proto`](https://github.com/nebius/api/blob/main/nebius/applications/v1alpha1/k8s_release_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetK8sReleaseRequest` | `K8sRelease` | ✅ |
| `List` | `ListK8sReleasesRequest` | `ListK8sReleasesResponse` |  |
| `Create` | `CreateK8sReleaseRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateK8sReleaseRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteK8sReleaseRequest` | `common.v1.Operation` | ✅ |

#### `nebius.applications.v1alpha1.VmAppTemplateService`

Endpoint: `apps.msp.api.nebius.cloud:443` · Proto: [`nebius/applications/v1alpha1/vmapp_template_service.proto`](https://github.com/nebius/api/blob/main/nebius/applications/v1alpha1/vmapp_template_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetVmAppTemplateRequest` | `VmAppTemplate` |  |
| `List` | `ListVmAppTemplatesRequest` | `ListVmAppTemplatesResponse` |  |
