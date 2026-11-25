# 12 — Test harness

The harness has to answer two questions continuously:

1. **Does the mocker behave like Nebius?** (API fidelity)
2. **Do real tools work against it?** The first target is the Terraform provider.

It is a layered pyramid. Layers 1–3 and 5 need no Nebius credentials.

> **No CI budget (for now).** Every layer runs locally: `make check` (format, lint, unit, contract and arch tests)
> runs from a git pre-commit hook. `make tf-test`, `make e2e` and `make drift` are run by hand before merging larger
> changes. The workflow descriptions below still apply if CI becomes available later.

```text
            ┌──────────────────────────────┐
  on demand │ 6. API drift detection        │  upstream nebius/api + provider changed?
            ├──────────────────────────────┤
  opt-in    │ 4. Parity (record ↔ replay)   │  real Nebius tenant ↔ mocker
            ├──────────────────────────────┤
  pre-merge │ 5. Topology e2e (compose)     │  all-in-one, single, split
            │ 3. Terraform scenarios        │  real terraform + real provider binary
            │ 2. Resource conformance kit   │  gosdk client, every kind, generic invariants
            │ 1. Unit tests                 │  framework + module domain rules
            └──────────────────────────────┘
```

## 1. Unit tests

Standard `go test` next to the code, covering the framework (reset-mask patching, NID generation, paging tokens, quota
arithmetic, authz evaluation, the lifecycle scheduler with a manual clock) and module domain rules (CIDR overlap,
platform/preset validation, and so on). Table-driven, with no network.

## 2. Resource conformance kit (`test/contract`)

This is a **generic suite parameterized by `resource.Kind`**. Each module provides one fixture per kind: a minimal valid
`metadata+spec`, an "update" mutation, one immutable-field mutation, and the dependencies to create first. The kit then
asserts the shared Nebius invariants through the **official gosdk client**, over a real gRPC connection to an in-process
server (`pkg/testkit.StartT`):

| # | Invariant |
|---|---|
| C1 | `Create` returns an `Operation` with `resource_id` set. Waiting on it yields `status.code=OK` and `finished_at` |
| C2 | `Get(id)` returns the spec as sent, plus server defaults. `metadata.created_at`, `resource_version ≥ 1` |
| C3 | `GetByName(parent, name)` returns the same resource. Duplicate name → `ALREADY_EXISTS` + `ResourceAlreadyExists` (unless `UNNAMED`) |
| C4 | `List(parent)` contains it. Paging with `page_size=1` visits every item exactly once |
| C5 | `Update` with a changed field is applied and bumps `resource_version`. Fields omitted and not in `X-ResetMask` are **unchanged**. Fields in `X-ResetMask` are **cleared** |
| C6 | Changing an `IMMUTABLE` field → `INVALID_ARGUMENT` + `BadRequest` naming the field |
| C7 | Requests that violate `buf.validate` → `INVALID_ARGUMENT` + `BadRequest` |
| C8 | Same `X-Idempotency-Key` + same payload → same operation ID. Different payload → error |
| C9 | A second mutation while an operation is running → `OperationConflict` (with `timing=fast` + manual clock) |
| C10 | `Delete` → operation OK → `Get` returns `NOT_FOUND` + `ResourceNotFound` |
| C11 | Missing parent → `NOT_FOUND`. Deleting a resource with dependents → `FAILED_PRECONDITION` |
| C12 | In `authz=enforce` with a viewer token: reads OK, writes `PERMISSION_DENIED`. With no permit: `Get` → `NOT_FOUND` |
| C13 | Quota set to the current usage → `Create` fails with `QuotaFailure` and the right quota name |
| C14 | `OperationService.Get/List` work for the operation |
| C15 | `status` fields sent by the client are ignored |

A coverage check (`go test ./test/contract -run TestCoverage`) **fails if any T1+ kind has no fixture**. New services
therefore cannot skip the kit.

## 3. Terraform scenarios (`test/terraform`)

This is the core of milestone 1. It uses the **real** `terraform` binary and the **real** `nebius/nebius` provider from
the Terraform Registry, at a pinned version and a matrix of the last N versions. There are no forks or patches.

### Runner

A Go test runner built on `hashicorp/terraform-exec` and `hc-install`:

1. Start the mocker in-process (`testkit.StartT`, `profile=ci`, `timing=instant`, `ids=sequential`), or attach to
   `NEBIUS_MOCKER_ADDR` to test a container image.
2. Write a provider override file (what `tfnebiuslocal` generates):

   ```hcl
   # nebius_mocker_override.tf
   provider "nebius" {
     resolvers       = { "*" = "127.0.0.1:<port>" }
     address_options = { "*" = { insecure = true } }
     token           = "<token minted for the scenario's principal>"
     parent_id       = "<scenario project>"
   }
   ```

3. Run each scenario through these steps, asserting after each one:

| Step | Assertion |
|---|---|
| `init` | Provider downloads and configures |
| `plan` | No errors. Preflight checks pass or are skipped |
| `apply` | Succeeds. Mocker state contains exactly the expected kinds and counts (via admin API) |
| **`plan` again** | **Empty plan, "No changes".** This is the most important fidelity signal: what `Get` returns must round-trip through the provider schema with no diff |
| `apply` of `step2.tf` (if present) | In-place updates happen in place; changes to `IMMUTABLE` fields plan a replacement |
| `plan` again | Empty |
| `import` (if `import.tf` present) | Imported state matches applied state |
| drift (optional) | Mutate via admin API → `plan -refresh-only` shows the drift |
| `destroy` | Succeeds. Mocker state for the scenario project is empty, and quota usage is back to 0 |

