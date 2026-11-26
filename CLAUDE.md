# CLAUDE.md

NebiusMocker is an open-source, LocalStack-style emulator of the Nebius AI Cloud API, written in Go. The goal is
**local development against Nebius APIs**: the official Terraform provider, Go SDK and `nebius` CLI must work against it
unmodified. It's a mock, not a cloud. Keep it simple and easy to maintain.

Start with `README.md`, `docs/architecture/` and `docs/services/`. Binding rules live in `.claude/skills/`.

## Principles

- **Fidelity through official tools only.** Tests use the real `nebius/nebius` Terraform provider from the Registry,
  `github.com/nebius/gosdk` and the `nebius` CLI. Point them at the mock only through their own settings
  (`resolvers = { "*" = addr }`, `address_options = { "*" = { insecure = true } }`, `token`). Never write custom
  providers, modules, forks or patches.
- **Build the Nebius logic, adopt the heavy lifting.** API shape, validation, IDs, lifecycles, operations, errors and
  authz are ours. Heavy data planes come from popular open-source projects run in Docker:
  - Harbor for the container registry;
  - RustFS for object storage and blobs;
  - k3s, Postgres and the like for the rest.

  Wrap each behind a port with an in-memory fake (`.claude/skills/add-nebius-service/references/backends.md`). Use
  plain Go where the scale is small, e.g. no Lucene for a few MB of audit logs.
- **Stubs are fine where the user asks for inventory-only semantics.** For example, serverless endpoints return
  `200 "endpoint was called"` and run no container.
- **The protos are the spec.** Types and server stubs come from gosdk. Validation comes from `buf.validate`. Upstream
  sources are cached in `~/.cache/nebius-mocker/upstream`.

## No shortcuts

A test proves a feature only if it takes the path a real user takes. These are **gaps to fix, not deferrals**:

- Tests that bypass the user path, e.g. writing straight into a backend (RustFS, Harbor, the store) where a user would
  upload through the S3 front, the CLI or the documented client.
- Accepting a request field or option and silently ignoring it: filters, `rotation_period`, settings that have no
  effect. Either implement it, or reject it with a clear `InvalidArgument`/`Unimplemented` that names it.
- Environment workarounds that make a test pass in a way a user's machine wouldn't, e.g. proxies or forwarders that only
  exist in the test. Make the documented user setup work, and test exactly that.
- Skipping a documented workflow (for example a CLI credential helper) while claiming its criterion is met.

**What isn't a gap:** deferring *scope* is fine, as long as it is explicit and listed under "Not yet" in the service doc.
Examples: quotas, an RPC nobody calls, features the public API can't reach.

**Enforcement:**
- Leaders audit their own suites against this list before shipping, and say in their report what they checked.
- When the dispatcher finds a shortcut in a report:
  - if the leader is still live, it instructs the leader to fix it;
  - otherwise it starts a follow-up task for it.

  A shortcut is never accepted as "deferred".

## Code rules (enforced, do not weaken)

- Every service area is `services/<area>/{module.go, api/, domain/, dal/}`:
  - `api` holds the views (conversion only);
  - `domain` holds the logic, with its ports in `ports.go`;
  - `dal` is the only code that touches `pkg/store`;
  - `backend/<name>/` holds adapters to adopted systems.
- `services/A` never imports `services/B`. Cross-service needs go through `pkg/` (`pkg/objstore`, `pkg/peer`)
  or the authz hierarchy (`deps.Authz.Parent`/`Root`). `pkg/` never imports `services/`.
- Domain and DAL return `pkg/errs` kinds. Only `api` maps them to gRPC status codes, through `pkg/serviceerror`.
- Never call `time.Now`/`Sleep`, `os.Getenv`, `context.Background`, `fmt.Print*`, `errors.As` or `panic` outside their
  one allowed place. Use `clock.Clock`, `pkg/config`, the caller's ctx, `slog` and `errors.AsType`.
- Every RPC served needs an `authz.Rules` entry (under `enforce`, unknown methods are denied), and every resource kind
  needs `deps.Authz.RegisterKind`.
- The skills hold the details: `go-architecture`, `go-style`, `go-errors`, `go-concurrency`, `go-patterns`,
  `go-testing`, `go-lint`. They are enforced by `.golangci.yml` (depguard/forbidigo) and `test/arch`. Fix the code,
  never the linter. Every `nolint` needs a specific linter name and a reason.

