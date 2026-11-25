# 01 — Goals and scope

## Problem

Nebius infrastructure code (Terraform modules, CLI scripts, Go and Python SDK programs, operators) can only be tested
against a real tenant today. That has four costs:

- **Money and capacity.** GPU platforms are expensive and often unavailable.
- **Speed.** An `mk8s` cluster or a GPU instance takes minutes to provision.
- **Isolation.** CI jobs share quotas and can leak resources.
- **Access.** Open-source contributors to Nebius-related tooling may not have a tenant at all.

LocalStack solved the same problem for AWS. NebiusMocker brings that model to Nebius.

## Goals

1. **Wire compatibility.** Serve the exact gRPC services, methods and messages from [`nebius/api`](https://github.com/nebius/api),
   so that unmodified official clients work: the Terraform provider, the `nebius` CLI, `gosdk` and `pysdk`.
   The only client-side change is where it connects, set through the provider's `resolvers`, `domain` and `address_options`.
2. **Behavioral fidelity.** Respond the way Nebius does, not just in the right shape:
   - async `Operation`s that complete over time and are polled through `OperationService`;
   - resource state machines (`CREATING → READY`, `STARTING → RUNNING`, …);
   - `X-ResetMask` full-replace Update semantics;
   - `X-Idempotency-Key`;
   - `buf.validate` constraints, `IMMUTABLE` fields and name uniqueness;
   - referential integrity (a subnet needs a network, which needs a project);
   - `ServiceError` details on failures.
3. **Security semantics.** Authenticate bearer tokens, run the service-account JWT → IAM token exchange, and enforce
   authorization from IAM groups and access permits. Enforcement can be switched off for convenience (see [06](06-iam-and-authorization.md)).
4. **Limits.** Enforce quotas (`QuotaAllowance`), per-resource limits, rate limits and capacity, and return the real error
   types (`QuotaFailure`, `TooManyRequests`, `NotEnoughResources`).
5. **Composable distribution.** Ship Go libraries plus binaries. Run one service per container, every service in one
   container, or any split in between (see [10](10-deployment-topologies.md)).
6. **Testable by design.** Include a harness that proves the emulator matches the API and that Terraform works against it
   (see [12](12-test-harness.md)).

## Non-goals (for now)

| Non-goal | Why | Possible later |
|---|---|---|
| Running real VMs or GPUs | A laptop or CI box cannot do it, and it is not needed for IaC testing | Optional container- or VM-backed instances ([08](08-resource-lifecycle.md#emulation-backends)) |
| Real Kubernetes for every `mk8s` cluster by default | Heavy | Opt-in `k3s` backend, the way LocalStack runs EKS (see [services/mk8s.md](../services/mk8s.md)) |
| Billing accuracy | Prices change, and we are not a pricing oracle | Calculator returns deterministic, configurable prices |
| Console or web UI | Out of scope | A read-only state browser could reuse the admin API |
| Private, internal or undocumented Nebius APIs | We emulate only the public `nebius/api` surface | — |
| Bug-for-bug compatibility | We follow documented behavior. Where docs are silent, we copy observed behavior and record it in parity tests | — |

## Fidelity tiers

Not every service needs the same depth. Each service doc in [`docs/services/`](../services/README.md) declares a tier:

| Tier | Name | Meaning |
|---|---|---|
| **T0** | Unimplemented | Endpoint is registered and returns `UNIMPLEMENTED` with a clear message. The provider tolerates this for `PreflightCheck`. |
| **T1** | Stateful CRUD | Create/Get/GetByName/List/Update/Delete with validation, metadata, NIDs, operations, reset-mask, authz and quotas. No side effects beyond state. |
| **T2** | Behavioral | T1, plus realistic lifecycles, cross-resource side effects (an instance's managed boot disk; address allocation from subnet pools), custom verbs (Start/Stop), seed catalogs and synthesized output fields (IPs, endpoints, certificates). |
| **T3** | Functional data plane | T2, plus a working data plane: an S3 API for buckets, a container registry for `registry`, real crypto for KMS, a real Kubernetes API for `mk8s`. |

Milestone 1 brings every service the Terraform provider touches to at least T1, and the core of the
provider's quickstart (IAM, VPC, Compute, mk8s, Storage, Registry, MysteryBox) to T2. See [13](13-milestones.md).

## Guiding principles

- **The protos are the spec.** We never hand-write message types. Types and server stubs come from `gosdk/proto`.
  Validation comes from the `buf.validate` annotations. Field behaviors come from `nebius/annotations.proto`.
- **Generic first, specific second.** About 80% of behavior (metadata, NIDs, CRUD, reset-mask, idempotency, operations,
  authz, quotas) is shared framework. A service package adds only domain rules.
- **Deterministic by default, realistic on request.** Tests need reproducible IDs, timing and IPs. Seeding and a
  "fast" timing profile make runs reproducible. A "realistic" profile adds delays and jitter.
- **Fail like Nebius.** The same gRPC code, the same `ServiceError` detail type, the same `retry_type`.
  Clients' retry and error-handling paths get exercised as a result.
- **Observable.** Every request, operation and state change shows up in structured logs and the admin API,
  so a developer can see why a request failed.
