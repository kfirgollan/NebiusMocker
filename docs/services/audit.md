# Audit

> Module: `services/audit` · Event capture: `pkg/interceptors/audit.go` + `pkg/audit` · `NEBIUS_MOCKER_SERVICES=audit`
> Tier: **T2** · Endpoint: `audit.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| Automatic events for **every** module: one per mutating RPC and per enforced authz denial, from a central interceptor (no per-service code) | ✅ |
| `STARTED` → `DONE`/`ERROR` for asynchronous operations (via the operation engine's `OnFinish` hook); a single `DONE`/`ERROR` when the call finishes inline | ✅ |
| Redaction: request/response payloads are gosdk `Sanitize()`d copies, including messages packed in `Any`; bearer tokens are masked | ✅ |
| `nebius.audit.v2.AuditEventService.List`: parent scoping (tenant or any ancestor), `start`/`end`, `event_type`, filter language, paging | ✅ T2 |
| `nebius.audit.v2.AuditEventExportService`: Start, Get, List (paged), Cancel; JSON Lines objects in the Object Storage backend | ✅ T2 |
| Authorization: `admin` and the new `auditor` role; `viewer`/`editor` are excluded from `audit.*` | ✅ |
| Bounded in-memory log (`NEBIUS_MOCKER_AUDIT_MAX_EVENTS`, default 50 000, oldest evicted) | ✅ |
| `nebius audit v2 audit-event list` / `audit-event-export start\|get\|list\|cancel` with the official CLI; the destination bucket is created with `nebius storage bucket create` | ✅ e2e: `test/audit` |
| Terraform-driven changes (IAM groups, KMS keys, VPC networks and subnets) produce the matching events, checked with filters via the SDK | ✅ e2e: `test/terraform/audit` |
| Export into a real bucket, downloaded with the AWS SDK through the Object Storage S3 front | ✅ e2e: `test/audit` (`make e2e-storage`, RustFS) |
| Data-plane events (`DATA_PLANE`, S3 object reads/writes) | Not yet: `DATA_PLANE` is rejected with `INVALID_ARGUMENT` |
| `resource.state` (previous/current), `authentication.federation`, static-key credentials | Not yet |
| Persistence of events across restarts | Not yet (the log sits behind a port; a persistent implementation can replace it) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100.

Covered by unit tests (`pkg/audit`: filter parser with fuzzing, log eviction, recorder, redaction; `pkg/interceptors`:
event mapping; `services/audit/domain`, `services/audit/dal`, with a 50k-event List benchmark), gosdk contract tests
(`test/contract/audit_*`), the Terraform e2e (`test/terraform/audit`) and the CLI e2e (`test/audit`), all in `make e2e`,
plus the S3-front export download in `make e2e-storage`.

## Overview

Nebius [Audit Logs](https://docs.nebius.com/audit-logs) record "who did what and when" as CloudEvents-like
`AuditEvent`s (`source`, `type`, `service`, `action`, `time`, `authentication`, `resource`, `request`, `response`,
`status`). `AuditEventService.List` searches them; `AuditEventExportService` copies a time range of them to an
Object Storage bucket.

| Resource kind | Message | NID prefix | Parent | Lifecycle states |
|---|---|---|---|---|
| Audit event | `audit.v2.AuditEvent` | (UUID) | — (scoped by `resource.hierarchy`) | `STARTED`, `DONE`, `ERROR` |
| Export | `audit.v2.AuditEventExport` | `auditeventexport` | tenant (any container) | `RUNNING` → `DONE` \| `FAILED` \| `CANCELED` |

## Emulation design

### Event capture: centrally, not per service

`interceptors.Audit` sits in the edge chain after authentication and outside authorization (see
[architecture/04](../architecture/04-request-pipeline.md#audit-events)), so every service, current and future, is
audited with no change to its code:

| Event field | Source |
|---|---|
| `source` | gRPC method, e.g. `nebius.iam.v1.GroupService/Create` |
| `service.name` | API area upper-cased; documented exceptions `vpc` → `VIRTUAL_NETWORKS`, `mysterybox` → `MBOX`, `msp.postgresql` → `MSP_POSTGRESQL`, `msp.mlflow` → `MSP_MLFLOW`, `msp.serverless` → `MSP_SERVERLESS` |
| `action` | method name in UPPER_SNAKE: `CREATE`, `UPDATE`, `DELETE`, `ROTATE`, `START`, … |
| `type` | `ai.nebius.<area>.<NID prefix>.<action lowercase>`, e.g. `ai.nebius.iam.group.delete` |
| `resource.metadata` | id from `Operation.resource_id`, else the request's `id`/`metadata.id`; `type` = NID prefix; `name` from the request, or remembered from an earlier event about the same id (so Delete events carry the name) |
| `resource.hierarchy` | ancestors from the authz engine (`deps.Authz.Parent`), tenant first, resolved **before** the handler runs; the seeded tenant and project are named `default` |
| `authentication` | principal → `subject.tenant_user_id` or `subject.service_account_id` (and `subject.name` = the principal ID); `token_credential.masked_token` = JWT without signature, or `prefix***hash` for opaque dev tokens |
| `request` | `request_id` (also `trace_id`), `user_agent`, `client_ip`, `idempotency_id` (`X-Idempotency-Key`), `parameters` = redacted request |
| `response` | gRPC `status_code`, `error_message`, `payload` = redacted response |
| `status` | `STARTED` for a running operation, then `DONE`/`ERROR` when it finishes; `DONE`/`ERROR` for synchronous calls and denials |

**Which calls:** methods whose authz verb is neither `get` nor `list` (by name when no rule exists), plus every call
that `enforce` mode denied. Real Nebius control-plane logs show management operations; reads are included with
`NEBIUS_MOCKER_AUDIT_READS=true`. Calls rejected before the interceptor (invalid or missing token) are not recorded.

**Redaction:** gosdk generates `Sanitize()` for every message with `(nebius.sensitive)` fields (secret payloads,
access-key secrets, filters). `audit.Redact` runs it on a `proto.Clone`, never on the live request, walking into
`google.protobuf.Any` values; an `Any` of an unknown type is dropped. Tests assert that secret payloads and dev tokens
never appear in stored or exported events.

**Cost:** recording is synchronous and append-only: one clone, two JSON conversions and one append under a short mutex.
It never changes the call's result.

### Storage and search: vanilla, no index library

The expected scale is a few MB of events, tens of thousands. `pkg/audit.Log` is a ring of encoded events with their
filterable fields pre-extracted, so a query scans small structs and decodes only matches. A List over 50 000 events with
a three-clause filter including a regex takes about 0.5 ms (`BenchmarkList50k`). A search library (Lucene, Bleve)
would add weight with no benefit at this size. The log sits behind ports (`Sink` for the recorder, `EventSource` for
the service), so persistence can be added later.

### Filter language

From `audit_event_service.proto`, parsed by a hand-written tokenizer and parser (`pkg/audit/filter.go`, fuzzed):

```
service.name = 'IAM' AND resource.hierarchy.id:'project-e00' AND regex(resource.metadata.name, '^.*test.*$')
```

- Operators: `=`, `!=`, `:` (substring), `regex(field, 'RE2')`, joined with `AND` (case-insensitive). `OR`, `NOT`
  and parentheses are rejected.
- Values: single- or double-quoted strings (backslash escapes) or bare words (`action=DELETE`). Matching is
  case-sensitive.
- Fields: `action`, `type`, `status`, `service.name`, `authentication.subject.{name,service_account_id,tenant_user_id}`,
  `authentication.token_credential.masked_token`, `authentication.static_key_credential.id`,
  `resource.metadata.{id,name,type}` (`resource.name` is an alias), and `resource.hierarchy.{id,name}`. The hierarchy
  fields match if any ancestor matches (`!=`: if none does).
- Errors are `INVALID_ARGUMENT` with a `BadRequest` violation on `filter` that names the 1-based position, e.g.
  `position 8: expected operator (=, !=, :) after "action"`.

### List

`parent_id` must exist; events match when `parent_id` is the resource or one of its ancestors. `start` is inclusive and
`end` exclusive. `region` must be one of the emulated regions (`NEBIUS_MOCKER_REGIONS`), else `INVALID_ARGUMENT`
(`unknown region <r>; known: …`). The mocker serves one control plane, so every recorded event belongs to the
**default region** (the first one); List in another known region returns no events. `event_type` `DATA_PLANE` is
rejected with `INVALID_ARGUMENT` (`DATA_PLANE (Object Storage data-plane) events are not recorded by NebiusMocker yet;
use CONTROL_PLANE`); `EVENT_TYPE_UNSPECIFIED` is `CONTROL_PLANE`. `page_size` is 0–500 (default 100). Results are oldest
first, and `page_token` is an opaque cursor that survives eviction.

### Exports

- **Start** validates the region (as for List), the params (`from < to`, filter, event type; `DATA_PLANE` is rejected
  on `spec.params.event_type`) and the destination bucket. It creates the export
  as `RUNNING` and returns an operation whose `resource_id` is the export; `status.export_operation_id` is that
  operation's id.
- **Objects:** the matching events are written as JSON Lines (proto field names), at most 10 000 per object, to
  `<object_prefix or "auditlogs">/<export_operation_id>/audit-events-000001.jsonl` in the Object Storage backend
  (`pkg/objstore`; memory by default). The state then becomes `DONE`, or `FAILED` with `status.message`.
- **Timing:** with `NEBIUS_MOCKER_AUDIT_EXPORT_DURATION=0` (the default) this happens inside Start. Otherwise the
  export stays `RUNNING` for that long, so Cancel can be exercised.
- **Cancel** takes effect immediately (`CANCELED`); a pending run then writes nothing. Cancelling a finished export is
  `FAILED_PRECONDITION`. While the Start operation itself is still running (`NEBIUS_MOCKER_OP_DELAY` > 0), Cancel gets
  the usual operation conflict.
- **Region:** an export remembers the region it was started in, and export List returns only the exports of the
  requested region. An export started in a non-default region is created and finishes `DONE`, but exports the events
  of its region, which are none, so it writes no objects.
- **Bucket:** exports write to Object Storage, so they need the **storage module**. Without it, Start fails with
  `FAILED_PRECONDITION` ("enable the storage module (NEBIUS_MOCKER_SERVICES)"). The bucket (`bucket_by_id.id`) must
  exist in the export's tenant (else `INVALID_ARGUMENT` on `spec.nebius_object_storage.bucket_by_id.id`), and its
  backend name comes from storage's `peer.BucketResolver`. The storage module creates the backend bucket with the
  bucket; the export never creates it, and an export whose backend bucket is gone ends `FAILED` with the reason in
  `status.message`.
- **Bucket permission:** the Nebius docs require the `storage.uploader` role on the destination bucket. Under
  `NEBIUS_MOCKER_AUTHZ=enforce` the caller must hold `storage.objects.create` on the bucket (the permission storage
  checks for S3 writes), else `PERMISSION_DENIED`. The mocker has no `storage.uploader` role: `admin` and `editor`
  (including a binding on the bucket itself) hold it; `auditor` does not, so an auditor needs an additional editor
  binding on the bucket to export.

### Permissions

| RPC | Permission | Target |
|---|---|---|
| `AuditEventService.List` | `audit.auditEvents.list` | `parent_id` |
| `AuditEventExportService.Start` | `audit.auditEventExports.create` | `metadata.parent_id` |
| `.Get` / `.List` | `audit.auditEventExports.get` / `.list` | id / `parent_id` |
| `.Cancel` | `audit.auditEventExports.cancel` | id |

Audit events expose every service's request payloads and principals. So `viewer` and `editor` exclude `audit.*.*`;
`admin` holds it, and the new built-in role **`auditor`** (viewer reads plus `audit.*.*`) can be granted with an
access permit. The Nebius docs say a dedicated tenant role is needed but do not name it on the page.

## Terraform

_No Terraform provider objects use this area._ The e2e instead checks that Terraform-driven changes in other areas
(IAM groups, KMS keys, VPC networks and subnets) produce the matching audit events.

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_AUDIT_MAX_EVENTS` | `50000` | Log capacity; the oldest events are evicted |
| `NEBIUS_MOCKER_AUDIT_READS` | `false` | Also record Get/List calls |
| `NEBIUS_MOCKER_AUDIT_EXPORT_DURATION` | `0` | How long exports stay `RUNNING` before writing |

