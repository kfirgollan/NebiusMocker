# ADR-0003: Single Go module with public `pkg/` libraries

**Context.** We want reusable libraries (embed the mocker in Go tests) and many binaries. Multi-module repos add release
overhead and replace-directive friction while the APIs are still changing.

**Decision.** One module, `github.com/kfirgollan/NebiusMocker`. Public API under `pkg/` and `services/`. Binaries under `cmd/`.
Dependency rules are enforced by a lint check (`services/A` must not import `services/B`, `pkg` must not import `services`).

**Consequences.** One version number for everything. Embedding users pull all service packages, but the Go linker drops
unused code. If independent versioning becomes necessary, split along the existing package boundaries.
