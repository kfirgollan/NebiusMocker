# 13 — Milestones

## M0 — Foundations

These are the framework pieces every service needs. No service is "done" in M0, but `iam` and `vpc.Network` run end to
end as the pilot.

- [x] Go module, local gate (`make check`: lint + `go test -race`) — CI, Dockerfile and release workflow still to do
- [x] `pkg/edge`: h2c listener, reflection, health (the "service not enabled" stub is still to do)
- [x] Interceptors: request-id, recovery, logging, authn (dev tokens), protovalidate (authz and idempotency still to do; reset mask is read in the api layer)
- [ ] `pkg/resource`: generic CRUD, NIDs, metadata rules, field behaviors from annotations, paging
- [x] `pkg/operations`: engine, single-goroutine scheduler, per-resource locks, `OperationService` v1 (v1alpha1 still to do)
- [x] `pkg/store`: memory backend with name/parent indexes (references still to do)
- [x] `pkg/serviceerror`
- [ ] `tools/apigen`: handler stubs (all 91 services registered, T0 by default) + doc tables
- [ ] `tools/tfcoverage`
- [ ] `pkg/testkit` + conformance kit (C1–C15) running on the pilot kinds
- [x] Terraform runner (`test/terraform`), with IAM groups as the pilot instead of a VPC network

**Exit criteria:** `terraform apply && terraform plan` (empty) `&& terraform destroy` passes for a `nebius_vpc_v1_network`,
using the unmodified registry provider against the all-in-one binary.

## M1 — The Terraform provider works

**Goal:** every resource, data source and ephemeral resource in `nebius/nebius` (pinned version **0.6.91**, and the
two previous minors) passes its Terraform scenario against NebiusMocker.

Scope (from [services/terraform-coverage.md](../services/terraform-coverage.md)):

- **50 resources, 62 data sources, 4 ephemeral resources**, backed by **60 gRPC services / 262 RPCs** plus
  `OperationService` and `TokenExchangeService`.
- Tier **T2** for the provider quickstart path and the most common resources: IAM (projects, SAs, keys, groups, permits),
  VPC, Compute (disk, instance with managed disks, filesystem, image data source, GPU cluster), mk8s (cluster + node group,
  including `PreflightCheck`), Storage bucket, Registry, MysteryBox (secret, version, payload).
- Tier **T1** for everything else the provider touches: DNS, KMS, quotas, billing pricing policy, capacity allowance and
  block group, tunnel, applications k8s release, AI endpoint, MSP PostgreSQL and MLflow, IAM federation and invitations,
  storage inventory and transfer, vpc v1alpha1.
- Tier **T0** (`UNIMPLEMENTED`) for services the provider does not use. Examples: audit, logging, billing calculator,
  maintenance, ai jobs and devlabs.
- Auth: dev tokens, **service-account JWT exchange** (`service_account` provider block and credentials file), static
  token, impersonation.
- Authz: `audit` and `enforce`, with the `negative/permission-denied` scenario.
- Quotas: ledger and catalog defaults; `QuotaAllowance` live usage; `negative/quota-exceeded`.
- Topologies: **all-in-one and single-service images**. Split + gateway is stretch.
- `tfnebiuslocal` wrapper and the GitHub Action.

**Exit criteria:**

1. `test/terraform` is green: every scenario passes apply → empty plan → update → empty plan → destroy → empty state.
2. Every upstream `examples/resources/*/resource.tf` passes as an auto-generated scenario.
3. The provider quickstart guide config (`docs/guides/quickstart.md`) runs verbatim, with only the override file added.
4. The coverage report shows 116/116 Terraform objects passing for the pinned provider version.
5. Contract kit C1–C15 is green for every T1+ kind.

## M2 — CLI, SDKs and parity

- `nebius` CLI smoke suite (`nebius iam`, `nebius compute`, `nebius vpc`, `nebius mk8s cluster get-credentials` …), pointed
  at the mocker through its endpoint override config.
- `pysdk` smoke suite.
- Parity record/replay infrastructure. Golden transcripts for every T2 kind, recorded against a real tenant.
- Split-container topology with gateway, `peer` remote implementation, and the e2e topology suite.
- Persistence (bolt + snapshot on shutdown) and the snapshot admin API.
- Fault-injection API, rate limits and capacity simulation.

## M3 — Data planes (T3)

- `mk8s` `k3s` backend: working kubeconfig, and the Kubernetes and Helm Terraform providers can deploy into it.
- `storage` S3 data plane with IAM access keys; the Terraform S3 state backend guide works.
- `registry` OCI data plane: `docker push` and `pull`.
- `kms` real crypto; `mysterybox` payload versioning parity.

## M4 — Breadth

- Bring the remaining T0 services to T1: audit events (generated from our own request journal), logging export,
  billing calculator (configurable price table), AI jobs and devlabs, maintenance, capacity intervals and advice.
- Helm chart; testcontainers modules for Go, Python and Java.
- Drift automation fully hands-off: a nightly PR bumps `gosdk`, regenerates, and runs the suite.
