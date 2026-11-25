# IAM — Identity and Access Management

> Module: `services/iam` · Image: `nebius-mocker-iam` · `NEBIUS_MOCKER_SERVICES=iam`
> M1 tier: **T2** · Endpoints: `cpl.iam`, `tokens.iam`, plus HTTP `/oauth2/token/exchange`

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
- At boot the module seeds the default tenant and project, the `admins/editors/viewers` groups with tenant-level permits,
  and a dev user (architecture/06 § Default bootstrap). The tenant list and project regions come from seed files.
- `ProjectService.Create` (v1 is deprecated, v2 is current) validates the region against `NEBIUS_MOCKER_REGIONS` and
  stamps the routing code used for every NID under it.
- **Project deletion** (v2) is refused while the project contains any resource from any module. The check uses the
  reverse-reference index, plus `peer` queries to remote modules in split mode.

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
  `secret_delivery_mode=MYSTERYBOX`, the module creates a MysteryBox secret through `peer`, puts the secret there, and sets
  `status.secret_reference_id`. These keys are the S3 credentials for the storage data plane (M3).

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
sources only. They read seeded data. The `iam_token` ephemeral resource returns the provider's current bearer token,
and calls `TokenExchangeService` when the provider uses SA or impersonation credentials.

## Configuration

| Env | Default | Description |
|---|---|---|
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
