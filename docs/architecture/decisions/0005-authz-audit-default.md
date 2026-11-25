# ADR-0005: Authorization defaults to `audit` mode

**Context.** LocalStack does not enforce IAM by default, and that is a big part of why it is pleasant to use. Our goals
still include checking permissions.

**Decision.** Three modes: `off`, `audit` (default for `dev`), `enforce` (default for the `ci`/`strict` profiles).
Audit mode evaluates and logs every decision and serves denials at `/_mocker/authz/denials`.

**Consequences.** New users succeed on the first try. Teams can still turn on enforcement and use the audit output to
write least-privilege access permits. The role catalog is our own model (the public API does not publish role
definitions), refined through parity tests.
