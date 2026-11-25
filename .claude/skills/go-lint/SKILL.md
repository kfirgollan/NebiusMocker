---
name: go-lint
description: How NebiusMocker enforces its Go rules — the golangci-lint v2 config (.golangci.yml), depguard layer and DAL import bans, forbidigo bans (time, env, context, print, panic), importas proto aliases, the test/arch architecture test for per-area layering, nolint discipline, and the local make lint / pre-commit workflow (no CI budget). Use when a lint fails, when adding a dependency or a new package/area, when changing lint config, or before committing Go code.
---

# Linting and architecture enforcement

Rules that are not enforced by a tool erode. Every rule in the `go-architecture`, `go-style`, `go-errors` and
`go-concurrency` skills that *can* be checked mechanically is checked by one of three tools:

| Tool | Checks | Runs in |
|---|---|---|
| `golangci-lint` v2 (`.golangci.yml`) | Import bans by file glob (depguard), banned calls (forbidigo), errors, ctx, protos, style, complexity | `make lint` |
| `test/arch` (Go test) | Relative layering that globs can't express: api → domain ← dal, no cross-area imports, `pkg` ↛ `services` | `make test` |
| `go vet` / `go test -race` | waitgroup misuse, printf, copylocks, races | `make test` |

**There is no CI budget**, so enforcement is local: `make check` (= `fmt` + `lint` + `test`) is run by a git
pre-commit hook (`make hooks` installs it). Never bypass it with `--no-verify` for Go changes.

## What each depguard rule protects

| Rule | Meaning |
|---|---|
| `storage-engines-only-in-store` | `go.etcd.io/bbolt`, `database/sql` and any DB driver/ORM may only be imported under `pkg/store/**` |
| `store-only-in-dal` | `pkg/store` may only be imported by `services/*/dal`, `services/*/module.go`, `pkg/mocker`, `pkg/testkit`, `pkg/operations/dal`, `cmd/` |
| `domain-is-transport-free` | `services/*/domain` and domain-side `pkg/*` must not import grpc (incl. `codes`/`status`), `pkg/serviceerror`, `pkg/edge`, `pkg/interceptors`, `net/http` |
| `dal-is-transport-free` | DAL code returns `pkg/errs`, never gRPC statuses |
| `hygiene` | stdlib `errors`, `math/rand/v2`, `log/slog`, `google.golang.org/protobuf`, stdlib testing + go-cmp |

depguard matches **file globs against absolute paths**, hence the leading `**/`. Use the form `**/<dir>/**`:
it matches files directly in `<dir>` *and* in its sub-directories. Verified against golangci-lint 2.15: the brace form
`**/<dir>/{**/,}*.go` from the upstream depguard README **never matches** inside golangci-lint, so don't use it. `!`
negates a glob. Package entries are **prefix** matches; a trailing `$` makes them exact (`math/rand$` bans v1 but
not `math/rand/v2`).

`issues.uniq-by-line: false` is deliberate: golangci-lint otherwise reports one finding per line, and a gosec hit
can hide an errorlint hit on the same line.

**After changing `.golangci.yml`**, run `golangci-lint config verify` *and* lint a throwaway file containing the
violation you meant to catch. A schema-valid rule can still match nothing.

**Adding a new storage engine or driver**: put it in `pkg/store/<engine>/`. No config change is needed.
**Adding a new DAL-like package outside `services/*/dal`**: you almost certainly want a `dal` sub-package instead;
if not, add it to the `store-only-in-dal` exceptions with a comment, in the same PR, and say why in the PR description.

## forbidigo bans and their single allowed home

| Banned | Use instead | Allowed in |
|---|---|---|
| `time.Now/Sleep/After/NewTimer/…` | injected `clock.Clock` | `pkg/clock` |
| `os.Getenv/LookupEnv/Environ` | `pkg/config` | `pkg/config`, `cmd/` |
| `context.Background/TODO` | caller's ctx; `t.Context()` in tests | `cmd/`, `pkg/mocker` |
| `fmt.Print*`, `print`, `println` | injected `*slog.Logger` | `cmd/` |
| `errors.As` | `errors.AsType[E]` | — |
| `panic` | return an error | anywhere, with `//nolint:forbidigo // <invariant>` |

## The architecture test (`test/arch`)

Implemented with `golang.org/x/tools/go/packages` loading `./...` and checking each package's **direct** imports.
It exists because depguard cannot say "only *your own* area's domain". Rules:

1. A package under `services/<A>/` must not import `services/<B>/...` for `B ≠ A`.
2. `services/<A>/api` may import `services/<A>/domain`, never `services/<A>/dal`.
3. `services/<A>/domain` must not import `services/<A>/api` or `services/<A>/dal`.
4. `services/<A>/dal` may import `services/<A>/domain` (to implement its ports), never `services/<A>/api`.
5. `pkg/...` must not import `services/...`.
6. Only `cmd/...` and `pkg/mocker` may import more than one `services/<area>` module root.
7. Every `services/<A>/domain` package has a `ports.go` (consumer-defined interfaces live there).

Failures print the offending import edge and the rule number. The test has a table of **explicit, commented
exceptions**; an exception without a linked issue is rejected in review. `go-arch-lint` (`fe3dback/go-arch-lint`)
is a reasonable replacement if the rules grow, but a 100-line Go test costs no extra tool install.

## Suppressing a finding

- `//nolint:<linter> // <why this is correct here>`. Both the specific linter name and the explanation are mandatory
  (`nolintlint` enforces); unused nolint directives fail the build.
- Never disable a linter for a whole file or package to make a PR pass. Change the code, or change the rule in
  `.golangci.yml` in a dedicated commit that explains the reasoning.
- Generated code (`zz_generated_*.go`, `*.pb.go`) is excluded automatically (`generated: lax`).

## Workflow

```bash
make fmt     # gofumpt + gci (golangci-lint fmt)
make lint    # golangci-lint run ./...
make test    # go test -race -shuffle=on -count=1 ./...  (includes test/arch)
make check   # all of the above; what the pre-commit hook runs
make hooks   # install .githooks/pre-commit via git config core.hooksPath .githooks
```

Pin `golangci-lint` as a Go tool dependency (`go get -tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint@<version>`,
run as `go tool golangci-lint`) so every contributor runs the same version with no separate install.

When a lint fails: read the `desc`/`msg` — each rule states the alternative. If the alternative doesn't fit your
case, that is a design conversation (see `go-architecture`), not a `nolint`.
