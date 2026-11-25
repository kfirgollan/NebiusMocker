# NebiusMocker

A local emulator of the [Nebius AI Cloud](https://nebius.com) API, in the spirit of LocalStack. It serves the same
gRPC services as the real cloud, so the **official** Nebius Terraform provider, CLI and SDKs run against your laptop or
CI with no Nebius account.

> Status: early. **IAM groups** work end to end with the official Terraform provider. See
> [docs/services/iam.md](docs/services/iam.md#implementation-status) and the [roadmap](docs/architecture/13-milestones.md).

## Quick start

```bash
make run    # all-in-one server on :4566 (or: go run ./cmd/nebius-mocker)
```

Point the official provider at it. Nothing else changes in your Terraform:

```hcl
terraform {
  required_providers {
    nebius = { source = "nebius/nebius", version = "= 0.6.91" }
  }
}

provider "nebius" {
  resolvers       = { "*" = "127.0.0.1:4566" }      # every Nebius service → the mocker
  address_options = { "*" = { insecure = true } }   # plaintext gRPC
  token           = "nmock-dev-admin"               # built-in dev token
}

resource "nebius_iam_v1_group" "engineers" {
  parent_id = "tenant-e00mock0000000000default"     # seeded default tenant
  name      = "engineers"
}
```

Common settings (all `NEBIUS_MOCKER_*` env vars, see [configuration](docs/architecture/11-configuration.md)):

| Env | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_LISTEN` | `:4566` | gRPC edge address |
| `NEBIUS_MOCKER_AUTHN` | `optional` | `required` rejects requests without a valid token |
| `NEBIUS_MOCKER_DEV_TOKENS` | `nmock-dev-admin=<dev user>` | `token=principal,…` static tokens |
| `NEBIUS_MOCKER_OP_DELAY` | `0` | Keep operations running this long (exercise client polling) |
| `NEBIUS_MOCKER_IDS` | `random` | `sequential` for predictable IDs |

## Development

```bash
make hooks   # once: install the pre-commit gate
make check   # lint (layering rules included) + unit/contract/architecture tests with -race
make e2e     # real terraform + official nebius provider against the mock
```

Read [docs/architecture](docs/architecture/README.md) first. The Go rules for this repo live in `.claude/skills/`
(`go-architecture`, `go-style`, `go-errors`, `go-concurrency`, `go-patterns`, `go-testing`, `go-lint`) and are enforced
by `.golangci.yml` and `test/arch`.

## License

Apache-2.0. See [LICENSE](LICENSE).