## Known gaps

- No data-plane (S3) events (`DATA_PLANE` is rejected), `resource.state`, federation or static-key credentials.
- `authentication.subject.name` is the principal ID; the mocker has no user names.
- Every event belongs to the default region; there is no per-region event history.
- Events live in memory only.

## Open questions

- Does Nebius audit failed validation and authentication errors? The mocker records validation failures of mutating
  calls but not authentication failures.
- The exact export object naming and compression are not documented. The mocker writes uncompressed `.jsonl`.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.audit.v2.AuditEventExportService`

Endpoint: `audit.api.nebius.cloud:443` · Proto: [`nebius/audit/v2/audit_event_export_service.proto`](https://github.com/nebius/api/blob/main/nebius/audit/v2/audit_event_export_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Start` | `StartRequest` | `common.v1.Operation` |  |
| `Cancel` | `CancelRequest` | `common.v1.Operation` |  |
| `Get` | `GetRequest` | `AuditEventExport` |  |
| `List` | `ListRequest` | `ListAuditEventExportResponse` |  |

#### `nebius.audit.v2.AuditEventService`

Endpoint: `audit.api.nebius.cloud:443` · Proto: [`nebius/audit/v2/audit_event_service.proto`](https://github.com/nebius/api/blob/main/nebius/audit/v2/audit_event_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `List` | `ListAuditEventRequest` | `ListAuditEventResponse` |  |
