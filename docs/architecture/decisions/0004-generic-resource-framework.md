# ADR-0004: Generic, descriptor-driven resource framework

**Context.** There are 91 services and 488 RPCs. Most of them follow the same `metadata/spec/status` +
Get/GetByName/List/Create/Update/Delete + Operation pattern, with behavior encoded in annotations
(`field_behavior`, `resource_behavior`, `buf.validate`, `nid`).

**Decision.** Implement CRUD, validation, immutability, naming, NIDs, operations, quotas and authz once, generically, over
`protoreflect`, configured by a `resource.Kind` declaration. Modules add hooks for domain rules and custom verbs only.

**Consequences.** New services reach T1 quickly. Invariants are uniform and tested once by the conformance kit. The cost is
reflection overhead (irrelevant at emulator scale) and a framework that must stay general enough for odd resources
(unnamed resources, singleton-per-parent resources, v1alpha1 operations).
