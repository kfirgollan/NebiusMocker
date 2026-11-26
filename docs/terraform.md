# Terraform with NebiusMocker

The official [`nebius/nebius`](https://registry.terraform.io/providers/nebius/nebius/latest/docs) Terraform provider
runs against the mock unmodified. This page shows how to point it at the mock, what to expect, and which provider
objects work today compared with the full list in the official provider docs.

Checked against provider **0.6.91**, the newest release on the Terraform Registry (2026-11-25), and its documentation at
[registry.terraform.io](https://registry.terraform.io/providers/nebius/nebius/latest/docs) and
[docs.nebius.com/terraform-provider](https://docs.nebius.com/terraform-provider/reference). Status as of 2026-11-26
(`main` at `935eb6e`).

## Summary

| Kind | In the official provider | Supported and tested | Supported, no Terraform test | Provider-local (no API) | Missing |
|---|---|---|---|---|---|
| Resources | 50 | 27 | 0 | 1 (`nebius_hash`) | 22 |
| Data sources | 62 | 31 | 2 | 1 (`nebius_parent_id`) | 28 |
| Ephemeral resources | 4 | 4 | 0 | 0 | 0 |
| **Total** | **116** | **62** | **2** | **2** | **50** |

"Supported and tested" means every RPC the provider calls for the object is implemented, and an end-to-end test runs
the real provider through it. For resources, that test covers create, an empty `terraform plan`, an in-place update,
another empty plan, replacement where the provider marks attributes as immutable, and destroy, with the mock's state
checked through the Go SDK after each step. For a missing object, the mock answers `UNIMPLEMENTED`, so
`terraform apply` fails with an error that names the gRPC service.

## Pointing the provider at the mock

```hcl
terraform {
  required_providers {
    nebius = { source = "nebius/nebius", version = "= 0.6.91" }
  }
}

provider "nebius" {
  resolvers       = { "*" = "127.0.0.1:4566" }      # every Nebius service → the mock's gRPC edge
  address_options = { "*" = { insecure = true } }   # plaintext gRPC
  token           = "nmock-dev-admin"               # built-in dev token
}
```

These three settings are the only change. The same configuration works against real Nebius once you remove them and
supply real credentials.

- **Seeded parents.** Use the tenant `tenant-e00mock0000000000default` and the project
  `project-e00mock000000000default` (region `eu-north1`). The project already has a default network
  (`data "nebius_vpc_v1_default_network"`) and a subnet named `default-subnet`. Public images live in
  `project-e00public-images`. Add more projects with `NEBIUS_MOCKER_IAM_PROJECTS=dev,ml:eu-west1`.
- **Tokens and roles.** `nmock-dev-admin`, `nmock-dev-editor` and `nmock-dev-viewer` belong to the seeded
  `admins`, `editors` and `viewers` groups. The default `NEBIUS_MOCKER_AUTHZ=audit` only logs denials. Set `enforce` to
  get real `PERMISSION_DENIED` errors, as the e2e suites do.
- **Service-account credentials.** The provider's `service_account` block and `impersonate_service_account_id` work:
  the mock implements `TokenExchangeService`. Create the key with `nebius_iam_v1_auth_public_key`. See
  [iam.md](services/iam.md#service-account-keys-and-token-exchange).
- **Operations.** Mutations return operations that finish almost at once. Set `NEBIUS_MOCKER_OP_DELAY=5s` to make them
  run longer and exercise the provider's polling.
- **Restarts.** State lives in memory. Restarting the mock empties it, so any Terraform state you kept points at
  resources that no longer exist. Run `terraform apply` again, or delete the state file.
- **Objects in buckets.** Like Nebius, the mock handles S3 objects through the `hashicorp/aws` provider, pointed at the
  mock's S3 front (`http://127.0.0.1:4580`) with a `nebius_iam_v2_access_key`. See
  [storage.md](services/storage.md#terraform).
- **No real hardware.** Instances, GPU clusters and disks are management-plane records. They go through their
  lifecycle states and get IP addresses, but nothing boots. Shared filesystems do store real data: mount one with
  rclone through a filesystem bucket ([compute.md](services/compute.md#shared-filesystems-implemented)).

## Resources

| Resource | Status | Notes |
|---|---|---|
| `nebius_ai_v1_endpoint` | ✅ | Inventory only, and no container runs. The endpoint URL points at an HTTPS stub that answers `200 "endpoint was called"`. UDP ports are rejected ([ai.md](services/ai.md)) |
| `nebius_applications_v1alpha1_k8s_release` | ❌ Missing | Needs mk8s |
| `nebius_billing_v1_pricing_policy` | ❌ Missing | |
| `nebius_capacity_v1_capacity_allowance` | ❌ Missing | |
| `nebius_compute_v1_disk` | ✅ | From an image, an image family or a snapshot. No quotas ([compute.md](services/compute.md#disks-and-snapshots-implemented)) |
| `nebius_compute_v1_disk_snapshot` | ✅ | |
| `nebius_compute_v1_filesystem` | ✅ | Real data in RustFS, and the size limit is enforced. The `NETWORK_SSD` family only: `NETWORK_HDD`, `WEKA` and `VAST` are rejected ([compute.md](services/compute.md#shared-filesystems-implemented)) |
| `nebius_compute_v1_gpu_cluster` | ✅ | Checked against a fabric catalog. No topology or capacity simulation ([compute.md](services/compute.md#gpu-clusters-implemented)) |
| `nebius_compute_v1_instance` | ✅ | CPU and GPU platforms, managed and existing disks, filesystems, GPU clusters, private and public IPs, and stop/start. Nothing boots. Preemptible instances are accepted but never preempted. Specific reservations (`reservation_ids`), spot pricing policies, local disk passthrough and NVL instance groups are rejected by name ([compute.md](services/compute.md#instances-implemented)) |
| `nebius_compute_v1_nvl_instance_group` | ❌ Missing | |
| `nebius_dns_v1_record` | ❌ Missing | |
| `nebius_dns_v1_zone` | ❌ Missing | |
| `nebius_hash` | ➖ Provider-local | Computed by the provider, with no API call |
| `nebius_iam_v1_access_permit` | ✅ | Enforced under `NEBIUS_MOCKER_AUTHZ=enforce` |
| `nebius_iam_v1_auth_public_key` | ✅ | Usable as provider `service_account` credentials |
| `nebius_iam_v1_federated_credentials` | ❌ Missing | |
| `nebius_iam_v1_federation` | ❌ Missing | |
| `nebius_iam_v1_federation_certificate` | ❌ Missing | |
| `nebius_iam_v1_group` | ✅ | |
| `nebius_iam_v1_group_membership` | ✅ | |
| `nebius_iam_v1_invitation` | ❌ Missing | |
| `nebius_iam_v1_service_account` | ✅ | |
| `nebius_iam_v2_access_key` | ✅ | Works as S3 credentials on the mock's S3 front |
| `nebius_iam_v2_project` | ✅ | |
| `nebius_kms_v1_asymmetric_key` | ✅ | Real ECDSA and RSA keys. Key import is not supported ([kms.md](services/kms.md)) |
| `nebius_kms_v1_symmetric_key` | ✅ | Real AES-256-GCM keys, with rotation and delayed deletion |
| `nebius_mk8s_v1_cluster` | ❌ Missing | |
| `nebius_mk8s_v1_node_group` | ❌ Missing | |
| `nebius_mk8s_v1alpha1_cluster` | ❌ Missing | |
| `nebius_mk8s_v1alpha1_node_group` | ❌ Missing | |
| `nebius_msp_mlflow_v1alpha1_cluster` | ❌ Missing | |
| `nebius_msp_postgresql_v1alpha1_cluster` | ❌ Missing | |
| `nebius_mysterybox_v1_secret` | ✅ | No encryption with a customer KMS key ([mysterybox.md](services/mysterybox.md)) |
| `nebius_mysterybox_v1_secret_version` | ✅ | |
| `nebius_quotas_v1_quota_allowance` | ❌ Missing | |
| `nebius_registry_v1_registry` | ✅ | `docker push`/`pull` work with the Harbor backend ([registry.md](services/registry.md)) |
| `nebius_storage_v1_bucket` | ✅ | CORS, policies, lifecycle rules, size limits and storage classes take effect with the RustFS backend ([storage.md](services/storage.md)) |
| `nebius_storage_v1_inventory` | ❌ Missing | |
| `nebius_storage_v1_transfer` | ❌ Missing | |
| `nebius_storage_v1alpha1_transfer` | ❌ Missing | |
| `nebius_tunnel_v1_tunnel` | ❌ Missing | |
| `nebius_vpc_v1_allocation` | ✅ | Real IP address management: private and public addresses come from pool CIDRs ([vpc.md](services/vpc.md)) |
| `nebius_vpc_v1_network` | ✅ | |
| `nebius_vpc_v1_pool` | ✅ | |
| `nebius_vpc_v1_route` | ✅ | |
| `nebius_vpc_v1_route_table` | ✅ | |
| `nebius_vpc_v1_security_group` | ✅ | |
| `nebius_vpc_v1_security_rule` | ✅ | Stored and validated. No traffic is filtered |
| `nebius_vpc_v1_subnet` | ✅ | |
| `nebius_vpc_v1alpha1_allocation` | ❌ Missing | The whole `vpc.v1alpha1` API is not implemented. Use `nebius_vpc_v1_allocation` |

## Data sources

| Data source | Status | Notes |
|---|---|---|
| `nebius_ai_v1_endpoint` | ✅ | |
| `nebius_applications_v1alpha1_k8s_release` | ❌ Missing | |
| `nebius_billing_v1_pricing_policy` | ❌ Missing | |
| `nebius_capacity_v1_capacity_allowance` | ❌ Missing | |
| `nebius_capacity_v1_capacity_block_group` | ❌ Missing | |
| `nebius_compute_v1_disk` | ✅ | |
| `nebius_compute_v1_disk_snapshot` | ✅ | |
| `nebius_compute_v1_filesystem` | ✅ | |
| `nebius_compute_v1_gpu_cluster` | ✅ | |
| `nebius_compute_v1_image` | ✅ | Seeded public catalog for each region, plus custom images imported from Object Storage |
| `nebius_compute_v1_instance` | ✅ | |
| `nebius_compute_v1_nvl_instance_group` | ❌ Missing | |
| `nebius_dns_v1_record` | ❌ Missing | |
| `nebius_dns_v1_zone` | ❌ Missing | |
| `nebius_iam_v1_access_permit` | ⚠️ No Terraform test | The RPC (`Get`) is implemented and covered by contract tests |
| `nebius_iam_v1_auth_public_key` | ✅ | |
| `nebius_iam_v1_federated_credentials` | ❌ Missing | |
| `nebius_iam_v1_federation` | ❌ Missing | |
| `nebius_iam_v1_federation_certificate` | ❌ Missing | |
| `nebius_iam_v1_group` | ✅ | Tested in `make e2e-filesystems` |
| `nebius_iam_v1_group_membership` | ⚠️ No Terraform test | The RPC (`Get`) is implemented and covered by contract tests |
| `nebius_iam_v1_invitation` | ❌ Missing | |
| `nebius_iam_v1_project` | ✅ | |
| `nebius_iam_v1_service_account` | ✅ | |
| `nebius_iam_v1_tenant` | ✅ | |
| `nebius_iam_v1_tenant_user_account` | ❌ Missing | |
| `nebius_iam_v2_access_key` | ✅ | |
| `nebius_iam_v2_project` | ✅ | |
| `nebius_iam_v2_tenant` | ✅ | Reading by name needs `parent_id = <tenant ID>` |
| `nebius_kms_v1_asymmetric_key` | ✅ | |
| `nebius_kms_v1_symmetric_key` | ✅ | |
| `nebius_mk8s_v1_cluster` | ❌ Missing | |
| `nebius_mk8s_v1_node_group` | ❌ Missing | |
| `nebius_mk8s_v1alpha1_cluster` | ❌ Missing | |
| `nebius_mk8s_v1alpha1_node_group` | ❌ Missing | |
| `nebius_msp_mlflow_v1alpha1_cluster` | ❌ Missing | |
| `nebius_msp_postgresql_v1alpha1_cluster` | ❌ Missing | |
| `nebius_mysterybox_v1_secret` | ✅ | |
| `nebius_mysterybox_v1_secret_payload_entry` | ✅ | |
| `nebius_mysterybox_v1_secret_version` | ✅ | |
| `nebius_parent_id` | ➖ Provider-local | Resolved by the provider, with no API call |
| `nebius_quotas_v1_quota_allowance` | ❌ Missing | |
| `nebius_registry_v1_registry` | ✅ | |
| `nebius_storage_v1_bucket` | ✅ | |
| `nebius_storage_v1_inventory` | ❌ Missing | |
| `nebius_storage_v1_transfer` | ❌ Missing | |
| `nebius_storage_v1alpha1_transfer` | ❌ Missing | |
| `nebius_tunnel_v1_tunnel` | ❌ Missing | |
| `nebius_vpc_v1_allocation` | ✅ | |
| `nebius_vpc_v1_default_network` | ✅ | Seeded default network and subnet in each project |
| `nebius_vpc_v1_network` | ✅ | |
| `nebius_vpc_v1_pool` | ✅ | |
| `nebius_vpc_v1_route` | ✅ | |
| `nebius_vpc_v1_route_table` | ✅ | |
| `nebius_vpc_v1_security_group` | ✅ | |
| `nebius_vpc_v1_security_rule` | ✅ | |
| `nebius_vpc_v1_subnet` | ✅ | |
| `nebius_vpc_v1alpha1_allocation` | ❌ Missing | `vpc.v1alpha1` is not implemented |
| `nebius_vpc_v1alpha1_network` | ❌ Missing | |
| `nebius_vpc_v1alpha1_pool` | ❌ Missing | |
| `nebius_vpc_v1alpha1_scope` | ❌ Missing | |
| `nebius_vpc_v1alpha1_subnet` | ❌ Missing | |

## Ephemeral resources

| Ephemeral resource | Status | Notes |
|---|---|---|
| `nebius_iam_token` | ✅ | Returns the provider's token, or exchanges service-account credentials |
| `nebius_iam_v2_access_key_secret` | ✅ | Feed it to the `aws` provider for S3 objects |
| `nebius_mysterybox_v1_secret_payload` | ✅ | |
| `nebius_mysterybox_v1_secret_payload_entry` | ✅ | |

## What is missing, by area

| Area | Missing objects | Service doc |
|---|---|---|
| Managed Kubernetes | `mk8s_v1(alpha1)_cluster`, `mk8s_v1(alpha1)_node_group` (4 resources, 4 data sources) | [mk8s.md](services/mk8s.md) |
| IAM | `federation`, `federation_certificate`, `federated_credentials`, `invitation` (4 resources, 4 data sources), `tenant_user_account` (data source) | [iam.md](services/iam.md) |
| VPC v1alpha1 | `allocation` (resource and data source); `network`, `pool`, `scope`, `subnet` (data sources) | [vpc.md](services/vpc.md) |
| Object Storage | `inventory`, `transfer` (v1 and v1alpha1): 3 resources, 3 data sources | [storage.md](services/storage.md) |
| DNS | `zone`, `record` (2 resources, 2 data sources) | [dns.md](services/dns.md) |
| Compute | `nvl_instance_group` (resource and data source) | [compute.md](services/compute.md) |
| Capacity | `capacity_allowance` (resource and data source), `capacity_block_group` (data source) | [capacity.md](services/capacity.md) |
| Managed databases | `msp_postgresql_v1alpha1_cluster`, `msp_mlflow_v1alpha1_cluster` (resource and data source each) | [msp-postgresql.md](services/msp-postgresql.md), [msp-mlflow.md](services/msp-mlflow.md) |
| Other | `applications_v1alpha1_k8s_release`, `billing_v1_pricing_policy`, `quotas_v1_quota_allowance`, `tunnel_v1_tunnel` (resource and data source each) | [applications.md](services/applications.md), [billing.md](services/billing.md), [quotas.md](services/quotas.md), [tunnel.md](services/tunnel.md) |

Across the supported objects, these are not modeled:
- **Quotas:** nothing returns `RESOURCE_EXHAUSTED` for a quota.
- **Capacity:** no `NotEnoughResources` errors.
- **Hardware faults:** nothing ever reaches an `ERROR` state.

Each service doc's **Implementation status** section lists the field-level gaps. Fields the mock does not support are
rejected with an error that names them, never silently ignored.

## How this list is kept current

Run `/terraform-sweep` (`.claude/skills/terraform-sweep/`). Its script
(`python3 -I .claude/skills/terraform-sweep/scripts/sweep.py`) does three things:
- Reads every object and the gRPC calls it makes from the newest provider release's generated code.
- Checks which calls the mock serves, matching the exact proto package and version, and which objects a test under
  `test/` uses in HCL.
- With `--write`, updates this page's statuses and summary.

The skill then has you check the changes against the code and update the notes, the "missing by area" table,
[services/terraform-coverage.md](services/terraform-coverage.md), the README and the service docs.

The Terraform suites live in `test/terraform/<area>` and run with `make e2e`. The Docker-backed flows run with
`make e2e-<area>`. A new provider release is adopted (the pin bumped and the gaps filled) through an
`add-nebius-service` refresh. Run the sweep again afterwards.
