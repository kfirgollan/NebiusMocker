# Billing

> Module: `services/billing` · Image: `nebius-mocker-billing` · `NEBIUS_MOCKER_SERVICES=billing`
> M1 tier: **T1** pricing policy · T0 calculator, one-time export, spot price · Endpoints: `pricing-policies.billing-cpl`, `api.calculator.billing-data-plane`, `api.billing-report-exporter.billing-data-plane`

## Overview

- **Pricing policies** (`PricingPolicy`, prefix `pricingpolicy`): per-tenant price agreements bound to compute instance
  specs (`spec.compute_instance_spec`, `spec.pricing`). The provider manages them as `nebius_billing_v1_pricing_policy`.
- **Spot prices** (`v1alpha1.SpotPriceService`): current spot prices per platform and region.
- **Calculator** (`v1` and `v1alpha1` `CalculatorService`): price estimates for resource specs.
- **One-time export** (`v1alpha1.OneTimeExportService`): billing report export jobs.

## Emulation design

- Pricing policy: T1 CRUD with structural validation. Platforms are reference-checked against compute's catalog via `peer`.
- **M4**: a calculator driven by a configurable price table (`NEBIUS_MOCKER_BILLING_PRICES_FILE`, `sku-…` → unit price).
  It is deterministic, and it clearly isn't real pricing. Spot prices come from the same table plus an optional random
  walk on the mock clock, which feeds compute's `follows_spot_price` behavior. One-time exports produce a CSV in the S3
  data plane, if that is enabled.
- Permissions: `billing.pricingPolicies.*`, `billing.calculator.estimate`, `billing.exports.*`.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_billing_v1_pricing_policy` | `nebius.billing.v1.PricingPolicyService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T1 |
| Data source | `nebius_billing_v1_pricing_policy` | `nebius.billing.v1.PricingPolicyService` | Get, GetByName | T1 |

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.billing.v1.CalculatorService`

Endpoint: `api.calculator.billing-data-plane.api.nebius.cloud:443` · Proto: [`nebius/billing/v1/calculator_service.proto`](https://github.com/nebius/api/blob/main/nebius/billing/v1/calculator_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `EstimateBatch` | `EstimateBatchRequest` | `EstimateBatchResponse` |  |

#### `nebius.billing.v1.PricingPolicyService`

Endpoint: `pricing-policies.billing-cpl.api.nebius.cloud:443` · Proto: [`nebius/billing/v1/pricing_policy_service.proto`](https://github.com/nebius/api/blob/main/nebius/billing/v1/pricing_policy_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreatePricingPolicyRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetPricingPolicyRequest` | `PricingPolicy` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `PricingPolicy` | ✅ |
| `List` | `ListPricingPoliciesRequest` | `ListPricingPoliciesResponse` |  |
| `Update` | `UpdatePricingPolicyRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeletePricingPolicyRequest` | `common.v1.Operation` | ✅ |

#### `nebius.billing.v1alpha1.CalculatorService`

Endpoint: `api.calculator.billing-data-plane.api.nebius.cloud:443` · Proto: [`nebius/billing/v1alpha1/calculator_service.proto`](https://github.com/nebius/api/blob/main/nebius/billing/v1alpha1/calculator_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Estimate` | `EstimateRequest` | `EstimateResponse` |  |
| `EstimateBatch` | `EstimateBatchRequest` | `EstimateBatchResponse` |  |

#### `nebius.billing.v1alpha1.OneTimeExportService`

Endpoint: `api.billing-report-exporter.billing-data-plane.api.nebius.cloud:443` · Proto: [`nebius/billing/v1alpha1/one_time_export_service.proto`](https://github.com/nebius/api/blob/main/nebius/billing/v1alpha1/one_time_export_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateOneTimeExportRequest` | `common.v1.Operation` |  |
| `Get` | `GetOneTimeExportRequest` | `OneTimeExport` |  |
| `List` | `ListOneTimeExportsRequest` | `ListOneTimeExportsResponse` |  |

#### `nebius.billing.v1alpha1.SpotPriceService`

Endpoint: `pricing-policies.billing-cpl.api.nebius.cloud:443` · Proto: [`nebius/billing/v1alpha1/spot_price_service.proto`](https://github.com/nebius/api/blob/main/nebius/billing/v1alpha1/spot_price_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `List` | `ListSpotPricesRequest` | `ListSpotPricesResponse` |  |
| `ListSeries` | `ListSpotPriceSeriesRequest` | `ListSpotPriceSeriesResponse` |  |
