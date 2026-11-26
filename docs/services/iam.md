# IAM — Identity and Access Management

> Module: `services/iam` · Image: `nebius-mocker-iam` · `NEBIUS_MOCKER_SERVICES=iam`
> M1 tier: **T2** · Endpoints: `cpl.iam`, `tokens.iam`, plus HTTP `/oauth2/token/exchange`, `/_mocker/oidc/…`
> (fake OIDC issuer) and `/_mocker/iam/invitations/{id}/accept`

## Implementation status

| Piece | Status |
|---|---|
| Seeded default tenant (`tenant-e00mock0000000000default`) and default project (`project-e00mock000000000default`, region `eu-north1`), plus extra projects from `NEBIUS_MOCKER_IAM_PROJECTS` | ✅ |
| `nebius.iam.v2.ProjectService`: Create, Get, GetByName, List (paged, filtered to what the caller may read), Update (name and labels; `spec.region` and the tenant are immutable), Delete (only an empty project; `dry_run`) | ✅ T1 |
| `nebius.iam.v1.ProjectService` *(deprecated)*: Create, Get, GetByName, List, Update (same rules; v1 has no Delete) | ✅ T1 |
| `nebius.iam.v1.TenantService` *(deprecated)*: Get, List; `nebius.iam.v2.TenantService`: Get, GetByName, List, Update (name and labels with reset mask and `resource_version`; `name_prefix` is refused by name). List returns the tenants the caller may read | ✅ T1 |
| `nebius_iam_v2_project` (+ `nebius_iam_v1_project`, `nebius_iam_v2_project`, `nebius_iam_v1_tenant`, `nebius_iam_v2_tenant` data sources), and `nebius iam [v2] project …` / `nebius iam [v2] tenant list` | ✅ e2e: `test/terraform/iam/project_test.go`, `tenant_test.go`, `test/cli/iam/project_e2e_test.go` |
| A default VPC network in newly created projects; quotas (`iam.project.count`) | Not yet |
| `nebius.iam.v1.GroupService`: Create, Get, GetByName, List (paged), Update (reset mask, `resource_version`), Delete | ✅ T1 |
| `nebius.iam.v1.GroupMembershipService`: Create, Get, GetWithAttributes, Delete, ListMembers, ListMembersWithAttributes, ListMemberOf. `status` carries the member's current tenant-user-account or service-account status (computed on read). A `tenantuseraccount-…` member must exist in the group's tenant (`NOT_FOUND`), and one whose invitation has expired is refused (`FAILED_PRECONDITION`) | ✅ T1 |
| `nebius.iam.v1.AccessPermitService`: Create, Get, Delete, List | ✅ T1 |
| Seeded `admins` / `editors` / `viewers` groups with tenant-level `admin` / `editor` / `viewer` permits | ✅ |
| Authorization (`NEBIUS_MOCKER_AUTHZ=off\|audit\|enforce`) from memberships + permits, inherited down the hierarchy | ✅ |
| `nebius_iam_v1_group`, `_group_membership`, `_access_permit` with the official Terraform provider | ✅ e2e: `test/terraform/iam` |
| Static dev tokens: `nmock-dev-admin`, `nmock-dev-editor`, `nmock-dev-viewer` (members of the seeded groups) | ✅ |
| `nebius.iam.v1.ProfileService.Get` (whoami; the `nebius` CLI calls it before `iam get-access-token`), derived from the token's principal; service-account callers get the stored `ServiceAccount` | ✅ |
| `nebius.iam.v1.ServiceAccountService`: Create, Get, GetByName, List, Update (reset mask), Delete (cascades to the account's access keys). Parent must be a project | ✅ T1 |
| `nebius.iam.v2.AccessKeyService`: Create, Get, GetByName, GetSecret, List, ListByAccount, GetByAwsId, DeleteByAwsId, ActivateByAwsId, DeactivateByAwsId, Update, Delete, Activate, Deactivate | ✅ T1 |
| Access keys as S3 credentials: Object Storage's S3 front verifies SigV4 with them (`peer.AccessKeys` capability in `pkg/peer`, provided by IAM) | ✅ |
| Project regions for other areas (`peer.ProjectRegions`: a project's `spec.region`; Compute checks GPU cluster fabrics with it) | ✅ |
| `nebius_iam_v1_service_account`, `nebius_iam_v2_access_key` (+ data sources) and the `nebius_iam_v2_access_key_secret` ephemeral resource | ✅ e2e: `test/terraform/storage` |
| `secret_delivery_mode = MYSTERY_BOX`: the secret is delivered to a MysteryBox secret (`status.secret_reference_id`), read with `nebius_mysterybox_v1_secret_payload_entry` / `nebius mysterybox payload get` and used against the S3 front | ✅ e2e: `test/terraform/iam` (lifecycle in `make e2e`; the S3 use with RustFS in `make e2e-storage`), `test/cli/iam` (`make e2e-storage`) |
| `nebius.iam.v1.AuthPublicKeyService`: Create, Get, GetByName, List, ListByAccount, Update (reset mask), Activate, Deactivate, Delete. Status `state`/`fingerprint`/`algorithm`/`key_size` computed from `spec.data` | ✅ T1 (see [Service-account keys and token exchange](#service-account-keys-and-token-exchange)) |
| `nebius.iam.v1.TokenExchangeService.Exchange` (gRPC, `tokens.iam`) and `POST /oauth2/token/exchange` (HTTP form, RFC 8693) on the edge port: service-account JWT exchange and impersonation | ✅ |
| Issued access tokens (`nmock.v1.…`, HMAC-signed, 12h, revocable) accepted by every gRPC service and the registry's docker front, next to the static dev tokens | ✅ |
| Service-account credentials in the official tools: gosdk `ServiceAccountReader` (credentials file, private key file), Terraform provider `service_account` block, `nebius profile create --service-account-file`, `nebius iam auth-public-key create/generate` | ✅ contract: `test/contract/iam_token*_test.go`, `iam_authkey*_test.go`; e2e: `test/terraform/iam/sa_auth_test.go`, `test/cli/iam/sa_auth_e2e_test.go` |
| `nebius_iam_v1_auth_public_key` resource and data source | ✅ e2e: `test/terraform/iam/sa_auth_test.go` |
| `nebius_iam_token` ephemeral resource: with `service_account` credentials it yields the token the provider exchanged (it makes no RPC of its own); a second provider configured with only that token works as the service account | ✅ e2e: `TestProviderIAMTokenEphemeral` in `test/terraform/iam/sa_auth_test.go` |
| Keys live in their service account's project: a key parented in another existing project is refused (`metadata.parent_id`), even for an editor of that project, and its key material mints no token | ✅ contract: `test/contract/iam_authkey_crossproject_test.go` |
| `nebius.iam.v1.FederationService`: Create, Get, GetByName, List, Update (reset mask), Activate, Deactivate, Delete; `nebius.iam.v1.FederationCertificateService`: Create, Get, ListByFederation, Update, UpdateBulk, Delete (see [Federations and certificates](#federations-and-certificates)) | ✅ T1 |
| `nebius_iam_v1_federation`, `nebius_iam_v1_federation_certificate` resources and data sources | ✅ e2e: `test/terraform/iam/federation_test.go` |
| `nebius.iam.v1.FederatedCredentialsService`: Get, GetByName, List, Create, Update, Delete, and the federated token exchange (an external OIDC JWT for a service-account token), with a built-in fake OIDC issuer (see [Federated credentials](#federated-credentials-workload-identity)) | ✅ T1 |
| `nebius_iam_v1_federated_credentials` resource and data source; gosdk `auth.NewFederatedCredentialsTokener` and the profile key `federated-subject-credentials-file-path`; `nebius profile create --service-account-id … --federated-subject-credentials-file-path …` | ✅ contract: `test/contract/iam_federatedcreds_test.go`; e2e: `test/terraform/iam/federatedcreds_test.go`, `test/cli/iam/federated_e2e_test.go` |
| `nebius.iam.v1.InvitationService`: Create, Get, List, Update, Resend, Delete; acceptance through the mock control endpoint `POST /_mocker/iam/invitations/{id}/accept` (see [Invitations](#invitations-and-tenant-user-accounts)) | ✅ T1 |
| `nebius.iam.v1.TenantUserAccountService`: Get, List, Block, Unblock; `nebius.iam.v1.TenantUserAccountWithAttributesService`: Get, List. The static-token users are seeded as tenant user accounts, and invitations create more | ✅ T1 |
| `nebius_iam_v1_invitation` resource and data source, `nebius_iam_v1_tenant_user_account` data source, and the `nebius_iam_v1_group_membership` / `nebius_iam_v1_access_permit` data sources | ✅ e2e: `test/terraform/iam/invitation_test.go` |
| `nebius.iam.v1.SessionManagementService.Revoke`: `service_account_id` and `tenant_user_account_id` (both need the admin-only `revokeSessions` verb), and `all_my_active` (any caller, for their own tokens). It revokes issued tokens; static dev tokens are configuration, not sessions, and stay valid | ✅ T1 (contract: `test/contract/iam_gaps_test.go`) |
| `nebius.iam.v1.StaticKeyService`, deprecated `nebius.iam.v1.AccessKeyService` | Not yet (`UNIMPLEMENTED`, see [Not yet](#not-yet)) |

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

### Service-account keys and token exchange

How a service account authenticates, end to end (the flow in the
[nebius/api README](https://github.com/nebius/api#authentication-with-service-accounts)):

1. Upload an RSA public key with one of:
   - `nebius iam auth-public-key create --account-service-account-id $SA_ID --data "$(cat public.pem)"`;
   - `nebius iam auth-public-key generate --service-account-id $SA_ID --output=creds.json`, which also writes the
     gosdk/CLI credentials file;
   - Terraform `nebius_iam_v1_auth_public_key`;
   - `AuthPublicKeyService.Create`.
2. The client signs an RS256 JWT (`kid` is the key ID `publickey-…`, `iss` = `sub` is the service account ID, plus
   `exp`) and exchanges it. gosdk, the provider and the CLI do this themselves over gRPC
   (`nebius.iam.v1.TokenExchangeService/Exchange` on the edge). Over HTTP:

   ```bash
   curl http://localhost:4566/oauth2/token/exchange \
     -d "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
     -d "requested_token_type=urn:ietf:params:oauth:token-type:access_token" \
     -d "subject_token=${JWT}" \
     -d "subject_token_type=urn:ietf:params:oauth:token-type:jwt"
   ```

   The HTTP endpoint is served on the edge port (`:4566`; `https://` when `NEBIUS_MOCKER_TLS_*` is set). Where the
   README uses `https://auth.eu.nebius.com:443/oauth2/token/exchange`, use `http://<mocker>:4566/oauth2/token/exchange`.
3. The response carries:
   - `access_token` (`nmock.v1.…`);
   - `issued_token_type`;
   - `token_type: Bearer`;
   - `expires_in` in seconds (`NEBIUS_MOCKER_TOKEN_TTL`, default 12h, the lifetime of Nebius IAM tokens).

   Requests made with the token run as the service account. Authorization uses the SA's group memberships and
   permits, and audit events name the SA.

**Keys.**
- The parent (`metadata.parent_id`) must be the service account's project, which is also what the CLI's `generate`
  uses by default. A key for an SA in another project is refused. This way, editor rights in one project cannot mint
  credentials for an SA elsewhere.
- Only `spec.account.service_account` is supported. User-account keys are refused with `BadRequest` on `spec.account`.
- `spec.data` must be a single PEM block holding an RSA key of at least 2048 bits. Either `PUBLIC KEY` (PKIX, what
  `openssl rsa -pubout` writes) or `RSA PUBLIC KEY` (PKCS#1) is accepted. Anything else is refused with `BadRequest`
  naming `spec.data`.
- `spec.account`, `spec.expires_at` and `spec.data` are immutable. Name, labels and description update in place.

| Status field | Value |
|---|---|
| `state` | `ACTIVE` on create. `INACTIVE` after `Deactivate`, and `ACTIVE` again after `Activate`. `EXPIRED` once `spec.expires_at` has passed on the mocker clock (computed on read). `Delete` removes the key (`Get` returns `NOT_FOUND`), so `DELETING` and `DELETED` are never reported |
| `fingerprint` | Lower-case hex SHA-256 of the DER `SubjectPublicKeyInfo`. It is the same for a PKIX and a PKCS#1 upload of one key. Upstream does not document the format |
| `algorithm` | `RSA` |
| `key_size` | Modulus size in bits |

**Key age and expiry.** Nebius has no `status.created_at` or `status.expires_at`. The creation time is
`metadata.created_at`, and the expiry is `spec.expires_at`. Tools that report key age read `metadata.created_at`.

**Exchange checks.** Any failed check refuses the exchange. Over HTTP the response is status 400 with OAuth JSON
`{"error": …, "error_description": …}`. Over gRPC it is the mapped code:

| Check | OAuth `error` | gRPC |
|---|---|---|
| `grant_type` is `urn:ietf:params:oauth:grant-type:token-exchange` | `unsupported_grant_type` | `INVALID_ARGUMENT` |
| `requested_token_type` is empty or `…:access_token`; `subject_token` and `subject_token_type` are present; HTTP only: the body is form-encoded and no parameter repeats | `invalid_request` | `INVALID_ARGUMENT` |
| `scopes` (HTTP `scope`), `audience` and `resource` are unsupported and refused by name | `invalid_scope` / `invalid_target` | `INVALID_ARGUMENT` |
| `subject_token_type` is `…:jwt`, or `urn:nebius:params:oauth:token-type:subject_identifier` with `actor_token_type` `…:access_token` (impersonation) or `…:jwt` (federated credentials) | `invalid_request` | `INVALID_ARGUMENT` |
| See the JWT checks below | `invalid_grant` | `UNAUTHENTICATED` |
| See the impersonation checks below | `invalid_grant` / `unauthorized_client` | `UNAUTHENTICATED` / `PERMISSION_DENIED` |

JWT checks, all reported as `invalid_grant` / `UNAUTHENTICATED`:
- `alg` must be RS256. `none`, HS*, RS512, PS* and ES* are refused before any key lookup.
- `kid` must name an existing `publickey-…`. Kids from external issuers are refused as federated.
- The key must be `ACTIVE`, not `INACTIVE` or `EXPIRED`.
- `iss` = `sub` must be the key's service account.
- The signature must verify.
- `exp` must be present, in the future, and at most `NEBIUS_MOCKER_IAM_JWT_MAX_TTL` (1h) ahead.
- `iat` and `nbf` must not be in the future (30s skew allowed), and `exp - iat` must not exceed the max TTL.
- The service account must exist and be active.

Impersonation checks:
- `actor_token` must be a valid mocker token (static or issued), with `actor_token_type` `…:access_token`. Otherwise
  `invalid_grant` / `UNAUTHENTICATED`.
- `subject_token` must be an existing SA. Otherwise `invalid_grant` / `UNAUTHENTICATED`.
- The actor needs `iam.serviceAccounts.impersonate` on the SA. Only `admin` grants it, and it is checked according to
  `NEBIUS_MOCKER_AUTHZ`. A denial is `unauthorized_client` / `PERMISSION_DENIED`.

This is the flow behind the gosdk/provider `impersonate_service_account_id`. An `actor_token` sent with a JWT subject
is refused (`invalid_request`).

**Revocation.** Nebius does not document whether deactivating a key ends the sessions it already opened. The mock
ends them, because local development uses deactivation to check that a key really stops working:
- `Deactivate` and `Delete` of a key revoke every token exchanged with it. Tokens issued after a later `Activate`
  work.
- Deleting a service account deletes its auth public keys and access keys. It also revokes all its tokens, including
  tokens it obtained by impersonation.
- Expired tokens are refused (`UNAUTHENTICATED`) once their `expires_in` has passed on the mocker clock.

Tokens are verified statelessly with `NEBIUS_MOCKER_TOKEN_SECRET`. The secret is random per process by default, so
tokens do not survive a restart. The revocation deny-list is kept in process memory.

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100 (re-verified for
`ProjectService`/`TenantService` v1+v2 and for `AuthPublicKeyService`/`TokenExchangeService` on 2026-11-25; for
federations, federation certificates, federated credentials, invitations, tenant user accounts, session management and
the `*ByAwsId` access-key RPCs on 2026-11-26; `nebius` CLI 0.12.312).

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
| `…:jwt` from an external issuer, sent as `subject_token` | Refused (`invalid_grant`). The message points to the federated flow below | — |
| `urn:nebius:params:oauth:token-type:subject_identifier` (the SA ID) + `actor_token` of type `…:access_token` | Impersonation: the actor needs `iam.serviceAccounts.impersonate` on the target SA | target SA |
| `urn:nebius:params:oauth:token-type:subject_identifier` (the SA ID) + `actor_token` of type `…:jwt` (an external OIDC JWT) | Federated credentials of that SA trust the token's `iss` and `sub`, and its signature verifies (see [Federated credentials](#federated-credentials-workload-identity)) | that SA |

The response is `CreateTokenResponse{access_token, issued_token_type, token_type="Bearer", expires_in}`. The checks and
error mapping are listed under [Service-account keys and token exchange](#service-account-keys-and-token-exchange).

### Federations and certificates
- A federation's parent is a tenant. `metadata.name` is required and unique in the tenant. `spec.saml_settings` is
  required: `idp_issuer`, and `sso_url` as an absolute http(s) URL. `force_authn` and `user_account_auto_creation`
  are stored.
- The deprecated `spec.active` is stored and echoed, but has no effect, as upstream documents ("'active' field
  ignored. Use Activate/Deactivate"). A new federation is `ACTIVE`, and `Deactivate`/`Activate` toggle `status.state`.
- **Delete needs an inactive federation**, as the proto says ("An active federation cannot be deleted and must be
  deactivated first"). Deleting an active one fails with `FAILED_PRECONDITION`. Terraform has no attribute for this,
  so deactivate the federation (`nebius iam federation deactivate --federation-id …` or the SDK) before
  `terraform destroy`, as on Nebius. Deleting a federation deletes its certificates and keeps the tenant user
  accounts linked to it.
- `status.users_count` counts the tenant user accounts linked to the federation (accepted invitations, see below).
  `status.certificates_count` counts its certificates. Both are computed on read.
- A certificate's parent is a federation. `spec.data` must be one PEM `CERTIFICATE` block holding an X.509 certificate.
  It is immutable, so Terraform replaces the resource when it changes. Status fields:
  - `fingerprint` is the lower-case hex SHA-256 of the DER certificate (upstream does not document the format);
  - `algorithm` is `RSA`, `ECDSA` or `ED25519`, and `key_size` is in bits;
  - `not_before` and `not_after` come from the certificate;
  - `state` is `EXPIRED` once `not_after` has passed on the mocker clock, `ACTIVE` before.

  Name, labels and description update in place.
- `UpdateBulk` replaces the federation's whole certificate set in one transaction. An update with `metadata.id` changes
  that certificate, one without an ID creates a new one, and certificates not mentioned are removed. Everything is
  validated before anything changes.
- No SAML sign-in happens (see [Not yet](#not-yet)).

### Federated credentials (workload identity)
A workload that holds a JWT from an OIDC issuer exchanges it for a token of a service account, without holding any
key of that service account. This is the flow of gosdk's `auth.NewFederatedCredentialsTokener`, and of a CLI or SDK
profile with `service-account-id` and `federated-subject-credentials-file-path`.

1. Create the trust with `nebius iam federated-credentials create --parent-id <project> --subject-id <SA>
   --oidc-provider-issuer-url <issuer> --federated-subject-id <sub>`, or `nebius_iam_v1_federated_credentials`:
   - The parent must be a project, and `spec.subject_id` a service account in that project (as for auth keys).
   - `issuer_url` must be an absolute http(s) URL.
   - `federated_subject_id` is **required by the mock**. Upstream allows it empty, but the mock does not trust every
     subject of an issuer, so an empty one is refused by name.
   - `jwk_set_json`, when set, must be a JWKS with at least one usable key, and is used instead of the issuer's
     published keys. Without it, the mock fetches `<issuer_url>/.well-known/openid-configuration` and its `jwks_uri`
     (10s timeout, cached for 5 minutes on the mocker clock, refetched once for an unknown `kid`).
2. The workload exchanges its JWT. The request has `subject_token` = the SA ID, `subject_token_type` =
   `urn:nebius:params:oauth:token-type:subject_identifier`, `actor_token` = the JWT and `actor_token_type` =
   `urn:ietf:params:oauth:token-type:jwt`, over gRPC or `POST /oauth2/token/exchange`. The gosdk and the CLI build it
   from a profile (the endpoint must be `localhost:<port>`, and plaintext needs `--insecure` on every command):

   ```bash
   nebius profile create wif --endpoint localhost:4566 --parent-id <project> \
     --service-account-id <SA> --federated-subject-credentials-file-path ./token.jwt
   nebius iam whoami --profile wif --insecure     # prints the service account
   ```
3. The exchange checks, all refused with `invalid_grant`/`UNAUTHENTICATED` (a malformed JWT is `invalid_request`):
   - the SA exists and is active;
   - a federated credential of the SA has `issuer_url` = the token's `iss` (one trailing slash tolerated) and
     `federated_subject_id` = its `sub`;
   - `alg` is RS256 (RSA keys of at least 2048 bits) or ES256, and the signature verifies;
   - `exp` is present and in the future, and `iat`/`nbf` are not in the future (30s skew).

   `aud` is not checked, because Nebius documents no audience for this exchange. `NEBIUS_MOCKER_IAM_JWT_MAX_TTL` does
   not apply to external JWTs.
4. Requests with the token run as the SA. Deleting the federated credentials, or changing their spec, revokes the
   tokens issued through them; a label or name change does not. Deleting the SA stops the exchange.

**Fake OIDC issuer** (`NEBIUS_MOCKER_IAM_FAKE_OIDC`, on by default). The edge port serves one issuer per name, so a
workload can be simulated without running an identity provider:

| Endpoint | Returns |
|---|---|
| `GET /_mocker/oidc/<name>/.well-known/openid-configuration` | Discovery. `issuer` is `<scheme>://<host>/_mocker/oidc/<name>`, from the request, plus `jwks_uri` |
| `GET /_mocker/oidc/<name>/jwks` | The issuer's public key (RSA 2048, created on first use, kept in memory) |
| `POST /_mocker/oidc/<name>/token` with form fields `sub` (required), `aud`, `ttl` (Go duration, default `1h`) | The signed RS256 JWT as the body (`application/jwt`), so `curl -o token.jwt` writes the credentials file |

```bash
curl -s -o token.jwt http://localhost:4566/_mocker/oidc/ci/token -d sub=system:serviceaccount:ci:builder
# the issuer URL for the federated credentials is http://localhost:4566/_mocker/oidc/ci
```

The issuer's keys are lost on restart, and unknown form fields are refused. Any other OIDC issuer the mock can reach
over HTTP(S) works the same way. Under TLS, the mock trusts its own certificate when it fetches keys.

### Invitations and tenant user accounts
- Every static-token principal that is a `tenantuseraccount-…` (the dev users, and the users in
  `NEBIUS_MOCKER_DEV_TOKENS`) is seeded as an `ACTIVE` tenant user account of the default tenant.
- `InvitationService.Create` takes a tenant parent, a valid `spec.email` (required) and `expires_in` (default 72h, as
  the docs say). It creates a tenant user account at once, with `status.invitation_id` set. The account's ID is the
  invitation's `status.tenant_user_account_id`, usable as a group-membership `member_id` before acceptance, as in the
  docs' Terraform example. The invitation is `PENDING` (sent), or `CREATED` with `no_send`.
- `EXPIRED` is computed on read once `expires_at` has passed while the invitation is `CREATED` or `PENDING`. An invitee
  whose invitation expired cannot be added to a group (`FAILED_PRECONDITION`) until it is resent.
- `Resend` (from `CREATED`, `PENDING` or `EXPIRED`) makes the invitation `PENDING` again, with `expires_at` = now + the
  original validity. `Update` changes the name, labels, description and email, except once `ACCEPTED`. `Delete`
  cancels the invitation. If it was not accepted, its tenant user account and that account's memberships go too.
- **Acceptance** happens in the invitee's browser on Nebius, through an emailed link with a secret code. There is no
  public RPC, so the mock has a control endpoint that stands in for that click:

  ```bash
  curl -X POST http://localhost:4566/_mocker/iam/invitations/<id>/accept [-d federation_id=<federation>]
  ```

  Only a `PENDING` invitation can be accepted (`CREATED` answers "not sent yet, call Resend"; `EXPIRED` and `ACCEPTED`
  answer 409). The invitation becomes `ACCEPTED`. The account loses `invitation_id` and gets
  `user_account_state ACTIVE`. With `federation_id` (an `ACTIVE` federation of the same tenant), the account is linked
  to that federation, which then counts it in `users_count`. The endpoint is unauthenticated, like the emailed link.
- `TenantUserAccountService.Block` makes an account `BLOCKED`. As upstream describes, the user can still authenticate
  but is authorized for nothing (`PERMISSION_DENIED`) until `Unblock`.
- `TenantUserAccountWithAttributesService` returns what the mock knows: the email of invited accounts, and `sub` (the
  account ID) once an account is real (seeded or accepted).

### Keys
- `AuthPublicKey.Create` parses the PEM and computes the fingerprint, algorithm and key size. Unsupported key types are
  rejected with `BadRequest` naming `spec.data` (details above).
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
| `NEBIUS_MOCKER_IAM_FAKE_OIDC` | `true` | **Implemented.** Serve the fake OIDC issuer at `/_mocker/oidc/<name>` on the edge port. `false` removes the routes |
| `NEBIUS_MOCKER_IAM_JWT_MAX_TTL` | `1h` | Max `exp` accepted on self-signed exchange JWTs |
| `NEBIUS_MOCKER_TOKEN_SECRET` | random per process | HMAC key of issued access tokens (at least 32 bytes). Processes sharing it accept each other's tokens |
| `NEBIUS_MOCKER_TOKEN_TTL` | `12h` | Lifetime of issued access tokens (`expires_in`) |

Of these, `IAM_JWT_MAX_TTL`, `IAM_FAKE_OIDC`, `TOKEN_SECRET` and `TOKEN_TTL` are implemented; the others are planned.

### Not yet
- `StaticKeyService` (`UNIMPLEMENTED`). A static key is only useful if its token then authenticates against the
  registry, observability and AI Studio data planes, which needs a token lookup in every front. Deferred as scope.
- The deprecated `nebius.iam.v1.AccessKeyService` (`UNIMPLEMENTED`). It is superseded by `iam.v2.AccessKeyService`,
  which the provider and the CLI use.
- SAML sign-in. Federations and their certificates are stored and validated, but no SAML login flow is served. So
  `user_account_auto_creation` has no effect, and federation users exist only through accepted invitations.
- Sending invitation emails, and the web console's invitation flows (`CreateInvitationToGroups`, accepting by code).
  Acceptance goes through the mock control endpoint instead.
- `filter` on List RPCs: refused with `INVALID_ARGUMENT`.
- An external JWT sent as `subject_token` instead of `actor_token`: refused. The gosdk and the CLI send it as
  `actor_token`.
- An admin API for minting user tokens.
- Scoped, audience- or resource-restricted tokens (`scopes`, `audience`, `resource`): refused by name.
- The revocation deny-list is per process: split deployments sharing `NEBIUS_MOCKER_TOKEN_SECRET` do not share revocations.
- Quotas on keys (`iam.authpublickey.count`).
- The HTTP exchange ignores unrecognized form parameters, as RFC 6749 §3.2 requires; every parameter of
  `ExchangeTokenRequest` is either implemented or refused by name.

## Open questions
- Exact NID prefixes for static keys. The mock uses `federation`, `federationcertificate`, `federatedcredentials`
  and `invitation`; `tenantuseraccount` is upstream's.
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
