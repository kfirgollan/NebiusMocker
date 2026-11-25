# IAM — Identity and Access Management

> Module: `services/iam` · Image: `nebius-mocker-iam` · `NEBIUS_MOCKER_SERVICES=iam`
> M1 tier: **T2** · Endpoints: `cpl.iam`, `tokens.iam`, plus HTTP `/oauth2/token/exchange`

## Implementation status

| Piece | Status |
|---|---|
| Seeded default tenant (`tenant-e00mock0000000000default`) and default project (`project-e00mock000000000default`, region `eu-north1`), plus extra projects from `NEBIUS_MOCKER_IAM_PROJECTS` | ✅ |
| `nebius.iam.v2.ProjectService`: Create, Get, GetByName, List (paged, filtered to what the caller may read), Update (name and labels; `spec.region` and the tenant are immutable), Delete (only an empty project; `dry_run`) | ✅ T1 |
| `nebius.iam.v1.ProjectService` *(deprecated)*: Create, Get, GetByName, List, Update (same rules; v1 has no Delete) | ✅ T1 |
| `nebius.iam.v1.TenantService` *(deprecated)*: Get, List; `nebius.iam.v2.TenantService`: Get, GetByName, List. List returns the tenants the caller may read | ✅ T1 (`v2 Update`: Not yet) |
| `nebius_iam_v2_project` (+ `nebius_iam_v1_project`, `nebius_iam_v2_project`, `nebius_iam_v1_tenant`, `nebius_iam_v2_tenant` data sources), and `nebius iam [v2] project …` / `nebius iam [v2] tenant list` | ✅ e2e: `test/terraform/iam/project_test.go`, `tenant_test.go`, `test/cli/iam/project_e2e_test.go` |
| `nebius.iam.v2.TenantService.Update`; a default VPC network in newly created projects; quotas (`iam.project.count`) | Not yet |
| `nebius.iam.v1.GroupService`: Create, Get, GetByName, List (paged), Update (reset mask, `resource_version`), Delete | ✅ T1 |
| `nebius.iam.v1.GroupMembershipService`: Create, Get, Delete, ListMembers, ListMemberOf (`*WithAttributes` not yet) | ✅ T1 |
| `nebius.iam.v1.AccessPermitService`: Create, Get, Delete, List | ✅ T1 |
| Seeded `admins` / `editors` / `viewers` groups with tenant-level `admin` / `editor` / `viewer` permits | ✅ |
| Authorization (`NEBIUS_MOCKER_AUTHZ=off\|audit\|enforce`) from memberships + permits, inherited down the hierarchy | ✅ |
| `nebius_iam_v1_group`, `_group_membership`, `_access_permit` with the official Terraform provider | ✅ e2e: `test/terraform/iam` |
| Static dev tokens: `nmock-dev-admin`, `nmock-dev-editor`, `nmock-dev-viewer` (members of the seeded groups) | ✅ |
| `nebius.iam.v1.ProfileService.Get` (whoami; the `nebius` CLI calls it before `iam get-access-token`), derived from the token's principal | ✅ |
| `nebius.iam.v1.ServiceAccountService`: Create, Get, GetByName, List, Update (reset mask), Delete (cascades to the account's access keys). Parent must be a project | ✅ T1 |
| `nebius.iam.v2.AccessKeyService`: Create, Get, GetByName, GetSecret, List, ListByAccount, GetByAwsId, Update, Delete, Activate, Deactivate (`*ByAwsId` mutations not yet) | ✅ T1 |
| Access keys as S3 credentials: Object Storage's S3 front verifies SigV4 with them (`peer.AccessKeys` capability in `pkg/peer`, provided by IAM) | ✅ |
| `nebius_iam_v1_service_account`, `nebius_iam_v2_access_key` (+ data sources) and the `nebius_iam_v2_access_key_secret` ephemeral resource | ✅ e2e: `test/terraform/storage` |
| `secret_delivery_mode = MYSTERY_BOX`: the secret is delivered to a MysteryBox secret (`status.secret_reference_id`), read with `nebius_mysterybox_v1_secret_payload_entry` / `nebius mysterybox payload get` and used against the S3 front | ✅ e2e: `test/terraform/iam` (lifecycle in `make e2e`; the S3 use with RustFS in `make e2e-storage`), `test/cli/iam` (`make e2e-storage`) |
| Everything else in this document | Planned |

Access keys: the secret is generated in `services/iam/domain` (`GenerateKeyMaterial`: a 20-character `NMK…` AWS key ID and
a 40-character secret from `crypto/rand`) and stored with the key in the IAM store (`services/iam/dal`, plus an
AWS-key-ID → key index). Delivery modes:

| `secret_delivery_mode` | Behavior |
|---|---|
| `UNSPECIFIED`, `INLINE` | `status.secret` is returned by `Get`/`List` (upstream: "returned directly in the API response"), and by `GetSecret` |
| `EXPLICIT` | `status.secret` is empty; only `GetSecret` reveals it |
| `MYSTERY_BOX` | `status.secret` is empty. The secret is stored in a new MysteryBox secret in the key's project, and `status.secret_reference_id` holds its ID (`mbsec-…`). Read it with `PayloadService` (`nebius mysterybox payload get --secret-id …`, Terraform `nebius_mysterybox_v1_secret_payload_entry`). `GetSecret` still works, as for the other modes |

MYSTERY_BOX details:
- The MysteryBox secret is named after the access key ID, and has one primary version with one string entry, key
  `secret`. Upstream documents only that the secret is the first entry (`.data[0].string_value` in the LanceDB
  tutorial), not the entry's key, so the key name `secret` is the mock's choice (after `status.secret`).
- It is created through the `peer.SystemSecrets` capability (`pkg/peer/systemsecrets.go`, provided by MysteryBox)
  without MysteryBox permission checks: the caller was already authorized for `iam.accessKeys.create`. Reading the
  payload back needs the usual `mysterybox.payload.read` (`admin`, `mysterybox.payload-viewer`). Like any secret
  created by a user, the access key's creator does not get payload access automatically.
- Deleting the access key (or its service account) leaves the MysteryBox secret as it is. Nebius documents no effect
  on it, so delete it through MysteryBox when you no longer need it. If storing the key fails after the secret was
  created, the secret is discarded again.
- If the `mysterybox` service is not enabled (`NEBIUS_MOCKER_SERVICES`), MYSTERY_BOX creates fail with
  `FAILED_PRECONDITION` ("MysteryBox … is not enabled"). They never fall back to another mode.

`GetSecret` needs `iam.accessKeys.getSecret`, which only the `admin` role grants (viewer's `get` does not match it).
`status.state` is computed on read: `EXPIRED` once `spec.expires_at` passes; `Deactivate`/`Activate` toggle `INACTIVE`.
Only `ACTIVE` keys of existing, active service accounts authenticate S3 requests. `account`, `expires_at` and
`secret_delivery_mode` are immutable.

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100 (re-verified for
`ProjectService`/`TenantService` v1+v2 on 2026-11-25; `nebius` CLI 0.12.312).

Covered by unit tests (`services/iam/domain`, `services/iam/dal`), gosdk contract tests (`test/contract`) and the
Terraform e2e suite (`make e2e`). The suite runs create → add → remove → destroy, plus in-place label and name updates.

## Overview

IAM owns the resource hierarchy (tenants → projects), the principals (user accounts, service accounts), credentials
(auth public keys, static keys, access keys), federation (SAML/OIDC identity providers, workload-identity federated
credentials), groups and access permits, and token exchange. **Every other module depends on it** for authentication,
authorization and parent validation. The cross-cutting model is in
[architecture/06](../architecture/06-iam-and-authorization.md). This doc covers the API.

| Resource kind | Message | NID prefix | Parent | Notes |
|---|---|---|---|---|
| Tenant | `iam.v1.Container` / `iam.v2.Tenant` | `tenant` | — | Seeded only; no public Create |
| Project | `iam.v1.Container` / `iam.v2.Project` | `project` | tenant | `spec.region` immutable; `status.project_state` |
| Service account | `ServiceAccount` | `serviceaccount` | project | `status.active` |
| Auth public key | `AuthPublicKey` | `publickey` | SA (`spec.account`) | `spec.data` (PEM) immutable; `status.fingerprint/algorithm/key_size` computed |
| Static key | `StaticKey` | `statickey` | SA | `Issue` returns the token once |
| Access key (v2) | `iam.v2.AccessKey` | `accesskey` | SA | S3-style: `status.aws_access_key_id`, secret via `GetSecret` or MysteryBox delivery |
| Group | `Group` | `group` | tenant | |
| Group membership | `GroupMembership` | `groupmembership` | group | `spec.member_id` = user account or SA |
| Access permit | `AccessPermit` | `accesspermit` | group (subject) | `UNNAMED`; `spec.resource_id` + `spec.role`, both immutable |
| Federation | `Federation` | `federation` | tenant | SAML IdP config, `Activate/Deactivate` |
| Federation certificate | `FederationCertificate` | `federationcertificate` | federation | |
| Federated credentials | `FederatedCredentials` | `federatedcredentials` | SA | OIDC issuer + subject for workload identity |
| Invitation | `Invitation` | `invitation` | tenant | `Resend` |
| Tenant user account | `TenantUserAccount` | `tenantuseraccount` | tenant | `Block/Unblock` |

The NID prefixes for keys, federation objects and invitations are not all visible in the protos' `nid` annotations.
Confirm them during parity recording.

## Emulation design

### Hierarchy and bootstrap
- At boot the module seeds the default tenant (`default`) and its projects: `default` (`NEBIUS_MOCKER_PROJECT_ID`, in
  the first region), then each `NEBIUS_MOCKER_IAM_PROJECTS` entry. A seeded project's ID is derived from the tenant and
  its name (`config.Config.SeededProjects`), with the routing code of its region, so it stays the same across restarts.
  It also seeds the `admins/editors/viewers` groups with tenant-level permits and the dev principals
  (architecture/06 § Default bootstrap).
- Both API versions serve the same stored project (`iam.v1.Container`). The v2 view maps `container_state ACTIVE` to
  `status.project_state ACTIVE`; tenants map to `iam.v2.Tenant` with `status.tenant_state ACTIVE`.
- Projects follow docs.nebius.com/iam/manage-projects: the parent must be a tenant, the name is required and unique in
  the tenant, and the region and tenant are fixed at creation ("for an existing project, you can only edit the name";
  labels can change too). `spec.region` must be one of `NEBIUS_MOCKER_REGIONS`. v2 Create requires it, because
  `ProjectSpec.region` has no default and `nebius_iam_v2_project` declares `region` Optional, not Computed. A
  server-chosen region would fail the apply with "inconsistent result", so omitting it gets a clear
  `spec.region is required` instead. v1 Create defaults it to the first region, since v1 `ContainerSpec.region` is
  `NON_EMPTY_DEFAULT`. An Update that changes the region fails with `INVALID_ARGUMENT` ("spec.region is immutable");
  an empty region in an Update keeps the stored one. The provider plans a region change as an in-place update, so in
  Terraform it fails at apply time; use `terraform apply -replace=…` to move a project. New project IDs use
  `NEBIUS_MOCKER_ROUTING_CODE`.
- `TenantService.GetByName` (v2): upstream allows only an empty `parent_id`, but the `nebius_iam_v2_tenant` data source
  will not read by name without one. The mock therefore also accepts the tenant's own ID
  (`parent_id = <tenant>`), and rejects any other value.
- **Listing is filtered per caller.** `ProjectService.List(parent_id=tenant)` and `TenantService.List` are open to
  every authenticated caller, and return only the items the caller holds `iam.projects.get` / `iam.tenants.get` on
  (directly or through an ancestor), via `authz.Visibility`. So a tenant viewer sees every project, an outsider sees
  an empty list, and a principal granted on one project sees only that one. A page is filled from as many stored pages
  as needed, and its token resumes right after its last item. A token is only returned when another visible item
  follows, so paging never ends on an empty page. Under `NEBIUS_MOCKER_AUTHZ=audit|off` nothing is hidden. `filter`
  is not supported and is rejected with `INVALID_ARGUMENT`, not ignored.
- `GetByName` is authorized on the project (or tenant) it names, so a single-project grantee can also look its project
  up by name.
- **Project deletion** (v2 `Delete`) is immediate. Nebius documents "you can only delete an empty project", and the v2
  API has no scheduled-deletion states, so nothing is soft-deleted. Delete fails with `FAILED_PRECONDITION`, naming up
  to five resources, while any record of a registered resource kind (`authz.Engine.HasKind`: VPC networks and pools,
  buckets, service accounts, secrets, …) has the project as its parent. The check and the delete run in one store
  transaction (`store.ListQuery{AnyKind: true}`), without importing other areas. Internal records such as operation
  indexes do not count. `dry_run` runs the check only. Access permits on a deleted project are not removed.
- A new project is immediately a valid parent for every module: they check parents through `deps.Authz.Parent`.
  Unlike the seeded default project, it has no default VPC network or pools, so create pools before networks.

### Token exchange (`TokenExchangeService/Exchange`, HTTP `/oauth2/token/exchange`)
| `subject_token_type` | Validation | Principal |
|---|---|---|
| `urn:ietf:params:oauth:token-type:jwt`, self-signed by SA key | RS256 signature against the `AuthPublicKey` named by `kid`; `iss == sub == serviceaccount id`; key `ACTIVE` and not expired; `exp` ≤ 1h ahead | that SA |
| `…:jwt` from an external issuer | Matches a `FederatedCredentials` (issuer + subject). Signature checked against the issuer's JWKS (fetched, or a mocker-hosted fake issuer) | the SA that owns the federated credentials |
| `…:access_token` + `actor_token` | Impersonation: the actor needs `iam.serviceAccounts.impersonate` on the target SA | target SA |

The response is `CreateTokenResponse{access_token, issued_token_type, token_type="Bearer", expires_in}`. Errors follow
OAuth conventions in the HTTP form (`invalid_grant`, `invalid_request`) and `UNAUTHENTICATED`/`INVALID_ARGUMENT` in gRPC.

### Keys
- `AuthPublicKey.Create` parses the PEM and computes the fingerprint, algorithm and key size. Unsupported key types are
  rejected with `BadRequest`.
- `StaticKeyService.Issue` returns the secret **once**. `Find` looks up by token. `Revoke` invalidates it immediately.
- `iam.v2.AccessKey`: on create the module generates an AWS-style `aws_access_key_id` (`NMK…`, 20 chars) and secret.
  `GetSecret` reveals the secret, and only to principals with `iam.accessKeys.getSecret`. With
  `secret_delivery_mode=MYSTERY_BOX`, the module creates a MysteryBox secret through `peer.SystemSecrets`, puts the secret
  there, and sets `status.secret_reference_id` (see the delivery modes above). These keys are the S3 credentials for the storage data plane (M3).

### Groups and permits
- `GroupMembership.Create` validates that `member_id` exists in the same tenant. A missing member → `NOT_FOUND`.
- `AccessPermit.Create`: `resource_id` must be the tenant, a project, or a resource inside the subject group's tenant.
  The upstream comment says: *"If parent of subject is not from resource's hierarchy - NOT_FOUND will be thrown."*
  `role` must exist in the role catalog.
- Every change to permits and memberships **invalidates the authz decision cache** and bumps a generation counter that
  remote peers poll.

### Profile
`ProfileService.Get` returns `user_profile` or `service_account_profile` for the calling principal, in the shape the
upstream README example shows. `nebius iam whoami` relies on it.

### Permissions and quotas
- Permission prefixes: `iam.projects.*`, `iam.serviceAccounts.*` (+ `impersonate`), `iam.authPublicKeys.*`,
  `iam.accessKeys.*` (+ `getSecret`), `iam.groups.*`, `iam.groupMemberships.*`, `iam.accessPermits.*`, `iam.federations.*`, …
- Quotas: `iam.project.count` (tenant), `iam.serviceaccount.count` (project), `iam.authpublickey.count` (SA).

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_iam_v1_access_permit` | `nebius.iam.v1.AccessPermitService` | Create, Delete, Get + OperationService.Get | T2 |
| Resource | `nebius_iam_v1_auth_public_key` | `nebius.iam.v1.AuthPublicKeyService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_iam_v1_federated_credentials` | `nebius.iam.v1.FederatedCredentialsService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 |
| Resource | `nebius_iam_v1_federation` | `nebius.iam.v1.FederationService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T1 |
| Resource | `nebius_iam_v1_federation_certificate` | `nebius.iam.v1.FederationCertificateService` | Create, Get, Update, Delete + OperationService.Get | T1 |
| Resource | `nebius_iam_v1_group` | `nebius.iam.v1.GroupService` | Create, Get, GetByName, Delete, Update + OperationService.Get | T2 |
| Resource | `nebius_iam_v1_group_membership` | `nebius.iam.v1.GroupMembershipService` | Create, Get, Delete + OperationService.Get | T2 |
| Resource | `nebius_iam_v1_invitation` | `nebius.iam.v1.InvitationService` | Create, Get, Delete, Update + OperationService.Get | T1 |
| Resource | `nebius_iam_v1_service_account` | `nebius.iam.v1.ServiceAccountService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_iam_v2_access_key` | `nebius.iam.v2.AccessKeyService` | Create, Get, GetByName, GetSecret, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_iam_v2_project` | `nebius.iam.v2.ProjectService` | Create, Get, GetByName, Update, Delete + OperationService.Get | T2 |
| Data source | `nebius_iam_v1_access_permit` | `nebius.iam.v1.AccessPermitService` | Get | T2 |
| Data source | `nebius_iam_v1_auth_public_key` | `nebius.iam.v1.AuthPublicKeyService` | Get, GetByName | T2 |
| Data source | `nebius_iam_v1_federated_credentials` | `nebius.iam.v1.FederatedCredentialsService` | Get, GetByName | T1 |
| Data source | `nebius_iam_v1_federation` | `nebius.iam.v1.FederationService` | Get, GetByName | T1 |
| Data source | `nebius_iam_v1_federation_certificate` | `nebius.iam.v1.FederationCertificateService` | Get | T1 |
| Data source | `nebius_iam_v1_group` | `nebius.iam.v1.GroupService` | Get, GetByName | T2 |
| Data source | `nebius_iam_v1_group_membership` | `nebius.iam.v1.GroupMembershipService` | Get | T2 |
| Data source | `nebius_iam_v1_invitation` | `nebius.iam.v1.InvitationService` | Get | T1 |
| Data source | `nebius_iam_v1_project` | `nebius.iam.v1.ProjectService` | Get, GetByName | T2 |
| Data source | `nebius_iam_v1_service_account` | `nebius.iam.v1.ServiceAccountService` | Get, GetByName | T2 |
| Data source | `nebius_iam_v1_tenant` | `nebius.iam.v1.TenantService` | Get | T2 |
| Data source | `nebius_iam_v1_tenant_user_account` | `nebius.iam.v1.TenantUserAccountService` | Get | T2 |
| Data source | `nebius_iam_v2_access_key` | `nebius.iam.v2.AccessKeyService` | Get, GetByName | T2 |
| Data source | `nebius_iam_v2_project` | `nebius.iam.v2.ProjectService` | Get, GetByName | T2 |
| Data source | `nebius_iam_v2_tenant` | `nebius.iam.v2.TenantService` | Get, GetByName | T2 |
| Ephemeral | `nebius_iam_token` | `nebius.iam.v1.TokenExchangeService` | Exchange (when SA/impersonation creds); else returns current token | T2 |
| Ephemeral | `nebius_iam_v2_access_key_secret` | `nebius.iam.v2.AccessKeyService` | GetSecret | T2 |

`nebius_iam_v1_project`, `nebius_iam_v1_tenant`, `nebius_iam_v1_tenant_user_account` and `nebius_iam_v2_tenant` are data
sources only. The project data sources read seeded and created projects. The tenant ones read the seeded tenant;
`nebius_iam_v2_tenant` by name needs `parent_id = <tenant ID>`. The `iam_token` ephemeral resource returns the provider's current bearer token,
and calls `TokenExchangeService` when the provider uses SA or impersonation credentials.

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_IAM_PROJECTS` | — (only `default`) | **Implemented.** `name[:region],…`, e.g. `dev,ml:eu-west1`: extra projects seeded in the default tenant after `default`. The region defaults to the first `NEBIUS_MOCKER_REGIONS` entry. Unknown regions, duplicate names and `default` are rejected at startup. Tests use `testkit.WithProjects` |
| `NEBIUS_MOCKER_IAM_DEV_USER` | `dev@nebius-mocker.local` | Seeded admin user |
| `NEBIUS_MOCKER_IAM_ROLES_FILE` | built-in | Role → permission catalog override |
| `NEBIUS_MOCKER_IAM_FAKE_OIDC` | `1` | Serve a fake OIDC issuer at `/_mocker/oidc/<name>` for federated-credential tests |
| `NEBIUS_MOCKER_IAM_JWT_MAX_TTL` | `1h` | Max `exp` accepted on self-signed exchange JWTs |

## Open questions
- Exact NID prefixes for keys, federation, invitations and static keys.
- Real role names and the permission granularity behind them.
- Error codes and messages for each exchange failure mode (expired JWT, wrong `kid`, deactivated key).
- Whether `GroupMembership` to a blocked user is allowed.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.iam.v1.AccessKeyService` *(deprecated)*

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/access_key_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/access_key_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateAccessKeyRequest` | `common.v1.Operation` |  |
| `List` | `ListAccessKeysRequest` | `ListAccessKeysResponse` |  |
| `ListByAccount` | `ListAccessKeysByAccountRequest` | `ListAccessKeysResponse` |  |
| `Update` | `UpdateAccessKeyRequest` | `common.v1.Operation` |  |
| `GetById` | `GetAccessKeyByIdRequest` | `AccessKey` |  |
| `GetByAwsId` | `GetAccessKeyByAwsIdRequest` | `AccessKey` |  |
| `GetSecretOnce` | `GetAccessKeySecretOnceRequest` | `GetAccessKeySecretOnceResponse` |  |
| `Activate` | `ActivateAccessKeyRequest` | `common.v1.Operation` |  |
| `Deactivate` | `DeactivateAccessKeyRequest` | `common.v1.Operation` |  |
| `Delete` | `DeleteAccessKeyRequest` | `common.v1.Operation` |  |

#### `nebius.iam.v1.AccessPermitService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/access_permit_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/access_permit_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateAccessPermitRequest` | `common.v1.Operation` | ✅ |
| `List` | `ListAccessPermitRequest` | `ListAccessPermitResponse` |  |
| `Delete` | `DeleteAccessPermitRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetAccessPermitRequest` | `AccessPermit` | ✅ |

#### `nebius.iam.v1.AuthPublicKeyService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/auth_public_key_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/auth_public_key_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateAuthPublicKeyRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetAuthPublicKeyRequest` | `AuthPublicKey` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `AuthPublicKey` | ✅ |
| `List` | `ListAuthPublicKeyRequest` | `ListAuthPublicKeyResponse` |  |
| `ListByAccount` | `ListAuthPublicKeyByAccountRequest` | `ListAuthPublicKeyResponse` |  |
| `Update` | `UpdateAuthPublicKeyRequest` | `common.v1.Operation` | ✅ |
| `Activate` | `ActivateAuthPublicKeyRequest` | `common.v1.Operation` |  |
| `Deactivate` | `DeactivateAuthPublicKeyRequest` | `common.v1.Operation` |  |
| `Delete` | `DeleteAuthPublicKeyRequest` | `common.v1.Operation` | ✅ |

#### `nebius.iam.v1.FederatedCredentialsService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/federated_credentials_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/federated_credentials_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetFederatedCredentialsRequest` | `FederatedCredentials` | ✅ |
| `GetByName` | `GetByNameFederatedCredentialsRequest` | `FederatedCredentials` | ✅ |
| `List` | `ListFederatedCredentialsRequest` | `ListFederatedCredentialsResponse` |  |
| `Create` | `CreateFederatedCredentialsRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateFederatedCredentialsRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteFederatedCredentialsRequest` | `common.v1.Operation` | ✅ |

#### `nebius.iam.v1.FederationCertificateService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/federation_certificate_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/federation_certificate_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateFederationCertificateRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetFederationCertificateRequest` | `FederationCertificate` | ✅ |
| `ListByFederation` | `ListFederationCertificateByFederationRequest` | `ListFederationCertificateResponse` |  |
| `Update` | `UpdateFederationCertificateRequest` | `common.v1.Operation` | ✅ |
| `UpdateBulk` | `UpdateBulkFederationCertificateRequest` | `common.v1.Operation` |  |
| `Delete` | `DeleteFederationCertificateRequest` | `common.v1.Operation` | ✅ |

#### `nebius.iam.v1.FederationService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/federation_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/federation_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateFederationRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetFederationRequest` | `Federation` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Federation` | ✅ |
| `List` | `ListFederationsRequest` | `ListFederationsResponse` |  |
| `Update` | `UpdateFederationRequest` | `common.v1.Operation` | ✅ |
| `Activate` | `ActivateFederationRequest` | `common.v1.Operation` |  |
| `Deactivate` | `DeactivateFederationRequest` | `common.v1.Operation` |  |
| `Delete` | `DeleteFederationRequest` | `common.v1.Operation` | ✅ |

#### `nebius.iam.v1.GroupMembershipService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/group_membership_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/group_membership_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateGroupMembershipRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetGroupMembershipRequest` | `GroupMembership` | ✅ |
| `GetWithAttributes` | `GetGroupMembershipRequest` | `GroupMembershipWithAttributes` |  |
| `Delete` | `DeleteGroupMembershipRequest` | `common.v1.Operation` | ✅ |
| `ListMembers` | `ListGroupMembershipsRequest` | `ListGroupMembershipsResponse` |  |
| `ListMembersWithAttributes` | `ListGroupMembershipsRequest` | `ListGroupMembershipsWithAttributesResponse` |  |
| `ListMemberOf` | `ListMemberOfRequest` | `ListMemberOfResponse` |  |

#### `nebius.iam.v1.GroupService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/group_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/group_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateGroupRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetGroupRequest` | `Group` | ✅ |
| `GetByName` | `GetGroupByNameRequest` | `Group` | ✅ |
| `List` | `ListGroupsRequest` | `ListGroupsResponse` |  |
| `Delete` | `DeleteGroupRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateGroupRequest` | `common.v1.Operation` | ✅ |

#### `nebius.iam.v1.InvitationService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/invitation_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/invitation_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateInvitationRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetInvitationRequest` | `Invitation` | ✅ |
| `List` | `ListInvitationsRequest` | `ListInvitationsResponse` |  |
| `Delete` | `DeleteInvitationRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateInvitationRequest` | `common.v1.Operation` | ✅ |
| `Resend` | `ResendInvitationRequest` | `common.v1.Operation` |  |

#### `nebius.iam.v1.ProfileService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/profile_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/profile_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetProfileRequest` | `GetProfileResponse` |  |

#### `nebius.iam.v1.ProjectService` *(deprecated)*

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/project_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/project_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateProjectRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetProjectRequest` | `Container` | ✅ |
| `GetByName` | `GetProjectByNameRequest` | `Container` | ✅ |
| `List` | `ListProjectsRequest` | `ListProjectsResponse` |  |
| `Update` | `UpdateProjectRequest` | `common.v1.Operation` | ✅ |

#### `nebius.iam.v1.ServiceAccountService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/service_account_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/service_account_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateServiceAccountRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetServiceAccountRequest` | `ServiceAccount` | ✅ |
| `GetByName` | `GetServiceAccountByNameRequest` | `ServiceAccount` | ✅ |
| `List` | `ListServiceAccountRequest` | `ListServiceAccountResponse` |  |
| `Update` | `UpdateServiceAccountRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteServiceAccountRequest` | `common.v1.Operation` | ✅ |

#### `nebius.iam.v1.SessionManagementService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/session_management_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/session_management_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Revoke` | `RevokeSessionRequest` | `RevokeSessionResponse` |  |

#### `nebius.iam.v1.StaticKeyService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/static_key_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/static_key_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Issue` | `IssueStaticKeyRequest` | `IssueStaticKeyResponse` |  |
| `List` | `ListStaticKeysRequest` | `ListStaticKeysResponse` |  |
| `Get` | `GetStaticKeyRequest` | `StaticKey` |  |
| `GetByName` | `GetStaticKeyByNameRequest` | `StaticKey` |  |
| `Delete` | `DeleteStaticKeyRequest` | `common.v1.Operation` |  |
| `Find` | `FindStaticKeyRequest` | `FindStaticKeyResponse` |  |
| `Revoke` | `RevokeStaticKeyRequest` | `common.v1.Operation` |  |

#### `nebius.iam.v1.TenantService` *(deprecated)*

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/tenant_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/tenant_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetTenantRequest` | `Container` | ✅ |
| `List` | `ListTenantsRequest` | `ListTenantsResponse` |  |

#### `nebius.iam.v1.TenantUserAccountService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/tenant_user_account_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/tenant_user_account_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetTenantUserAccountRequest` | `TenantUserAccount` | ✅ |
| `List` | `ListTenantUserAccountsRequest` | `ListTenantUserAccountsResponse` |  |
| `Block` | `BlockTenantUserAccountRequest` | `common.v1.Operation` |  |
| `Unblock` | `UnblockTenantUserAccountRequest` | `common.v1.Operation` |  |

#### `nebius.iam.v1.TenantUserAccountWithAttributesService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/tenant_user_account_with_attributes_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/tenant_user_account_with_attributes_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetTenantUserAccountWithAttributesRequest` | `TenantUserAccountWithAttributes` |  |
| `List` | `ListTenantUserAccountsWithAttributesRequest` | `ListTenantUserAccountsWithAttributesResponse` |  |

#### `nebius.iam.v1.TokenExchangeService`

Endpoint: `tokens.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v1/token_exchange_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v1/token_exchange_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Exchange` | `ExchangeTokenRequest` | `CreateTokenResponse` |  |

#### `nebius.iam.v2.AccessKeyService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v2/access_key_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v2/access_key_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateAccessKeyRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetAccessKeyRequest` | `AccessKey` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `AccessKey` | ✅ |
| `GetSecret` | `GetAccessKeySecretRequest` | `GetAccessKeySecretResponse` | ✅ |
| `List` | `ListAccessKeysRequest` | `ListAccessKeysResponse` |  |
| `Update` | `UpdateAccessKeyRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteAccessKeyRequest` | `common.v1.Operation` | ✅ |
| `Activate` | `ActivateAccessKeyRequest` | `common.v1.Operation` |  |
| `Deactivate` | `DeactivateAccessKeyRequest` | `common.v1.Operation` |  |
| `ListByAccount` | `ListAccessKeysByAccountRequest` | `ListAccessKeysResponse` |  |
| `GetByAwsId` | `GetAccessKeyByAwsIdRequest` | `AccessKey` |  |
| `DeleteByAwsId` | `DeleteAccessKeyByAwsIdRequest` | `common.v1.Operation` |  |
| `ActivateByAwsId` | `ActivateAccessKeyByAwsIdRequest` | `common.v1.Operation` |  |
| `DeactivateByAwsId` | `DeactivateAccessKeyByAwsIdRequest` | `common.v1.Operation` |  |

#### `nebius.iam.v2.ProjectService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v2/project_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v2/project_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Create` | `CreateProjectRequest` | `common.v1.Operation` | ✅ |
| `Get` | `GetProjectRequest` | `Project` | ✅ |
| `GetByName` | `GetProjectByNameRequest` | `Project` | ✅ |
| `List` | `ListProjectsRequest` | `ListProjectsResponse` |  |
| `Update` | `UpdateProjectRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteProjectRequest` | `common.v1.Operation` | ✅ |

#### `nebius.iam.v2.TenantService`

Endpoint: `cpl.iam.api.nebius.cloud:443` · Proto: [`nebius/iam/v2/tenant_service.proto`](https://github.com/nebius/api/blob/main/nebius/iam/v2/tenant_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetTenantRequest` | `Tenant` | ✅ |
| `GetByName` | `GetTenantByNameRequest` | `Tenant` | ✅ |
| `List` | `ListTenantsRequest` | `ListTenantsResponse` |  |
| `Update` | `UpdateTenantRequest` | `common.v1.Operation` | ✅ |
