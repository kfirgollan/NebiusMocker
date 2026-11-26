---
name: terraform-sweep
description: Compare NebiusMocker with the newest official nebius/nebius Terraform provider. Lists every resource, data source and ephemeral resource the provider documents, maps each to the gRPC calls it makes, checks which ones the mock serves and which ones have Terraform tests, then updates docs/terraform.md, docs/services/terraform-coverage.md, the README Terraform notes and the affected service docs. Use when asked for a Terraform sweep, Terraform coverage or support status, what's missing against the provider, or after a new provider release or a service lands.
---

# terraform-sweep

**Goal:** the Terraform docs say exactly what works against the mock today, checked against the newest provider
release. The sweep **documents** status. It doesn't implement services, bump the provider pin or touch code.

## 1. Run the sweep

```bash
S=.claude/skills/terraform-sweep/scripts/sweep.py
python3 -I $S                       # Markdown report on stdout
python3 -I $S --json > <scratchpad>/sweep.json   # the same data, for scripting
python3 -I $S --provider-ref v0.6.91             # a specific provider tag instead of the newest
```

The script syncs `nebius/api` and `terraform-provider-nebius` into `~/.cache/nebius-mocker/upstream` (newest `v*`
tag by default). For each object in the provider's `docs/{resources,data-sources,ephemeral-resources}`, it:

| Step | How |
|---|---|
| Finds the calls the provider makes | Generated code (`generated/**`): the struct whose `GetName()` returns the type, its `New<X>Service` constructor and `service.<RPC>(ctx` calls. Resources use `Read/GetByName/Create/Update/Delete`, data sources `Read/GetByName/GetAdditionalGetters`, ephemeral resources `Read`. Hand-written objects (`nebius_hash`, `nebius_parent_id`, `nebius_iam_token`) fall back to `docs/services/terraform-coverage.md` |
| Checks the mock serves them | `services/**` and `pkg/**` (non-test): `<alias>.Register<Svc>Server(` where the alias imports that exact `gosdk/proto/nebius/<area>/<version>` package, plus a handler method `<RPC>(ctx context.Context, req *<alias>.<Request>)` with the proto's request type. Mutations also need `OperationService/Get` of the matching version (`common.v1` or `common.v1alpha1`) |
| Finds tests | HCL in Go files under `test/`: `resource|data|ephemeral "nebius_…"`, including templated blocks such as `data "nebius_vpc_v1_%[1]s"` whose suffixes appear as quoted strings in the same file |

Statuses:

| Status | Meaning |
|---|---|
| ✅ | Every call is served and a test uses the object |
| ⚠️ No Terraform test | Every call is served, but no test uses the object in HCL |
| ❌ Partial | Some of the object's own calls are served (the report lists the missing ones) |
| ❌ Missing | None of its calls is served |
| ➖ Provider-local | The provider computes it without an API call |

The report also lists **terraform-coverage.md drift**: objects whose service or calls differ from that doc, objects
the doc lacks, and objects the provider dropped.

## 2. Check the report before trusting it

The detection is heuristic. Confirm in the code before writing:
- **Every status change**, compared with the current `docs/terraform.md`.
- **Every ✅ row whose only test is outside `test/terraform/`:** a test file that merely mentions the object doesn't
  prove the full create, empty plan, update, empty plan, replace and destroy cycle for resources. Open the test.
- **Every ❌ Partial row:** find out why the remaining calls are missing.
- **New provider objects and drift lines:** read their generated code.

If the newest provider is newer than the pin (`test/internal/tfharness/tfharness.go` `ProviderVersion`), the statuses
describe the mock against the newest provider, but the tests still run the pinned one. Say so in the version line of
`docs/terraform.md`, list the changed objects from the provider `CHANGELOG.md`, and recommend an `add-nebius-service`
refresh. **Don't bump the pin in a sweep**: that needs the full gates.

## 3. Update the docs

1. **Coordinate first.** Run `python3 -I .claude/skills/orchestrate-task/scripts/ledger.py show`. If an active task
   claims `docs/terraform.md` or a service doc you're about to edit, work in the main checkout anyway, but push under
   the push lock (step 4). Running leaders rebase over your change.
2. **`python3 -I $S --write`** updates `docs/terraform.md` in place:
   - status cells, keeping each row's Notes;
   - rows added for new objects, with empty notes, and rows dropped for removed objects;
   - the summary table;
   - the provider version, date and `main` commit in the intro.

   It prints every change it made.
3. **By hand, in `docs/terraform.md`:**
   - notes for added rows and for rows whose status changed (a ⚠️ row says what covers it; a ✅ row notes real
     behavior or limits, linking the service doc);
   - the "What is missing, by area" table;
   - the "Not modeled" list, if it changed.
4. **`docs/services/terraform-coverage.md`:** fix every drift line, add rows for new objects with their service and
   calls (from the generated code) and a tier, and update the totals line.
5. **`README.md` → "Terraform notes":** the coverage counts and the main gaps.
6. **Service docs (`docs/services/<area>.md`):** for each area with new or changed objects, update its **Terraform**
   table. Add an Implementation status row only when the mock's status changed.
7. **`docs/services/README.md`:** the "Used by Terraform" counts, if objects were added or removed.

## 4. Ship

The sweep changes documentation only, so the gate is `make check` (the pre-commit hook runs it).

```bash
L=.claude/skills/orchestrate-task/scripts/ledger.py
python3 -I $L push-lock terraform-sweep acquire     # only if leaders are active; exit 3 = busy, wait and retry
git add docs/ README.md && git commit               # message: what changed and why; Co-Authored-By trailer
git fetch origin && git rebase origin/main && git push origin HEAD:main   # never force-push
python3 -I $L push-lock terraform-sweep release
```

If the rebase conflicts in `docs/terraform.md`, take `origin/main`'s version, run `--write` again, and redo the notes
by hand.

## 5. Report

- The provider version compared, and the pin.
- The summary counts before and after.
- Every status change, with its evidence.
- New and removed provider objects.
- The drift fixed.
- Anything you overrode by hand after checking the code, and why.
- The pushed commit.
- For each missing area, the follow-up command: `/orchestrate-task /add-nebius-service <area>`.
