# Billing

> Module: `services/billing` · Image: `nebius-mocker-billing` · `NEBIUS_MOCKER_SERVICES=billing`
> Tier: **T1** pricing policy, spot prices, calculator (v1, v1alpha1) · one-time export not yet · Endpoints: `pricing-policies.billing-cpl`, `api.calculator.billing-data-plane`, `api.billing-report-exporter.billing-data-plane`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.billing.v1.PricingPolicyService`: Create, Get, GetByName, List (paged), Update (reset mask, partial CLI updates, `resource_version`), Delete | ✅ T1 |
| Pricing-policy rules: project parent, name of Latin letters and digits, platform with a spot market in the project's region (and offered there for preemptibles, checked against compute's catalog through `peer.ComputePlatforms`), `max_price` decimal with ≤ 3 places inside the platform's range (`OUT_OF_RANGE` otherwise), immutable `compute_instance_spec` | ✅ |
| Pricing-policy status: `state` (CREATING → ACTIVE, UPDATING → ACTIVE on a price change, DELETING), `sku_id`, `currency`, `scheduling_state` (BLOCKED while `max_price` < spot price, computed on every read), `running_vm_count` | ✅ (`running_vm_count` is always 0, see Not yet) |
| `nebius.billing.v1alpha1.SpotPriceService`: List (paged), ListSeries (filters, 15-minute / 1-hour steps, first/last/avg), no authentication needed | ✅ |
| `nebius.billing.v1alpha1.CalculatorService`: Estimate, EstimateBatch (compute instances, disks, filesystems; create and update specs) | ✅ |
| `nebius.billing.v1.CalculatorService`: EstimateBatch (`Any` specs of the compute create/update requests, SKU breakdown, `currency`, `filter_aggregation_unit`) | ✅ |
| Price list (built-in `prices.yaml`, `NEBIUS_MOCKER_BILLING_PRICES_FILE`) | ✅ |
| Authorization: `billing.pricingPolicies.*` on the project (viewer reads, editor manages); calculator for any authenticated caller; spot prices public | ✅ |
| `nebius_billing_v1_pricing_policy` resource and data source with the official provider | ✅ e2e: `test/terraform/billing` |
| `nebius billing pricing-policy`, `nebius billing v1alpha1 spot-price`, `nebius billing v1alpha1 calculator` CLI flows from the docs | ✅ e2e: `test/cli/billing` |
| `nebius.billing.v1alpha1.OneTimeExportService` (Create, Get, List) | Not yet: `UNIMPLEMENTED` |
| Compute instances priced by a policy (`spec.spot_pricing_policy`) | Not yet |
| A moving spot market | Not yet |
| Quota on pricing policies per tenant | Not yet (owned by the quotas area) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/billing/{domain,dal,api}`), gosdk contract tests (`test/contract/billing_*`), the
Terraform e2e suite and the CLI e2e suite (`make e2e`).

### Behavior notes

