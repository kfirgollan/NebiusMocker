---
name: add-nebius-service
description: Add a Nebius service area to NebiusMocker, or refresh an existing one against upstream changes. Runs a gap report against the Nebius protos, the official Terraform provider and the Go SDK; builds or extends the module, using a popular open-source backend for heavy lifting (Harbor, RustFS, …); proves full CRUD with Terraform and the Go SDK plus unit/contract/e2e tests; then merges to main and pushes. Reentrant — safe to run again on a service that exists. Use when asked to add, implement, support, extend, update or refresh a Nebius service (compute, vpc, registry, storage, mk8s, …) or to catch up with a new provider/SDK release.
---

# add-nebius-service

**Goal:** the official Nebius tools (Terraform provider, Go SDK, `nebius` CLI) work against NebiusMocker for this
service, so people can develop locally. Implement the **Nebius-specific behavior**: API shape, validation, lifecycle,
IDs, operations, errors, authz, quotas. Adopt a well-known open-source system for any heavy data plane. Keep
everything simple and easy to maintain. This is an emulator, not a cloud.

Input: an area name as listed in `docs/services/README.md` (`compute`, `vpc`, `registry`, `storage`, `mk8s`,
`msp-postgresql`, …), plus an optional scope ("images only", "the new X resource").

Before writing code, read the project rules. They are binding: `go-architecture`, `go-style`, `go-errors`,
`go-concurrency`, `go-patterns`, `go-testing`, `go-lint`.

## 1. Gap report (always first; this is what makes the skill reentrant)

```bash
python3 -I .claude/skills/add-nebius-service/scripts/inventory.py <area>            # newest provider tag
python3 -I .claude/skills/add-nebius-service/scripts/inventory.py <area> --json     # for scripting
go list -m -versions github.com/nebius/gosdk | tr ' ' '\n' | tail -1                 # newest gosdk
```

The script syncs `nebius/api` and `terraform-provider-nebius` into `~/.cache/nebius-mocker/upstream`. Per RPC it
reports whether the repo **implements** it, has an **authz rule** and a **test**, and whether **Terraform uses** it. Per
Terraform resource, data source and ephemeral resource it reports whether an e2e test covers it. Its detection is
heuristic, so confirm each row in the code.

Then read what the report can't tell you:
- The product docs at `https://docs.nebius.com/<service>`: behavior, limits, naming rules, workflows, CLI commands.
- The provider's `generated/nebius/<area>/**_service.tfgen.go`: exactly which RPCs each TF object calls, its
  `RequiresReplace` (immutable) attributes, and computed fields that must round-trip without drift.
- The provider `CHANGELOG.md` between the version the tests pin and the newest one.
- `docs/services/<area>.md`, including its **Implementation status** section, which records the versions last verified.

**Decide the mode:**

| Situation | Mode |
|---|---|
| `services/<area>` does not exist | **New service**: do every step |
| It exists and the report or changelog shows ❌ rows, new TF objects/attributes, new RPCs/fields, or a newer gosdk/provider | **Refresh**: do steps 2–7 for the delta only, and bump the pins |
| No gaps and the pins are current | **Done**: report "up to date" with the report summary. Change nothing |

Write the plan as a short checklist: RPCs, TF objects, CLI commands and backend work, in priority order. Terraform-used
RPCs come first, then the remaining SDK RPCs, then CLI flows. Get agreement before large scope changes.

## 2. Decide what is real and what is adopted

Read [references/backends.md](references/backends.md). In short:
- Control-plane logic (resources, validation, state machines, operations, authz) is **always ours**, built on `pkg/*`.
- A heavy data plane (object bytes, OCI images, Kubernetes, databases) is **adopted**. Use a popular, maintained,
  permissively licensed project run from its official Docker image, behind a domain **port**, with an in-memory fake for
  Docker-free tests. Never reimplement those systems.
- Shared adapters go in `pkg/` (for example `pkg/objstore`), because `services/A` must not import `services/B`.

## 3. Build (new) or extend (refresh)

