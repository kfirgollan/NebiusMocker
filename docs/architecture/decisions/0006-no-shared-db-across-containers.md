# ADR-0006: No shared database across containers; peers talk through public APIs

**Context.** Split deployments (one container per service) need cross-service checks: token validity, permits, whether a
referenced resource exists. A shared database would couple the containers and hide boundary bugs.

**Decision.** Each container owns its modules' state. Cross-module needs go through `peer.Directory`. In-process it is a
direct call. Remotely it is a call to the owning container's **public** Nebius RPCs, plus one internal authz-check RPC
behind an internal token. Tokens are stateless HMAC with a shared secret.

**Consequences.** Split and all-in-one deployments behave the same. There are no cross-container transactions, so a
delete in one container can race a reference created in another. We accept that for an emulator.