These follow [the pricing-policy docs](https://docs.nebius.com/signup-billing/pricing-policy),
[managing policies](https://docs.nebius.com/signup-billing/manage-pricing-policy),
[spot prices](https://docs.nebius.com/signup-billing/spot-prices) and the protos. Where neither says what happens, the
mock's choice is marked *(mock)*.

- **Pricing policies** belong to a project and apply to one GPU platform in the project's region. The platform must have
  a spot market there in the price list (`INVALID_ARGUMENT` lists the platforms that do). When compute is enabled, the
  platform must also be offered in the region and allowed for preemptible VMs.
- **`max_price`** is a decimal string per GPU-hour in USD with at most three decimal places. It is stored as sent, so
  `"2.5"` stays `"2.5"`, and Terraform sees no drift. Values outside `[min_price, max_price]` of the platform's market
  are `OUT_OF_RANGE` *(mock: the docs name the minimum price but not the upper bound, so the price list sets both)*. A
  value below the current spot price is accepted, and the policy reports `SCHEDULING_STATE_BLOCKED`.
- **Names** are required, unique in the project (`ALREADY_EXISTS`) and may only contain Latin letters and digits, as the
  docs say. The name and labels can change at any time. `compute_instance_spec` is immutable: Terraform replaces the
  policy instead.
- **Running VMs.** Changing `max_price` or deleting a policy is `FAILED_PRECONDITION` while `running_vm_count` > 0. In
  the mock the count is always 0 (see Not yet).
- **Spot prices** come from the price list and are constant: their `effective_time` is the start of the 30-day history
  window, and `ListSeries` gives the same value for every step and for all three methods (first, last, average)
  *(mock)*. `ListSeries` follows the proto: `start_time` within the last 30 days, 15-minute steps only for up to 7 days,
  up to 100 SKU IDs (an unknown one is `INVALID_ARGUMENT`), points at the step ends. An `end_time` in the future is
  `INVALID_ARGUMENT` *(mock)*.
- **The calculator** prices from the price list. GPU platforms cost `gpu_hour` × GPUs of the preset, and non-GPU platforms
  cost `vcpu_hour` × vCPUs + `memory_gib_hour` × GiB. A preemptible instance costs the region's spot price × GPUs (what a
  VM pays while the spot price is under its policy's maximum). Disks and filesystems cost `gib_hour` × size. Disks an
  instance references are separate resources, and the instance's price does not include them. The price list has no
  SKUs for public IP addresses or network traffic, so the other instance fields don't change the estimate *(mock)*. A disk or filesystem
  spec without an explicit size (for example, sized by its image) is `INVALID_ARGUMENT` *(mock)*. Monthly costs are
  720 hours. `*_rounded` amounts round up to two places. `OFFER_TYPE_CONTRACT_PRICE` is accepted and changes nothing,
  because the mock has no contract prices. The v1 `currency` must be empty or `usd`.
- **Authorization.** Viewers read pricing policies and editors manage them (docs). Spot prices need no credentials
  (the proto says so). Anyone authenticated may use the calculator *(mock: the prices are public)*.

### Price list

`services/billing/domain/prices.yaml` is the built-in price list. `NEBIUS_MOCKER_BILLING_PRICES_FILE` replaces it with a
file of the same shape (YAML or JSON). The prices are **illustrative**, not Nebius' real prices:

```yaml
currency: usd
compute:
  - platform: gpu-h100-sxm
    product_title: "NVIDIA® H100 NVLink"
    gpu_hour: "2.95"            # per GPU-hour; non-GPU platforms set vcpu_hour and memory_gib_hour
    preemptible:                # the platform's spot market
      product_title: "Preemptible NVIDIA® H100 NVLink"
      min_price: "0.500"        # pricing-policy max_price bounds (OUT_OF_RANGE outside)
      max_price: "50.000"
      spot: { eu-north1: "1.250" }   # current spot price per GPU-hour, per region
disks:
  NETWORK_SSD: { product_title: "Network SSD disk", gib_hour: "0.0000986" }
filesystems:
  NETWORK_SSD: { product_title: "Network SSD shared filesystem", gib_hour: "0.0001111" }
```

SKU IDs are derived from the platform and type names, so they stay the same across restarts. Spot prices for regions
the mocker doesn't emulate (`NEBIUS_MOCKER_REGIONS`) are ignored.

### Not yet

- **OneTimeExportService** answers `UNIMPLEMENTED` with the reason: the mock has no billing contracts
  (`contract-…` parents) and meters no usage, so there is no FOCUS report to export.
- **Instances under a policy.** Compute still rejects `spec.spot_pricing_policy` by name, and mk8s rejects it in node
  templates. No VM runs under a policy, so `running_vm_count` stays 0 and preemption on a price rise is not emulated.
  (AI endpoints accept `spot_pricing_policy` and now check that the policy exists.)
- **A moving market.** Spot prices are constant: the price list has no random walk.
- **Quotas** on policies per tenant (`RESOURCE_EXHAUSTED`) are left to the quotas area.

## Overview

- **Pricing policies** (`PricingPolicy`, prefix `pricingpolicy`, parent: project): the maximum price per GPU-hour that
  preemptible VMs on one platform accept. A VM pays the spot price while it stays under the maximum and is preempted
  when it rises above. The provider manages them as `nebius_billing_v1_pricing_policy`.
- **Spot prices** (`v1alpha1.SpotPriceService`): current and historical spot prices per platform and region.
- **Calculator** (`v1` and `v1alpha1` `CalculatorService`): cost estimates for resource specs.
- **One-time export** (`v1alpha1.OneTimeExportService`): billing report export jobs (not implemented).

## Emulation design

- One price list drives everything: the calculator's unit prices, the spot markets, and the pricing-policy price ranges.
  It is deterministic and clearly not real pricing.
- Pricing policies are plain control-plane resources (`pkg/store`, operations, X-ResetMask). Their region is the
  project's (`peer.ProjectRegions`), and platforms are checked against compute's catalog (`peer.ComputePlatforms`).
- The calculator resolves presets through the same compute capability, so it needs the compute module enabled to
  price instances.

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