### Scenario catalog

```text
test/terraform/scenarios/
├── 00-provider-config/        # token, env token, SA credentials file (JWT exchange), impersonation, no_credentials
├── 10-iam-basics/             # project v2, service account, group, membership, access permit, auth public key, access key
├── 20-vpc-network/            # pool, network, subnet, security group + rules, route table + route, allocation
├── 30-compute-vm/             # disk, instance with managed boot disk from image family, filesystem, secondary disk
├── 31-compute-gpu-cluster/    # gpu cluster, instances in it, nvl instance group
├── 40-mk8s-cluster/           # cluster + node group (preflight check path), version upgrade in step2
├── 50-storage-bucket/         # bucket, inventory, transfer
├── 60-registry/               # container registry
├── 70-mysterybox-kms/         # secret, secret version, payload ephemeral, symmetric/asymmetric keys
├── 80-dns/                    # zone, records
├── 90-msp/                    # postgresql, mlflow
├── 95-misc/                   # quota allowance, pricing policy, capacity allowance, tunnel, k8s release, ai endpoint
├── data-sources/              # one config per data source (62), reading resources created by a seed
├── ephemeral/                 # iam_token, iam_v2_access_key_secret, mysterybox payload(s)
├── negative/                  # quota exceeded, permission denied, name collision, invalid spec → expected error text
└── quickstart/                # provider's docs/guides/quickstart.md config, verbatim
```

**Seeding examples from upstream.** `tools/tfcoverage` also imports the provider's own
`examples/resources/*/resource.tf`, replaces placeholder IDs (`"project-id"`, `"subnet-id"`, …) with references to
fixtures, and runs each one as an auto-generated scenario. Every example the provider documents is then tested, and new
upstream examples show up automatically.

### Negative-path expectations

`negative/*/expect.yaml` asserts that Terraform's error output contains the right gRPC code and detail. This proves the
provider renders our `ServiceError`s the way it renders real ones:

```yaml
step: apply
expect_error:
  - "RESOURCE_EXHAUSTED"
  - "quota compute.instance.count"
```

## 4. Parity tests against real Nebius (`test/parity`)

This is our version of LocalStack's snapshot or parity testing.

- **Record mode** (`PARITY_MODE=record`, requires `NEBIUS_IAM_TOKEN` + a sandbox tenant/project): run the contract fixtures
  and selected Terraform scenarios **against real Nebius**, with the gosdk recording interceptor. The resulting
  request/response/error transcripts are stored under `test/parity/golden/`.
- **Sanitization**: IDs, timestamps, IPs, tokens and resource versions are replaced with stable placeholders
  (`<id:vpcnetwork:1>`), so the files can be committed safely. A secret scanner runs on the output.
- **Replay mode** (default, in `make test`): run the same calls against the mocker and compare **structurally**. The comparison
  covers the same fields being present, the same enum values, the same error codes and detail types, and the same
  operation sync/async shape. It ignores placeholder values and fields listed in a per-test `ignore` list.
- Each golden file records the API revision and the date it was recorded. A golden file older than N months gets a
  re-record ticket.

Parity transcripts are also how we pin behavior the docs don't specify: default values, error message wording, which
fields are echoed back.

## 5. Topology end-to-end (`test/e2e`)

`docker compose` runs each file in `deploy/compose/`, then executes a subset of the Terraform scenarios against it:

| Topology | Scenarios |
|---|---|
| all-in-one | full catalog |
| single service (`nebius-mocker-vpc`) | `20-vpc-network`, plus direct gosdk calls |
| split + gateway | `00`, `10`, `20`, `30` through the gateway, and the same through per-service resolvers |
| persistence | apply → restart container → `plan` is empty → destroy |

## 6. API drift detection

Run with `make drift` (by hand for now; a scheduled workflow once CI is available):

1. Fetch `nebius/api@main` and `terraform-provider-nebius@latest`.
2. `tools/apigen diff` compares services, RPCs, messages and fields against the `gosdk` version pinned in `go.mod`.
3. `tools/tfcoverage diff` reports new Terraform resources, data sources or RPCs.
4. On any difference, open or update an issue labeled `api-drift` with the diff, and open a PR that bumps `gosdk` and
   regenerates `docs/services` tables and handler stubs. New RPCs default to **T0 (`UNIMPLEMENTED`)**, so the build
   never breaks.

## Reporting

`make coverage-report` writes `docs/services/STATUS.md` (generated) with:

- RPCs implemented per service, and tier;
- Terraform resources, data sources and ephemeral resources passing / failing / not yet covered, per provider version;
- parity golden age.

The project README badge shows the share of Terraform resources passing against the latest provider.

## Developer ergonomics

- `make test` runs layers 1–2 (seconds).
- `make tf-test SCENARIO=30-compute-vm` runs one Terraform scenario with the mocker in-process and `TF_LOG=DEBUG`
  captured to a file. On failure the runner prints the mocker request journal next to the Terraform log.
- `tfnebiuslocal plan|apply|…` works in any user directory: it writes the override file, runs terraform, and removes the
  file again.
