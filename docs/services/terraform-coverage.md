# Terraform provider coverage

This is the milestone-1 contract ([ADR-0007](../architecture/decisions/0007-terraform-first-milestone.md)): every object in
the `nebius/nebius` provider **v0.6.91** (source [`terraform-provider-nebius@09355968`](https://github.com/nebius/terraform-provider-nebius))
must pass its scenario in [`test/terraform`](../architecture/12-test-harness.md#3-terraform-scenarios-testterraform).

The RPC lists were extracted from the provider's generated code (`generated/nebius/**/**_service.tfgen.go`).
They are exactly the calls the provider can make, and therefore the minimum surface the mocker must implement.

**Totals:** 50 resources · 62 data sources · 4 ephemeral resources = **116 objects**, backed by **60 gRPC services / 262 RPCs**,
plus `nebius.common.v1.OperationService/Get` (every resource polls it after Create/Update/Delete) and
`nebius.iam.v1.TokenExchangeService/Exchange` (when the provider authenticates with `service_account` or
`impersonate_service_account_id`).

Every resource follows the same call pattern:

```text
plan:    [PreflightCheck]                      (mk8s node group only; UNIMPLEMENTED is tolerated)
create:  Create → op.ResourceID() → OperationService.Get (poll until status set) → Get(id)
read:    Get(id)                               (refresh; NOT_FOUND ⇒ resource removed from state)
update:  Update + X-ResetMask → poll → Get
delete:  Delete → poll
import:  Get(id)
data:    Get(id)  |  GetByName(parent_id, name)  |  special lookup (GetDefault, GetLatestByFamily, …)
```

> Provider configuration needed: see [architecture/04 § How clients reach the mock](../architecture/04-request-pipeline.md#how-clients-reach-the-mock).

## Resources (50)

| Terraform type | gRPC service | RPCs called by the provider | M1 tier | Service doc |
|---|---|---|---|---|
| `nebius_ai_v1_endpoint` | `nebius.ai.v1.EndpointService` | Get, GetByName, Create, Delete + OperationService.Get | T1 | [ai](ai.md) |
| `nebius_applications_v1alpha1_k8s_release` | `nebius.applications.v1alpha1.K8sReleaseService` | Get, Create, Update, Delete + OperationService.Get | T1 | [applications](applications.md) |
| `nebius_billing_v1_pricing_policy` | `nebius.billing.v1.PricingPolicyService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T1 | [billing](billing.md) |
| `nebius_capacity_v1_capacity_allowance` | `nebius.capacity.v1.CapacityAllowanceService` | Get, GetByParentAndCapacityBlockGroup, Create, Update, Delete + OperationService.Get | T1 | [capacity](capacity.md) |
| `nebius_compute_v1_disk` | `nebius.compute.v1.DiskService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [compute](compute.md) |
| `nebius_compute_v1_disk_snapshot` | `nebius.compute.v1.DiskSnapshotService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [compute](compute.md) |
| `nebius_compute_v1_filesystem` | `nebius.compute.v1.FilesystemService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [compute](compute.md) |
| `nebius_compute_v1_gpu_cluster` | `nebius.compute.v1.GpuClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [compute](compute.md) |
| `nebius_compute_v1_instance` | `nebius.compute.v1.InstanceService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [compute](compute.md) |
| `nebius_compute_v1_nvl_instance_group` | `nebius.compute.v1.NVLInstanceGroupService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T2 | [compute](compute.md) |
| `nebius_dns_v1_record` | `nebius.dns.v1.RecordService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 | [dns](dns.md) |
| `nebius_dns_v1_zone` | `nebius.dns.v1.ZoneService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 | [dns](dns.md) |
| `nebius_hash` | `— (provider-local)` | Computes hashes locally; no API call | — |  |
| `nebius_iam_v1_access_permit` | `nebius.iam.v1.AccessPermitService` | Create, Delete, Get + OperationService.Get | T2 | [iam](iam.md) |
| `nebius_iam_v1_auth_public_key` | `nebius.iam.v1.AuthPublicKeyService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T2 | [iam](iam.md) |
| `nebius_iam_v1_federated_credentials` | `nebius.iam.v1.FederatedCredentialsService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 | [iam](iam.md) |
| `nebius_iam_v1_federation` | `nebius.iam.v1.FederationService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T1 | [iam](iam.md) |
| `nebius_iam_v1_federation_certificate` | `nebius.iam.v1.FederationCertificateService` | Create, Get, Update, Delete + OperationService.Get | T1 | [iam](iam.md) |
| `nebius_iam_v1_group` | `nebius.iam.v1.GroupService` | Create, Get, GetByName, Delete, Update + OperationService.Get | T2 | [iam](iam.md) |
| `nebius_iam_v1_group_membership` | `nebius.iam.v1.GroupMembershipService` | Create, Get, Delete + OperationService.Get | T2 | [iam](iam.md) |
| `nebius_iam_v1_invitation` | `nebius.iam.v1.InvitationService` | Create, Get, Delete, Update + OperationService.Get | T1 | [iam](iam.md) |
| `nebius_iam_v1_service_account` | `nebius.iam.v1.ServiceAccountService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T2 | [iam](iam.md) |
| `nebius_iam_v2_access_key` | `nebius.iam.v2.AccessKeyService` | Create, Get, GetByName, GetSecret, Update, Delete + OperationService.Get | T2 | [iam](iam.md) |
| `nebius_iam_v2_project` | `nebius.iam.v2.ProjectService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T2 | [iam](iam.md) |
| `nebius_kms_v1_asymmetric_key` | `nebius.kms.v1.AsymmetricKeyService` | Create, Update, Get, GetByName, Delete + OperationService.Get | T1 | [kms](kms.md) |
| `nebius_kms_v1_symmetric_key` | `nebius.kms.v1.SymmetricKeyService` | Create, Update, Get, GetByName, Delete + OperationService.Get | T1 | [kms](kms.md) |
| `nebius_mk8s_v1_cluster` | `nebius.mk8s.v1.ClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [mk8s](mk8s.md) |
| `nebius_mk8s_v1_node_group` | `nebius.mk8s.v1.NodeGroupService` | Get, GetByName, Create, Update, Delete, PreflightCheck + OperationService.Get | T2 | [mk8s](mk8s.md) |
| `nebius_mk8s_v1alpha1_cluster` | `nebius.mk8s.v1alpha1.ClusterService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [mk8s](mk8s.md) |
| `nebius_mk8s_v1alpha1_node_group` | `nebius.mk8s.v1alpha1.NodeGroupService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [mk8s](mk8s.md) |
| `nebius_msp_mlflow_v1alpha1_cluster` | `nebius.msp.mlflow.v1alpha1.ClusterService` | Get, GetByName, Create, Delete + OperationService.Get | T1 | [msp-mlflow](msp-mlflow.md) |
| `nebius_msp_postgresql_v1alpha1_cluster` | `nebius.msp.postgresql.v1alpha1.ClusterService` | Get, GetByName, Create, Delete, Update + OperationService.Get | T1 | [msp-postgresql](msp-postgresql.md) |
| `nebius_mysterybox_v1_secret` | `nebius.mysterybox.v1.SecretService` | Create, Update, Get, GetByName, Delete + OperationService.Get | T2 | [mysterybox](mysterybox.md) |
| `nebius_mysterybox_v1_secret_version` | `nebius.mysterybox.v1.SecretVersionService` | Create, Get, Delete + OperationService.Get | T2 | [mysterybox](mysterybox.md) |
| `nebius_quotas_v1_quota_allowance` | `nebius.quotas.v1.QuotaAllowanceService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 | [quotas](quotas.md) |
| `nebius_registry_v1_registry` | `nebius.registry.v1.RegistryService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [registry](registry.md) |
| `nebius_storage_v1_bucket` | `nebius.storage.v1.BucketService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [storage](storage.md) |
| `nebius_storage_v1_inventory` | `nebius.storage.v1.InventoryService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 | [storage](storage.md) |
| `nebius_storage_v1_transfer` | `nebius.storage.v1.TransferService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 | [storage](storage.md) |
| `nebius_storage_v1alpha1_transfer` | `nebius.storage.v1alpha1.TransferService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 | [storage](storage.md) |
| `nebius_tunnel_v1_tunnel` | `nebius.tunnel.v1.TunnelService` | Get, Create, Update, Delete + OperationService.Get | T1 | [tunnel](tunnel.md) |
| `nebius_vpc_v1_allocation` | `nebius.vpc.v1.AllocationService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_network` | `nebius.vpc.v1.NetworkService` | Get, GetByName, GetDefault, Create, Update, Delete + OperationService.Get | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_pool` | `nebius.vpc.v1.PoolService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_route` | `nebius.vpc.v1.RouteService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_route_table` | `nebius.vpc.v1.RouteTableService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_security_group` | `nebius.vpc.v1.SecurityGroupService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_security_rule` | `nebius.vpc.v1.SecurityRuleService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_subnet` | `nebius.vpc.v1.SubnetService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1alpha1_allocation` | `nebius.vpc.v1alpha1.AllocationService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 | [vpc](vpc.md) |

## Data sources (62)

| Terraform type | gRPC service | RPCs called by the provider | M1 tier | Service doc |
|---|---|---|---|---|
| `nebius_ai_v1_endpoint` | `nebius.ai.v1.EndpointService` | Get, GetByName | T1 | [ai](ai.md) |
| `nebius_applications_v1alpha1_k8s_release` | `nebius.applications.v1alpha1.K8sReleaseService` | Get | T1 | [applications](applications.md) |
| `nebius_billing_v1_pricing_policy` | `nebius.billing.v1.PricingPolicyService` | Get, GetByName | T1 | [billing](billing.md) |
| `nebius_capacity_v1_capacity_allowance` | `nebius.capacity.v1.CapacityAllowanceService` | Get, GetByParentAndCapacityBlockGroup | T1 | [capacity](capacity.md) |
| `nebius_capacity_v1_capacity_block_group` | `nebius.capacity.v1.CapacityBlockGroupService` | Get, GetByResourceAffinity | T1 | [capacity](capacity.md) |
| `nebius_compute_v1_disk` | `nebius.compute.v1.DiskService` | Get, GetByName | T2 | [compute](compute.md) |
| `nebius_compute_v1_disk_snapshot` | `nebius.compute.v1.DiskSnapshotService` | Get, GetByName | T2 | [compute](compute.md) |
| `nebius_compute_v1_filesystem` | `nebius.compute.v1.FilesystemService` | Get, GetByName | T2 | [compute](compute.md) |
| `nebius_compute_v1_gpu_cluster` | `nebius.compute.v1.GpuClusterService` | Get, GetByName | T2 | [compute](compute.md) |
| `nebius_compute_v1_image` | `nebius.compute.v1.ImageService` | Get, GetByName | T2 | [compute](compute.md) |
| `nebius_compute_v1_instance` | `nebius.compute.v1.InstanceService` | Get, GetByName | T2 | [compute](compute.md) |
| `nebius_compute_v1_nvl_instance_group` | `nebius.compute.v1.NVLInstanceGroupService` | Get, GetByName | T2 | [compute](compute.md) |
| `nebius_dns_v1_record` | `nebius.dns.v1.RecordService` | Get, GetByName | T1 | [dns](dns.md) |
| `nebius_dns_v1_zone` | `nebius.dns.v1.ZoneService` | Get, GetByName | T1 | [dns](dns.md) |
| `nebius_iam_v1_access_permit` | `nebius.iam.v1.AccessPermitService` | Get | T2 | [iam](iam.md) |
| `nebius_iam_v1_auth_public_key` | `nebius.iam.v1.AuthPublicKeyService` | Get, GetByName | T2 | [iam](iam.md) |
| `nebius_iam_v1_federated_credentials` | `nebius.iam.v1.FederatedCredentialsService` | Get, GetByName | T1 | [iam](iam.md) |
| `nebius_iam_v1_federation` | `nebius.iam.v1.FederationService` | Get, GetByName | T1 | [iam](iam.md) |
| `nebius_iam_v1_federation_certificate` | `nebius.iam.v1.FederationCertificateService` | Get | T1 | [iam](iam.md) |
| `nebius_iam_v1_group` | `nebius.iam.v1.GroupService` | Get, GetByName | T2 | [iam](iam.md) |
| `nebius_iam_v1_group_membership` | `nebius.iam.v1.GroupMembershipService` | Get | T2 | [iam](iam.md) |
| `nebius_iam_v1_invitation` | `nebius.iam.v1.InvitationService` | Get | T1 | [iam](iam.md) |
| `nebius_iam_v1_project` | `nebius.iam.v1.ProjectService` | Get, GetByName | T2 | [iam](iam.md) |
| `nebius_iam_v1_service_account` | `nebius.iam.v1.ServiceAccountService` | Get, GetByName | T2 | [iam](iam.md) |
| `nebius_iam_v1_tenant` | `nebius.iam.v1.TenantService` | Get | T2 | [iam](iam.md) |
| `nebius_iam_v1_tenant_user_account` | `nebius.iam.v1.TenantUserAccountService` | Get | T2 | [iam](iam.md) |
| `nebius_iam_v2_access_key` | `nebius.iam.v2.AccessKeyService` | Get, GetByName | T2 | [iam](iam.md) |
| `nebius_iam_v2_project` | `nebius.iam.v2.ProjectService` | Get, GetByName | T2 | [iam](iam.md) |
| `nebius_iam_v2_tenant` | `nebius.iam.v2.TenantService` | Get, GetByName | T2 | [iam](iam.md) |
| `nebius_kms_v1_asymmetric_key` | `nebius.kms.v1.AsymmetricKeyService` | Get, GetByName | T1 | [kms](kms.md) |
| `nebius_kms_v1_symmetric_key` | `nebius.kms.v1.SymmetricKeyService` | Get, GetByName | T1 | [kms](kms.md) |
| `nebius_mk8s_v1_cluster` | `nebius.mk8s.v1.ClusterService` | Get, GetByName | T2 | [mk8s](mk8s.md) |
| `nebius_mk8s_v1_node_group` | `nebius.mk8s.v1.NodeGroupService` | Get, GetByName | T2 | [mk8s](mk8s.md) |
| `nebius_mk8s_v1alpha1_cluster` | `nebius.mk8s.v1alpha1.ClusterService` | Get, GetByName | T2 | [mk8s](mk8s.md) |
| `nebius_mk8s_v1alpha1_node_group` | `nebius.mk8s.v1alpha1.NodeGroupService` | Get, GetByName | T2 | [mk8s](mk8s.md) |
| `nebius_msp_mlflow_v1alpha1_cluster` | `nebius.msp.mlflow.v1alpha1.ClusterService` | Get, GetByName | T1 | [msp-mlflow](msp-mlflow.md) |
| `nebius_msp_postgresql_v1alpha1_cluster` | `nebius.msp.postgresql.v1alpha1.ClusterService` | Get, GetByName | T1 | [msp-postgresql](msp-postgresql.md) |
| `nebius_mysterybox_v1_secret` | `nebius.mysterybox.v1.SecretService` | Get, GetByName | T2 | [mysterybox](mysterybox.md) |
| `nebius_mysterybox_v1_secret_payload_entry` | `nebius.mysterybox.v1.PayloadService` | GetByKey | T2 | [mysterybox](mysterybox.md) |
| `nebius_mysterybox_v1_secret_version` | `nebius.mysterybox.v1.SecretVersionService` | Get | T2 | [mysterybox](mysterybox.md) |
| `nebius_parent_id` | `— (provider-local)` | Resolves NID parent types locally | — |  |
| `nebius_quotas_v1_quota_allowance` | `nebius.quotas.v1.QuotaAllowanceService` | Get, GetByName | T1 | [quotas](quotas.md) |
| `nebius_registry_v1_registry` | `nebius.registry.v1.RegistryService` | Get, GetByName | T2 | [registry](registry.md) |
| `nebius_storage_v1_bucket` | `nebius.storage.v1.BucketService` | Get, GetByName | T2 | [storage](storage.md) |
| `nebius_storage_v1_inventory` | `nebius.storage.v1.InventoryService` | Get, GetByName | T1 | [storage](storage.md) |
| `nebius_storage_v1_transfer` | `nebius.storage.v1.TransferService` | Get, GetByName | T1 | [storage](storage.md) |
| `nebius_storage_v1alpha1_transfer` | `nebius.storage.v1alpha1.TransferService` | Get, GetByName | T1 | [storage](storage.md) |
| `nebius_tunnel_v1_tunnel` | `nebius.tunnel.v1.TunnelService` | Get | T1 | [tunnel](tunnel.md) |
| `nebius_vpc_v1_allocation` | `nebius.vpc.v1.AllocationService` | Get, GetByName | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_default_network` | `nebius.vpc.v1.NetworkService` | GetDefault | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_network` | `nebius.vpc.v1.NetworkService` | Get, GetByName, GetDefault | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_pool` | `nebius.vpc.v1.PoolService` | Get, GetByName | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_route` | `nebius.vpc.v1.RouteService` | Get, GetByName | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_route_table` | `nebius.vpc.v1.RouteTableService` | Get, GetByName | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_security_group` | `nebius.vpc.v1.SecurityGroupService` | Get, GetByName | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_security_rule` | `nebius.vpc.v1.SecurityRuleService` | Get, GetByName | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1_subnet` | `nebius.vpc.v1.SubnetService` | Get, GetByName | T2 | [vpc](vpc.md) |
| `nebius_vpc_v1alpha1_allocation` | `nebius.vpc.v1alpha1.AllocationService` | Get, GetByName | T1 | [vpc](vpc.md) |
| `nebius_vpc_v1alpha1_network` | `nebius.vpc.v1alpha1.NetworkService` | Get, GetByName | T1 | [vpc](vpc.md) |
| `nebius_vpc_v1alpha1_pool` | `nebius.vpc.v1alpha1.PoolService` | Get, GetByName | T1 | [vpc](vpc.md) |
| `nebius_vpc_v1alpha1_scope` | `nebius.vpc.v1alpha1.ScopeService` | Get, GetByName | T1 | [vpc](vpc.md) |
| `nebius_vpc_v1alpha1_subnet` | `nebius.vpc.v1alpha1.SubnetService` | Get, GetByName | T1 | [vpc](vpc.md) |

## Ephemeral resources (4)

| Terraform type | gRPC service | RPCs called by the provider | M1 tier | Service doc |
|---|---|---|---|---|
| `nebius_iam_token` | `nebius.iam.v1.TokenExchangeService` | Exchange (when SA/impersonation creds); else returns current token | T2 | [iam](iam.md) |
| `nebius_iam_v2_access_key_secret` | `nebius.iam.v2.AccessKeyService` | GetSecret | T2 | [iam](iam.md) |
| `nebius_mysterybox_v1_secret_payload` | `nebius.mysterybox.v1.PayloadService` | Get | T2 | [mysterybox](mysterybox.md) |
| `nebius_mysterybox_v1_secret_payload_entry` | `nebius.mysterybox.v1.PayloadService` | GetByKey | T2 | [mysterybox](mysterybox.md) |