## Gates: there is no CI budget

Nothing runs in CI. These local gates are the only gates, so run them for real and read the output:

```bash
make check        # lint (0 issues) + go test -race -shuffle + test/arch. Docker-free; the pre-commit hook (make hooks)
make e2e          # Terraform end-to-end suites with the official provider (needs terraform + registry access)
make e2e-<area>   # Docker-backed flows (Harbor, RustFS, CLI), each brings its backend up and down
```

- Don't add GitHub Actions or other CI workflows unless asked.
- Never weaken a test, criterion or lint rule to get green. A bug found by a test gets fixed in the code.
- An empty `terraform plan` after apply is the key fidelity check. A drift means the mock's `Get` doesn't round-trip.

## Git workflow

- Commit and **push directly to `main`**. Don't create branches or PRs, except the worktree branches used by
  orchestrated agents.
- Before pushing: `git fetch origin && git rebase origin/main`, then re-run the gates if the rebase touched code.
  Never force-push.
- Commit messages explain *why*, and end with the `Co-Authored-By` trailer from the session instructions.
- Never commit secrets, backend data volumes, downloaded binaries or caches.

## How we work: skills and agents

- **New or updated Nebius service:** use the `add-nebius-service` skill. It always starts with
  `scripts/inventory.py <area>`, a gap report against the protos, the provider and gosdk, so rerunning it on an existing
  service only fills the delta. Done means full CRUD via Terraform *and* the Go SDK, plus unit, contract and e2e tests.
  Each service doc records a `Verified against: nebius/api@…, provider …, gosdk …` line.
- **Terraform support status:** use the `terraform-sweep` skill (`/terraform-sweep`). It compares the mock with the
  newest provider release and updates `docs/terraform.md`, `docs/services/terraform-coverage.md`, the README notes and
  the service docs. Run it after a service lands or a provider release comes out.
- **Features and parallel work:** use the `orchestrate-task` skill, e.g. `/orchestrate-task /add-nebius-service <area>`.
  - The main session is the **dispatcher**. It doesn't do the task itself.
  - Each task gets one **leader**, always `model: opus`, in its own git worktree (`isolation: worktree`), running in the
    background. The leader is accountable for quality, verifies every gate itself and ships.
  - Leaders pick **worker models by complexity** (`orchestrate-task/references/model-selection.md`):
    - **haiku** for mechanical work: run and summarize tests, renames, version bumps, extraction;
    - **sonnet** by default, for implementation that follows an existing pattern;
    - **opus** only for design, debugging with an unknown cause, or cross-cutting integration.

    Escalate one tier only after a failed attempt. Run at most 3 workers at a time, keep worker reports short, and give
    precise briefs. Be token-efficient.
  - **Coordination** uses the shared ledger
    (`python3 -I .claude/skills/orchestrate-task/scripts/ledger.py`, stored in git's common dir and never committed):
    - claims and conflict checks before editing shared files;
    - `provide`/`need` for cross-task artifacts, with shared pieces landing early as small commits;
    - a push lock that serializes rebase, test and push to `main`.

    The dispatcher relays new artifacts to dependent leaders. It verifies each reported commit on `origin/main` with
    `make check` before removing that leader's worktree.
- Spawn agents only for these flows or when asked. For small changes, work directly.

## Local defaults (useful when testing)

| Thing | Value |
|---|---|
| gRPC edge (all services) | `:4566` |
| Data-plane ports | S3 front `4580`, registry front `4590`, AI endpoints `4600` |
| Seeded tenant / project | `tenant-e00mock0000000000default` / `project-e00mock000000000default` (eu-north1) |
| Dev tokens | `nmock-dev-admin`, `nmock-dev-editor`, `nmock-dev-viewer` (members of the seeded admins/editors/viewers groups) |
| Authz | `NEBIUS_MOCKER_AUTHZ=off\|audit\|enforce`. The default `audit` only logs; tests use `enforce` |
| Config | `NEBIUS_MOCKER_*` env vars, read only in `pkg/config` (see `docs/architecture/11-configuration.md`) |
| Pinned upstream | Terraform provider `0.6.91`, gosdk in `go.mod`. Bump them through `add-nebius-service` refresh runs |
| Environment | Apple Silicon (arm64), Go 1.26, Docker Desktop. Backend images must have arm64 builds |
