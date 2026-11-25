# Architecture

Read these in order. Each document is self-contained, but later ones assume the vocabulary of earlier ones.

| # | Document | Summary |
|---|---|---|
| 01 | [Goals and scope](01-goals-and-scope.md) | What we emulate, how faithfully, and what we deliberately do not do |
| 02 | [System overview](02-system-overview.md) | Components, the LocalStack mapping, a request's life end to end |
| 03 | [Repository layout](03-repository-layout.md) | Go module, shared libraries, service packages, binaries |
| 04 | [Request pipeline](04-request-pipeline.md) | Transport, routing, interceptor chain, headers |
| 05 | [API conventions](05-api-conventions.md) | Resource model, IDs, metadata, Update/reset-mask, List, errors, validation |
| 06 | [IAM and authorization](06-iam-and-authorization.md) | Tokens, service-account exchange, tenants/projects, groups, access permits, enforcement modes |
| 07 | [State store](07-state-store.md) | Storage interface, backends, persistence, snapshots, cross-service references |
| 08 | [Resource lifecycle and operations](08-resource-lifecycle.md) | Operation engine, state machines, timing, emulation backends |
| 09 | [Quotas, limits and fault injection](09-quotas-limits-faults.md) | Quota accounting, rate limits, capacity, chaos |
| 10 | [Deployment topologies](10-deployment-topologies.md) | Single service, all-in-one, multi-container; images, ports, TLS |
| 11 | [Configuration](11-configuration.md) | Environment variables, config file, seed data, init hooks |
| 12 | [Test harness](12-test-harness.md) | Unit, contract, Terraform end-to-end, parity, API drift detection |
| 13 | [Milestones](13-milestones.md) | Delivery plan; M1 = the Terraform provider works end to end |
| — | [Decisions (ADRs)](decisions/README.md) | Recorded architectural decisions and their rationale |