Follow the IAM module (`services/iam`) as the template:
- [ ] `services/<area>/module.go`: implements `mocker.Module`, plus `mocker.Lifecycle` if it runs listeners or backends.
- [ ] `domain/` holds logic and `ports.go`. `dal/` is the only code that touches `pkg/store`. `api/` holds thin views
      that embed `Unimplemented…Server`. Adapters go in `backend/<name>/`.
- [ ] Register every gRPC service in `module.go`, and add the module to `cmd/nebius-mocker/main.go`.
- [ ] Add an `authz.Rules` entry for **every** RPC served (`rules.CRUD(...)` plus custom verbs). Call
      `deps.Authz.RegisterKind(prefix, parentLookup)` for every resource kind. Under `enforce`, unknown methods are denied.
- [ ] Parents are validated through a `Hierarchy` port (`deps.Authz.Parent`/`Root`), never by importing another area.
      Most cloud resources live in the seeded project (`config.DefaultProjectID`).
- [ ] Apply the Nebius conventions from `docs/architecture/05-api-conventions.md`:
      - NIDs and metadata (`resource_version`, timestamps);
      - name uniqueness per parent;
      - `IMMUTABLE` fields;
      - `X-ResetMask` updates (`fieldmaskpb.PatchWithResetMask`, then restore server-owned fields);
      - async operations through `deps.Ops.Submit` (validation in `Prepare`, deferred effects in `Work`);
      - `pkg/errs` kinds, never gRPC codes, in domain and dal.
- [ ] Seed catalogs (public images, platforms, versions) through config-overridable defaults.
- [ ] New configuration lives in `pkg/config` (`NEBIUS_MOCKER_<AREA>_*`), each with a working default.

Keep it small. Implement what the official clients call and what the docs describe. Leave the rest as `UNIMPLEMENTED`
and list it under "Not yet" in the service doc.

## 4. Prove it: definition of done

The full matrix is in [references/done-checklist.md](references/done-checklist.md). Minimum:
1. **Unit tests**: domain rules with fakes; DAL against `store.NewMemory()`.
2. **Contract tests** (`test/contract`): the official gosdk client over real gRPC, covering every implemented RPC, plus
   error kinds and authz per role (`testkit.WithAuthz(authz.ModeEnforce)`).
3. **Terraform e2e** (`test/terraform/<area>`, tag `e2e`): the **official, unmodified** `nebius/nebius` provider from the
   Registry, pointed at the mock only through `resolvers`, `address_options` and `token`. No custom providers or modules.
   For every resource: create, empty plan, in-place update, empty plan, (replace where attributes are immutable),
   destroy. Check the mock's state with the gosdk after every step, and cover data sources. Run under
   `authz.ModeEnforce`.
4. **Go SDK CRUD** of the service's resources (and data-plane objects, using the client Nebius documents).
5. **CLI and tool flows** the docs show (`nebius ...`, `docker ...`, …), behind a Docker build tag
   `e2e_<area>` with `make e2e-<area>` that brings its backend up and tears it down.
6. `make check` (lint + race tests + `test/arch`) stays **Docker-free and green**. `make e2e` stays green.

A bug found by a test gets fixed in the code, never by loosening the test. Never weaken `.golangci.yml`.

## 5. Document

- `docs/services/<area>.md` → **Implementation status** table, plus a line
  `Verified against: nebius/api@<sha>, terraform-provider-nebius <tag>, gosdk <version>` (from the gap report).
  The next refresh starts from this line.
- `docs/services/README.md` tiers, the README quick start if user-facing behavior changed, and the config docs.

## 6. Ship

```bash
make check && make e2e && make e2e-<area>     # all green, or do not ship
git add -A && git commit                       # message ends with the Co-Authored-By trailer
git fetch origin && git rebase origin/main     # re-run checks if the rebase touched code
git push origin HEAD:main                      # never force-push
```

If you worked in a git worktree, remove it after the push is confirmed (`git worktree remove <path>` from the main
checkout, then delete the branch). If any done criterion fails, **do not push**. Report what fails and why.

## 7. Report

Report the mode, the gap-report summary before and after, what was built or extended, backend choices, the exact test
commands with pass/fail lines, the pushed commit hash, and anything deferred or approximated.
