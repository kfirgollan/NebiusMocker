# Definition of done

Copy this checklist into your plan. In refresh mode, only the delta rows apply, but every suite must still be green.

## Coverage matrix

| What | How | Where | Gate |
|---|---|---|---|
| Domain rules (validation, immutability, state machine, side effects) | Table tests with in-package fakes and `clock.Manual` | `services/<area>/domain/*_test.go` | `make test` |
| DAL (round-trip, uniqueness, paging, not-found) | Against `store.NewMemory()` | `services/<area>/dal/*_test.go` | `make test` |
| Every implemented RPC over gRPC | Official gosdk client via `testkit.StartT` + `inst.SDK(t)` | `test/contract/<area>_*_test.go` | `make test` |
| Error kinds on the wire | `status.Code` + `ServiceError` detail type | contract tests | `make test` |
| Authz per role | `testkit.WithAuthz(authz.ModeEnforce)`, tokens `config.Default{Viewer,Editor,Dev}Token`, an outsider via `WithTokens` | contract tests | `make test` |
| Async operations | `testkit.WithOperationDelay(…)` so `op.Wait` really polls | contract + e2e | `make test` |
| Every TF resource: create → empty plan → in-place update → empty plan → destroy | Real `terraform` + official `nebius/nebius` | `test/terraform/<area>` (tag `e2e`) | `make e2e` |
| Immutable attributes plan **replace** | Change one `RequiresReplace` attribute and expect `replace` | same | `make e2e` |
| Every TF data source | Read by id, by name, special lookups | same | `make e2e` |
| Mock state matches TF state after each step | gosdk `Get`/`List` vs `terraform show` | same | `make e2e` |
| Data plane / CLI / docker flows from the docs | Real tools against the real backend | `test/<area>` (tag `e2e_<area>`) | `make e2e-<area>` |
| Layering, imports, banned calls | `.golangci.yml` + `test/arch` | — | `make check` |

## Terraform e2e rules

- Use the provider version from the gap report. Update the `providerVersion` pin when refreshing.
- Point it at the mock only through the provider block: `resolvers = { "*" = addr }`,
  `address_options = { "*" = { insecure = true } }`, `token`. Run with an empty `TF_CLI_CONFIG_FILE`, so no
  dev_overrides or mirrors can swap in a different binary.
- Run under `testkit.WithAuthnRequired()` + `testkit.WithAuthz(authz.ModeEnforce)`.
- Seeded objects (default groups, public catalogs) are excluded from "the mock holds exactly the state" assertions.
- An empty plan after apply is the main fidelity check. A drift there means the mock's `Get` doesn't round-trip, so fix
  the mock.

## Docker-gated tests

- Use the build tag `e2e_<area>` and a Makefile target `e2e-<area>`. The helper starts the backend, waits for health,
  uses unique names and always cleans up (`t.Cleanup`), even on failure.
- Pin image tags. Check that they run on arm64.
- Install tools like the `nebius` CLI into a sandbox (`test/internal/nebiuscli`), never system-wide.

## No shortcuts

Every test must take the user's path (see CLAUDE.md "No shortcuts"):
- E2E tests never write directly into a backend. Data goes in the way users put it in: through the S3 front, the CLI, or
  the documented SDK.
- Every accepted field has an effect. If one doesn't, reject it explicitly by name.
- No test-only proxies or forwarders. The documented user setup must work as is.
- Every documented workflow for a criterion is exercised, e.g. a CLI credential helper as well as token login.

Scope may be deferred, but only when it is listed in the service doc's "Not yet". Shortcuts may not.

## Ship gate

```bash
make check            # Docker-free: lint (0 issues) + go test -race -shuffle + test/arch
make e2e              # every Terraform suite, not only yours
make e2e-<area>       # your Docker-gated flows
```

All three must pass on the rebased commit you push.
