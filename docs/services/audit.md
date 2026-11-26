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
| `resource.state`: `previous` (before an update or delete) and `current` (after a create or update), each the resource's own `Get` view (metadata, spec, status), redacted | ✅ contract + e2e: `test/terraform/audit` (IAM, KMS, VPC) |
| `DATA_PLANE` in List and exports: selected by `event_type`, never mixed with `CONTROL_PLANE` | ✅ |
| `peer.AuditSink`: data planes record `DATA_PLANE` events from structured fields; the audit module provides it | ✅ |
| Object Storage S3 front: `DATA_PLANE` events for every documented object operation, per the bucket's `object_audit_logging`, including denied, failed and badly signed requests | ✅ e2e: `test/audit` (`make e2e-storage`: AWS SDK v2, aws CLI, `nebius` CLI, export) |
| Failed authentications on the gRPC edge: every request with an unknown, malformed, expired or revoked bearer token (or none, when authentication is required), any method, as an `ERROR` event with `authenticated=false`, the masked token, `client_ip`, `user_agent` and the reason (see [Failed authentications](#failed-authentications)) | ✅ contract: `test/contract/audit_authn_test.go` |
| Failed token exchanges (gRPC `TokenExchangeService/Exchange` and HTTP `POST /oauth2/token/exchange`): the presented key (`kid` → `resource.metadata.id`), the claimed service account, the masked JWT (never its signature), `client_ip`, `user_agent`, the reason | ✅ contract + e2e: canary key with the `nebius` CLI and the gosdk credentials file (`test/audit`) |
| Failed authentications on the registry's Docker front and the AI endpoints data plane | Not yet |
| `authentication.federation` | Not yet |
| Persistence of events across restarts | Not yet (the log sits behind a port; a persistent implementation can replace it) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100.

Covered by unit tests (`pkg/audit`: filter parser with fuzzing, log eviction, recorder, redaction, failed-authentication
events; `pkg/interceptors`: event mapping, failed authentications; `services/audit/domain`, `services/audit/dal`, with a 50k-event List benchmark), gosdk contract tests
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
| `resource.state` | see [Resource state](#resource-state) |
| `resource.metadata` | id from `Operation.resource_id`, else the request's `id`/`metadata.id`; `type` = NID prefix; `name` from the request, or remembered from an earlier event about the same id (so Delete events carry the name) |
| `resource.hierarchy` | ancestors from the authz engine (`deps.Authz.Parent`), tenant first, resolved **before** the handler runs; the seeded tenant and project are named `default` |
| `authentication` | principal → `subject.tenant_user_id` or `subject.service_account_id` (and `subject.name` = the principal ID); `token_credential.masked_token` = JWT without signature, or `prefix***hash` for opaque dev tokens |
| `request` | `request_id` (also `trace_id`), `user_agent`, `client_ip`, `idempotency_id` (`X-Idempotency-Key`), `parameters` = redacted request |
| `response` | gRPC `status_code`, `error_message`, `payload` = redacted response |
| `status` | `STARTED` for a running operation, then `DONE`/`ERROR` when it finishes; `DONE`/`ERROR` for synchronous calls and denials |

### Resource state

`resource.state` follows the Nebius event reference: `previous` is the resource before an UPDATE or DELETE, `current`
the resource after a CREATE or UPDATE. Both are the JSON (proto field names) of what the changed service's own `Get`
returns for `resource.metadata.id`, so they hold `metadata`, `spec` and `status` exactly as a client sees them, after
the same `Sanitize()` redaction as request and response payloads.

- **How:** `interceptors.ResourceStates` wraps the registrar modules register on and remembers each gRPC service's
  `Get`. The interceptor calls that `Get` in process, without the interceptor chain (so it is neither authorized nor
  audited), before the handler for `previous` and after it for `current`. A service without a `Get` by `id`, or a
  resource that cannot be read (created by a failed call, or deleted), simply has no such side.
- **Asynchronous operations:** the `STARTED` event carries `previous` only. The `DONE` event repeats it and adds
  `current`, read when the operation finishes (the operation engine's `OnFinish` hook).
- **No state** for failed or denied calls, and for reads recorded with `NEBIUS_MOCKER_AUDIT_READS`.
- A soft delete that leaves the resource readable (e.g. a KMS key scheduled for deletion) has a `current` too,
  showing the resource as it now is.

**Which calls:** methods whose authz verb is neither `get` nor `list` (by name when no rule exists), plus every call
that `enforce` mode denied. Real Nebius control-plane logs show management operations; reads are included with
`NEBIUS_MOCKER_AUDIT_READS=true`. Calls whose credentials are rejected are recorded by a second interceptor, for
every method: see [Failed authentications](#failed-authentications).

**Redaction:** gosdk generates `Sanitize()` for every message with `(nebius.sensitive)` fields (secret payloads,
access-key secrets, filters). `audit.Redact` runs it on a `proto.Clone`, never on the live request, walking into
`google.protobuf.Any` values; an `Any` of an unknown type is dropped. Tests assert that secret payloads and dev tokens
never appear in stored or exported events.

**Cost:** recording is synchronous and append-only: one clone, two JSON conversions and one append under a short mutex.
It never changes the call's result.

### Failed authentications

> **Fidelity:** the mocker records failed authentications; Nebius does not document whether its audit log does. The
> event shape below uses only fields of `audit.v2.AuditEvent` and the naming every other event follows.

**Which:** every failed authentication, whatever the method, reads included, since the point is to see bad
credentials:

| Where | Refused | `source` / `action` | `resource.metadata` |
|---|---|---|---|
| gRPC edge (`interceptors.AuditAuthnFailures`, outside authentication) | an unknown or malformed bearer token, an issued token that is expired or revoked (its key was deactivated or deleted, its service account deleted), no token when authentication is required | the method, e.g. `nebius.iam.v1.ProfileService/Get` / `GET` | the request's resource, as for any event |
| Token exchange, gRPC | every exchange that issues no token: unknown, deactivated, expired or deleted auth public key, bad signature, wrong `iss`/`sub`, expired or malformed JWT, `alg` other than RS256, federated credentials that do not trust the issuer or subject or whose signature fails, an invalid impersonation actor token, a malformed request | `nebius.iam.v1.TokenExchangeService/Exchange` / `EXCHANGE` | the presented key (`kid`, `publickey-…`), else the claimed service account, else none |
| Token exchange, HTTP | the same, plus a malformed form | `oauth2/token/exchange` / `EXCHANGE` | as above |

`type` follows the usual rule: `ai.nebius.iam.publickey.exchange`, `ai.nebius.iam.serviceaccount.exchange`,
`ai.nebius.iam.tokenexchange.exchange` (nothing identified), `ai.nebius.iam.profile.get`. Successful exchanges are not
recorded.

| Event field | Value |
|---|---|
| `authentication` | `authenticated` false. `token_credential.masked_token`: the presented JWT without its signature, a mocker token without its MAC, or `prefix***hash` for an opaque token. `subject.service_account_id`: the account the credential **claims** (JWT `sub`/`iss`, the exchange's `subject_token`, an issued token's `sub`), unverified |
| `resource.hierarchy` | the resource's ancestors; for a key or account that does not exist (a canary ID that was never real), the claimed service account's, else the default tenant |
| `request` | `client_ip`, `user_agent`, `request_id`. `parameters`: for an exchange the non-secret form fields (`grant_type`, `subject_token_type`, `actor_token_type`, `requested_token_type`, `service_account_id`) and the JWT's `kid`, `alg`, `iss`, `sub`; for a rejected issued token its `token_id` and `token_key_id`. Never the JWT, its signature, the actor token or a private key. The request body of a rejected gRPC call is not recorded |
| `response` | `status_code` (`UNAUTHENTICATED`, `INVALID_ARGUMENT` for a malformed request, `PERMISSION_DENIED` for a denied impersonation), `error_message` the reason (e.g. `auth public key publickey-… is INACTIVE`); HTTP exchanges add `payload.http_status` and `payload.error` (the OAuth code, `invalid_grant`) |
| `status` | `ERROR` |

**Finding them:** the filter language has no `authenticated` field, so select by what was presented:

```
resource.metadata.id = 'publickey-e00…' AND action = 'EXCHANGE'          # a key, e.g. a canary
authentication.subject.service_account_id = 'serviceaccount-e00…' AND status = 'ERROR'
authentication.token_credential.masked_token = '<masked token>'
```

**Canary keys:** create a key for a service account (`nebius iam auth-public-key generate`) and deactivate it right
away; it can never yield a token. Any use of it, through a profile made with `--service-account-file` or with
`--service-account-id --public-key-id --private-key-file`, or a gosdk credentials file, is refused and recorded, and
`nebius audit v2 audit-event list --filter "resource.metadata.id = '<key>' AND action = 'EXCHANGE'"` returns it; the
first such event is the alert. A key ID that never existed works too (the event then belongs to the claimed service
account's tenant, else the default tenant).

**Bounded:** failed-authentication events go into the same ring as all others (`NEBIUS_MOCKER_AUDIT_MAX_EVENTS`, oldest
evicted), so an unauthenticated flood cannot grow memory beyond it, though it evicts older events as any burst would.
What an unauthenticated caller controls is clipped: IDs, subjects, parameter values and the user agent to 256 bytes,
the masked token to 2 KB, the message to 1 KB, at most 16 parameters.

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
`end` exclusive. `event_type` selects `CONTROL_PLANE` (also when unspecified) or `DATA_PLANE` events; the two are
stored apart and never mixed. `region` must be one of the emulated regions (`NEBIUS_MOCKER_REGIONS`), else `INVALID_ARGUMENT`
(`unknown region <r>; known: …`). The mocker serves one control plane, so every recorded event belongs to the
**default region** (the first one); List in another known region returns no events. `page_size` is 0–500 (default 100). Results are oldest
first, and `page_token` is an opaque cursor that survives eviction.

### Exports

- **Start** validates the region (as for List), the params (`from < to`, filter, and `spec.params.event_type`, which is required:
  `CONTROL_PLANE` or `DATA_PLANE`, and the export holds only events of that type) and the destination bucket. It creates the export
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

### Data-plane events

Object Storage records object access as `DATA_PLANE` events when the bucket's `spec.object_audit_logging` asks for it
([Nebius docs](https://docs.nebius.com/object-storage/logs)). Data planes are not gRPC, so they report through the
transport-free capability `peer.AuditSink` (`pkg/peer/audit.go`), which the audit module provides. A caller looks it up
per request: with the audit module disabled there is no sink and nothing is recorded. The sink applies the Nebius
policy itself, so a caller reports every request it serves:

| Mode (`object_audit_logging`) | Recorded operations |
|---|---|
| `NONE` (default) | none |
| `MUTATE_ONLY` | `PutObject`, `CopyObject`, `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `AbortMultipartUpload`, `CompleteMultipartUpload`, `DeleteObject`, `DeleteObjects`, `DeleteObjectVersion` |
| `ALL` | the above plus `GetObject`, `HeadObject`, `ListParts`, `ListObjectVersions`, `ListObjects` (and `ListObjectsV2`), `ListMultipartUploads` |

Other S3 calls (`GetObjectAcl`, tagging, bucket calls) and responses with HTTP 400 or 429 are never recorded, nor
are requests for a bucket that does not exist. `DeleteObject` with a `versionId` is recorded as `DeleteObjectVersion`.

The S3 front (`services/storage/s3/audit.go`) reports each request after its response is complete. It wraps the
response writer to see the final status and the S3 error code. It looks the sink up per request: when the audit
module is disabled there is nothing to do. It passes only structured fields: headers, the `Authorization` value and
presigned-URL credentials never reach the sink. Recording is an in-memory append. It cannot fail the request.

| Event field | Value |
|---|---|
| `source` / `action` / `type` | `s3/PutObject` / `PUT_OBJECT` / `ai.nebius.storage.storagebucket.put_object` |
| `service.name` | `STORAGE` |
| `resource` | the bucket (`id`, `name`, `type` `storagebucket`) and its hierarchy, tenant first |
| `authentication` | `authentication_type` `STATIC_KEY`; `subject.service_account_id` the key's service account; `static_key_credential.id` the Nebius access key ID (`accesskey-…`); `authenticated` false for an unknown key or a bad signature |
| `request` | `client_ip`, `user_agent`, `request_id` (the object store's `x-amz-request-id`); `parameters` `bucket_name`, `object_key` (or `object_keys` for `DeleteObjects`), `version`, `prefix`, `upload_id`, `part_number`, `copy_source`. Never credentials or signatures |
| `response` | `status_code` from the HTTP status (403 → `PERMISSION_DENIED`, a credential error → `UNAUTHENTICATED`, 404 → `NOT_FOUND`, 5xx → `INTERNAL`); `error_message` the S3 error code and message; `payload` `http_status`, `error_code`, `etag`, `version_id`; for `DeleteObjects` `deleted` and `errors` (key → S3 code) |
| `status` | `DONE`, or `ERROR` for HTTP ≥ 400 |

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

- Federation is not modeled.
- A `DeleteObjects` event lists the requested keys in `request.parameters.object_keys` (deleted ones first, then the
  failed ones sorted, not the request's order), and the outcome in `response.payload.deleted` and
  `response.payload.errors` (key → S3 code, e.g. `AccessDenied` from the bucket policy).
- `source`, `action` and `type` of data-plane events are not documented by Nebius; the mocker derives them like
  control-plane ones (`s3/<Operation>`, UPPER_SNAKE action).
- `authentication.subject.name` is the principal ID; the mocker has no user names.
- Every event belongs to the default region; there is no per-region event history.
- Events live in memory only.
- Failed authentications on the registry's Docker front (`docker login` with a bad token) and on the AI endpoints data
  plane are not recorded yet; successful token exchanges are not recorded either.
- A failed federated exchange names the service account as its resource, not the federated credentials it tried.

## Open questions

- Does Nebius audit failed validation and authentication errors? Not documented. The mocker records validation
  failures of mutating calls and, at a user's request, every failed authentication (see
  [Failed authentications](#failed-authentications)); the event shape for those is the mocker's own choice.
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
