# ADR-0008: Layered service modules (api / domain / dal), enforced by linters

**Context.** Each of the 21 areas mixes three concerns: gRPC transport (proto requests, status codes, headers),
business rules (validation, lifecycles, quotas, side effects) and storage. Mixed together, rules become untestable
without a gRPC server, storage leaks into handlers, and split-container deployments get harder.

**Decision.** Every area has `api/` (views: conversion and error mapping only), `domain/` (logic, depending only on
ports it declares in `ports.go`) and `dal/` (the only code that touches `pkg/store`). The direction is `api → domain ← dal`.
Domain and DAL are transport-free: they return `pkg/errs` kinds, and `api` maps them through `pkg/serviceerror`. Proto
messages may still be used as data in domain, because the protos *are* the data model we emulate. Real time,
environment access and root contexts are confined to `pkg/clock`, `pkg/config` and the composition roots.

Enforcement:
- `.golangci.yml` depguard: storage engines only in `pkg/store/**`; `pkg/store` only in dal and composition roots;
  no grpc, `serviceerror` or `net/http` in domain or dal.
- forbidigo: `time.*`, `os.Getenv`, `context.Background`, `fmt.Print`, `errors.As`, `panic`.
- `test/arch`: the relative rules globs can't express (no cross-area imports, api ↛ dal, domain ↛ api/dal).
- Run locally through `make check` and a pre-commit hook, since there is no CI budget.

**Consequences.** Domain logic is unit-testable with in-package fakes and a manual clock. Adding a storage backend
or changing transport touches one layer. The cost is extra conversion code in `api`, which is mechanical and largely
generated. The rules are verified against golangci-lint 2.15: every banned import and call in a fixture module was
reported, with no false positives in the allowed packages.
