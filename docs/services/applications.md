# Applications — Marketplace releases and VM app templates

> Module: `services/applications` · Image: `nebius-mocker-applications` · `NEBIUS_MOCKER_SERVICES=applications`
> M1 tier: **T1** k8s release · T0 VM app templates · Endpoints: `deployment-manager.mkt` (K8sRelease), `apps.msp` (VmAppTemplate)

## Overview

- **K8s releases** (`applicationrelease`): install a Marketplace application (`product_slug`, `application_name`,
  `namespace`, `values`) into an mk8s cluster (`cluster_id`). Status has `state` and `error_message`.
- **VM app templates** (`vmapptemplate`): templates for deploying Marketplace VM apps (read-only catalog).

## Emulation design

- K8s release: T1. `cluster_id` must be a `RUNNING` mk8s cluster (via `peer`). `product_slug` is validated against a
  seeded Marketplace catalog. Deleting the cluster cascades to its releases. The provider's call set is
  Create/Get/Update/Delete (there is no `GetByName`; releases are effectively unnamed).
- **M3**: with the mk8s `k3s` backend, the release is installed as a Helm chart from a configurable chart map
  (`product_slug` → chart reference). Failures surface in `status.error_message`.
- VM app templates: seeded read-only catalog (M4).

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_applications_v1alpha1_k8s_release` | `nebius.applications.v1alpha1.K8sReleaseService` | Get, Create, Update, Delete + OperationService.Get | T1 |
| Data source | `nebius_applications_v1alpha1_k8s_release` | `nebius.applications.v1alpha1.K8sReleaseService` | Get | T1 |

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
