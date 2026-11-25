# Common — Operations

> Package: `pkg/operations` (framework, not a module) · Always enabled
> Tier: **T2** · Endpoint: every endpoint that creates operations

## Overview

`nebius.common.v1.OperationService` (`Get`, `List`) and its deprecated twin `nebius.common.v1alpha1.OperationService`
(used by `mk8s/v1alpha1`, `vpc/v1alpha1` and `msp/*`). Neither has its own endpoint. Clients call them on the endpoint of
the service that created the operation. `List` takes a `resource_id` and returns that resource's operations.
Services also expose `ListOperationsByParent`.

The other `common` protos define shared messages: `ResourceMetadata`, `GetByNameRequest`, `Operation`, `ServiceError`,
`Warnings`, `PreflightCheck*` and `ProgressTracker`. Their semantics are in
[architecture/05](../architecture/05-api-conventions.md).

## Emulation design

See [architecture/08](../architecture/08-resource-lifecycle.md#operation-engine). Key points:

- The engine registers both service versions once on the edge server. IDs encode the owning module, so the gateway can
  route them in split deployments.
- `Get` on an unknown or expired ID → `NOT_FOUND`.
- `List` is paged and ordered by `created_at` descending, matching the real service. Confirm this through parity.
- The `v1alpha1` operation message differs in shape. The engine stores the v1 form and converts on the way out.



Every Terraform resource calls `OperationService.Get` (through `gosdk`'s `op.Wait`) after Create, Update and Delete.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.common.v1.OperationService`

Endpoint: same endpoint as the service that created the operation · Proto: [`nebius/common/v1/operation_service.proto`](https://github.com/nebius/api/blob/main/nebius/common/v1/operation_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetOperationRequest` | `Operation` |  |
| `List` | `ListOperationsRequest` | `ListOperationsResponse` |  |

#### `nebius.common.v1alpha1.OperationService` *(deprecated)*

Endpoint: same endpoint as the service that created the operation · Proto: [`nebius/common/v1alpha1/operation_service.proto`](https://github.com/nebius/api/blob/main/nebius/common/v1alpha1/operation_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetOperationRequest` | `Operation` |  |
| `List` | `ListOperationsRequest` | `ListOperationsResponse` |  |
