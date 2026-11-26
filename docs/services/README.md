# Services catalog

This is every public Nebius AI Cloud gRPC service in [`nebius/api@45221893`](https://github.com/nebius/api/tree/45221893616947fbd676f82e57b2ce02795b4a72):
**91 services, 488 RPCs, 28 endpoints**, grouped into the 21 *areas* that NebiusMocker uses as module boundaries
(`NEBIUS_MOCKER_SERVICES` values, `services/<area>` packages, `nebius-mocker-<area>` images).

Tiers are defined in [architecture/01](../architecture/01-goals-and-scope.md#fidelity-tiers):
**T0** unimplemented stub · **T1** stateful CRUD · **T2** behavioral · **T3** functional data plane.

| Area | Doc | gRPC services | RPCs | Used by Terraform (services / TF objects) | M1 tier | Later |
|---|---|---|---|---|---|---|
| IAM | [iam.md](iam.md) | 21 | 115 | 15 / 28 | **T2** | — |
| VPC | [vpc.md](vpc.md) | 14 | 84 | 13 / 23 | **T2** (v1 and v1alpha1; v1alpha1 is a view of the v1 resources) | — |
| Compute | [compute.md](compute.md) | 10 | 59 | 7 / 13 | **T2** (ImageService implemented, image payloads in RustFS; DiskService + DiskSnapshotService, GpuClusterService, NVLInstanceGroupService, InstanceService + PlatformService implemented, management plane only: no VM boots) | container-backed VMs (T3, optional) |
| Managed Kubernetes | [mk8s.md](mk8s.md) | 4 | 30 | 4 / 8 | **T2** ✅ implemented (v1 and v1alpha1; nodes are compute instances; `mock` backend); **T3** ✅ `k3s` backend: real Kubernetes in Docker (`make e2e-mk8s`) | — |
| Object Storage | [storage.md](storage.md) | 4 | 33 | 4 / 8 | **T2** bucket, inventory, transfer (v1, v1alpha1) | S3 data plane via RustFS (✅ buckets, objects, inventory reports, transfers; see storage.md) |
| Container Registry | [registry.md](registry.md) | 2 | 9 | 1 / 2 | **T2** ✅ implemented; **T3** OCI data plane via Harbor ✅ | — |
| MysteryBox (secrets) | [mysterybox.md](mysterybox.md) | 3 | 14 | 3 / 7 | **T2** | — |
| KMS | [kms.md](kms.md) | 4 | 23 | 2 / 4 | **T3** | — |
| DNS | [dns.md](dns.md) | 2 | 13 | 2 / 4 | **T2** ✅ implemented; **T3** DNS responder (`miekg/dns`) ✅ | — |
| Quotas | [quotas.md](quotas.md) | 1 | 6 | 1 / 2 | T1 (+ live usage) | — |
| Billing | [billing.md](billing.md) | 5 | 14 | 1 / 2 | T1 pricing policy, T0 rest | T1 calculator (M4) |
| Capacity | [capacity.md](capacity.md) | 4 | 14 | 2 / 3 | **T2** ✅ implemented (seeded block groups and intervals on the mock clock, project allowances, resource advice) | Compute instances drawing from reservations (`followup-compute-reservations`) |
| Managed PostgreSQL | [msp-postgresql.md](msp-postgresql.md) | 2 | 15 | 1 / 2 | T1 | — |
| Managed MLflow | [msp-mlflow.md](msp-mlflow.md) | 1 | 7 | 1 / 2 | T1 | — |
| AI (endpoints, jobs, devlabs) | [ai.md](ai.md) | 4 | 23 | 1 / 2 | **T2** endpoint inventory (stub data plane), T0 rest | T1 jobs/devlabs (M4) |
| Applications | [applications.md](applications.md) | 2 | 7 | 1 / 2 | T1 k8s release ✅ implemented (inventory only), T0 rest | T2 with k3s (Phase B) |
| Tunnel | [tunnel.md](tunnel.md) | 1 | 5 | 1 / 2 | T1 ✅ implemented (inventory only: no agent can connect) | Data plane if the agent protocol is documented |
| Audit | [audit.md](audit.md) | 2 | 5 | 0 / 0 | **T2** | — |
| Logging | [logging.md](logging.md) | 2 | 5 | 0 / 0 | T0 | T1 (M4) |
| Maintenance | [maintenance.md](maintenance.md) | 1 | 3 | 0 / 0 | T0 | T1 (M4) |
| Common (operations) | [common.md](common.md) | 2 | 4 | all | **T2** (framework) | — |

Also see:

- [terraform-coverage.md](terraform-coverage.md): every Terraform resource, data source and ephemeral resource → service → RPCs → tier.
- [../terraform.md](../terraform.md): which of those objects work against the mock today, and what is missing.
- [_template.md](_template.md): the template for adding a service doc.

## Endpoint map

Real clients address services by host. NebiusMocker serves all of them on one edge port
([ADR-0001](../architecture/decisions/0001-grpc-single-edge-port.md)). This table matters for the optional per-service-port
and wildcard-TLS modes, and for reading upstream docs. `OperationService` (v1 and v1alpha1) is served on every endpoint
that creates operations.

| Real endpoint (`:443`) | Services |
|---|---|
| `api.billing-report-exporter.billing-data-plane.api.nebius.cloud` | `billing.v1alpha1.OneTimeExportService` |
| `api.calculator.billing-data-plane.api.nebius.cloud` | `billing.v1.CalculatorService`, `billing.v1alpha1.CalculatorService` |
| `applicationtunnel.mkt.api.nebius.cloud` | `tunnel.v1.TunnelService` |
| `apps.msp.api.nebius.cloud` | `ai.v1.DevlabService`, `ai.v1.EndpointService`, `ai.v1.HardwareService`, `ai.v1.JobService`, `applications.v1alpha1.VmAppTemplateService` |
| `audit.api.nebius.cloud` | `audit.v2.AuditEventExportService`, `audit.v2.AuditEventService` |
| `capacity-advisor.billing-cpl.api.nebius.cloud` | `capacity.v1.ResourceAdviceService` |
| `capacity-blocks.billing-cpl.api.nebius.cloud` | `capacity.v1.CapacityAllowanceService`, `capacity.v1.CapacityBlockGroupService`, `capacity.v1.CapacityIntervalService` |
| `compute.api.nebius.cloud` | `compute.v1.DiskService`, `compute.v1.DiskSnapshotService`, `compute.v1.FilesystemService`, `compute.v1.GpuClusterService`, `compute.v1.ImageService`, `compute.v1.InstanceService`, `compute.v1.MaintenanceService`, `compute.v1.NVLInstanceGroupService`, `compute.v1.NodeService`, `compute.v1.PlatformService` |
| `cpl.iam.api.nebius.cloud` | `iam.v1.AccessKeyService`, `iam.v1.AccessPermitService`, `iam.v1.AuthPublicKeyService`, `iam.v1.FederatedCredentialsService`, `iam.v1.FederationCertificateService`, `iam.v1.FederationService`, `iam.v1.GroupMembershipService`, `iam.v1.GroupService`, `iam.v1.InvitationService`, `iam.v1.ProfileService`, `iam.v1.ProjectService`, `iam.v1.ServiceAccountService`, `iam.v1.SessionManagementService`, `iam.v1.StaticKeyService`, `iam.v1.TenantService`, `iam.v1.TenantUserAccountService`, `iam.v1.TenantUserAccountWithAttributesService`, `iam.v2.AccessKeyService`, `iam.v2.ProjectService`, `iam.v2.TenantService` |
| `cpl.kms.api.nebius.cloud` | `kms.v1.AsymmetricKeyService`, `kms.v1.SymmetricKeyService` |
| `cpl.mysterybox.api.nebius.cloud` | `mysterybox.v1.SecretService`, `mysterybox.v1.SecretVersionService` |
| `cpl.storage.api.nebius.cloud` | `storage.v1.BucketService`, `storage.v1.InventoryService` |
| `cpl.teplo.api.nebius.cloud` | `logging.v1.LogExportService` |
| `deployment-manager.mkt.api.nebius.cloud` | `applications.v1alpha1.K8sReleaseService` |
| `dns.api.nebius.cloud` | `dns.v1.RecordService`, `dns.v1.ZoneService` |
| `dpl.kms.api.nebius.cloud` | `kms.v1.AsymmetricCryptoService`, `kms.v1.SymmetricCryptoService` |
| `dpl.mysterybox.api.nebius.cloud` | `mysterybox.v1.PayloadService` |
| `maintenance.msp.api.nebius.cloud` | `maintenance.v1alpha1.MaintenanceService` |
| `mk8s.api.nebius.cloud` | `mk8s.v1.ClusterService`, `mk8s.v1.NodeGroupService`, `mk8s.v1alpha1.ClusterService`, `mk8s.v1alpha1.NodeGroupService` |
| `mlflow.msp.api.nebius.cloud` | `msp.mlflow.v1alpha1.ClusterService` |
| `observability-agent-manager.api.nebius.cloud` | `logging.agentmanager.v1.VersionService` |
| `postgresql.msp.api.nebius.cloud` | `msp.postgresql.v1alpha1.BackupService`, `msp.postgresql.v1alpha1.ClusterService` |
| `pricing-policies.billing-cpl.api.nebius.cloud` | `billing.v1.PricingPolicyService`, `billing.v1alpha1.SpotPriceService` |
| `quota-dispatcher.billing-cpl.api.nebius.cloud` | `quotas.v1.QuotaAllowanceService` |
| `registry.api.nebius.cloud` | `registry.v1.ArtifactService`, `registry.v1.RegistryService` |
| `tokens.iam.api.nebius.cloud` | `iam.v1.TokenExchangeService` |
| `transfer.storage.api.nebius.cloud` | `storage.v1.TransferService`, `storage.v1alpha1.TransferService` |
| `vpc.api.nebius.cloud` | `vpc.v1.AllocationService`, `vpc.v1.NetworkService`, `vpc.v1.PoolService`, `vpc.v1.RouteService`, `vpc.v1.RouteTableService`, `vpc.v1.SecurityGroupService`, `vpc.v1.SecurityRuleService`, `vpc.v1.SubnetService`, `vpc.v1.TargetGroupService`, `vpc.v1alpha1.AllocationService`, `vpc.v1alpha1.NetworkService`, `vpc.v1alpha1.PoolService`, `vpc.v1alpha1.ScopeService`, `vpc.v1alpha1.SubnetService` |

Additional HTTP endpoint: `https://auth.eu.nebius.com/oauth2/token/exchange` (RFC 8693 token exchange, the HTTP twin of
`TokenExchangeService/Exchange`). The mocker serves it at `/oauth2/token/exchange` on the edge port.

## How to read a service doc

Each service doc has the same sections:

1. **Overview**: what the service is in Nebius, and the resource kinds with their NID prefixes.
2. **Emulation design**: the resource model, lifecycle, validation and domain rules, side effects, seed catalogs and
   synthesized outputs; the design rules for the module.
3. **Terraform**: provider objects backed by this area, and the RPCs they call.
4. **Configuration**: module-specific `NEBIUS_MOCKER_<AREA>_*` keys.
5. **Open questions**: behavior to confirm through parity recording.
6. **API surface**: generated tables of every service and RPC, with a ✅ on RPCs the Terraform provider calls.
   Regenerate with `tools/apigen docs`; do not edit by hand.
