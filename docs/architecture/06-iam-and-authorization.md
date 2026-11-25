# 06 — IAM, authentication and authorization

The `iam` module is the root of every other service. Each resource lives under a project, each project under a tenant,
and each request is made by a principal. This document covers the cross-cutting model. The full IAM API surface is in
[services/iam.md](../services/iam.md).

## Resource hierarchy

```mermaid
flowchart TD
  T[tenant-e00…<br/>Container] --> P1[project-e00…<br/>Container, region=eu-north1]
  T --> P2[project-e01…<br/>region=eu-west1]
  T --> G[group-e00…<br/>e.g. 'admins','editors','viewers']
  T --> TUA[tenantuseraccount-…]
  P1 --> SA[serviceaccount-e00…]
  P1 --> RES[compute / vpc / mk8s / … resources]
  G --> GM[groupmembership → member: SA or user]
  G --> AP[accesspermit<br/>parent = group (subject)<br/>resource_id = tenant/project/resource<br/>role = 'editor']
  SA --> APK[authpublickey]
  SA --> SK[statickey]
  SA --> AK[accesskey (S3-style)]
```

- Tenants and projects are `nebius.iam.v1.Container` messages (`TenantService` and `ProjectService` return `Container`).
  There are also `iam/v2` variants, which the provider uses for `nebius_iam_v2_project` and `nebius_iam_v2_tenant`.
- Tenants cannot be created via the public API (`TenantService` has only `Get`/`List`, and `Update` in v2). The mocker
  **seeds** them from config.
- Every project has a region. The region drives the NID routing code, quota scoping and image catalogs.

### Default bootstrap

On first start, with empty state, the mocker seeds:

| Resource | ID (sequential mode) | Notes |
|---|---|---|
| Tenant `default` | `tenant-e00mock0000000000default` | |
| Project `default` (region `eu-north1`) | `project-e00mock0000000000default` | `parent_id` default for examples |
| Groups `admins`, `editors`, `viewers` | `group-e00mock…` | Access permits on the tenant with roles `admin`, `editor`, `viewer` |
| User account `dev@nebius-mocker.local` | `tenantuseraccount-e00mock…` | Member of `admins` |
| Static dev token `nmock-dev-admin` | — | Authenticates as the dev user (configurable, can be disabled) |
| Public images project `project-e00public-images` | — | Owns the public image catalog (`ImageService.GetLatestByFamily` defaults to `project-{region}public-images`) |

All of it can be overridden through a seed file ([11](11-configuration.md#seed-data)).

## Authentication

### Principals

| Principal | How it authenticates against the mocker |
|---|---|
| **User account** | Static dev tokens from config (`NEBIUS_MOCKER_DEV_TOKENS`). *Planned:* tokens minted via an admin API (`POST /_mocker/iam/tokens {"subject": "tenantuseraccount-…"}`) |
| **Service account (JWT exchange)** | The real flow: the client signs an RS256 JWT with `kid=<authpublickey id>`, `iss=sub=<serviceaccount id>` and calls `TokenExchangeService/Exchange` (gRPC) or `POST /oauth2/token/exchange` (HTTP form). The mocker verifies the signature against the **public key uploaded via `AuthPublicKeyService`**, checks `exp` and key state, and returns `CreateTokenResponse{access_token, expires_in, token_type=Bearer}` |
| **Service account (credentials file)** | The same flow. The provider's `service_account.credentials_file` is parsed client-side by gosdk, which then does the JWT exchange |
| **Federated credentials** | *Not yet.* JWTs from external issuers are refused by name in the exchange. Planned: `subject_token_type=…:jwt` from an external OIDC issuer configured in `FederatedCredentialsService`, verified against the issuer's JWKS, with a fake OIDC issuer at `/_mocker/oidc/<name>` for tests |
| **Impersonation** | `subject_token` = the target SA ID with `subject_token_type=urn:nebius:params:oauth:token-type:subject_identifier`, plus the caller's `actor_token` (the provider's `impersonate_service_account_id`). The actor needs `iam.serviceAccounts.impersonate` on the SA (granted by `admin`) |
| **Static keys** | *Planned:* `StaticKeyService` issues long-lived keys. Use them as bearer tokens with the prefix the real API documents |
| **Anonymous** | No `authorization` header. Allowed only when `NEBIUS_MOCKER_AUTHN=optional` (the default in `dev` profile, below) |

### Token format

Mocker-issued access tokens are opaque strings of the form `nmock.v1.<base64url(claims JSON)>.<base64url(HMAC-SHA256)>`.
The claims carry the token ID (`jti`), the principal (`sub`), the tenant (`tid`), the auth public key the token was
exchanged with (`kid`), the impersonating actor (`act`), and `iat`/`exp`. The HMAC with `NEBIUS_MOCKER_TOKEN_SECRET`
makes them verifiable statelessly, even across split containers that share the secret. By default the secret is
random per process.

The pieces live in `pkg/auth` (transport-free):
- `auth.Issuer` mints and verifies tokens and keeps the revocation deny-list.
- `auth.Authenticator` resolves a bearer token to a principal: a static dev token, or an issued token that verifies.
  `interceptors.Authenticate` and the registry's docker front use it.
- `auth.ParseAssertion`/`VerifyAssertion` check service-account JWTs.
- `auth.ParsePublicKey` reads uploaded keys and computes their fingerprint.

Revocation uses a deny-list in process memory. Deactivating or deleting an auth public key revokes the tokens
exchanged with it, and deleting a service account revokes all of its tokens. *Planned:* revocation through
`SessionManagementService` and an admin API.

Expiry defaults to 12h (`NEBIUS_MOCKER_TOKEN_TTL`; `expires_in` is returned) and is measured on the mock clock.
Advancing the clock tests client token refresh. Expired, revoked and forged tokens fail with `UNAUTHENTICATED`.

## Authorization

### Model

1. Each RPC maps to a **permission** string `<service>.<resource-plural>.<verb>`, for example `compute.instances.create`,
   `vpc.subnets.get` or `iam.accessPermits.create`. Modules declare these in `Kind.Permissions`. Non-CRUD verbs are
   explicit, for example `compute.instances.start`.
2. Each **role** is a set of permission globs. Seeded roles:

   | Role | Grants |
   |---|---|
   | `viewer` | `*.*.get`, `*.*.list`, `*.*.getByName` (excluding secret-revealing RPCs) |
   | `editor` | viewer + `*.*.create/update/delete` + custom verbs, **excluding** `iam.accessPermits.*` and `iam.groupMemberships.*` |
   | `admin` | `*` |
   | `mysterybox.payload.viewer`, `kms.crypto.user`, … | Narrow roles for data-plane RPCs |

   The role catalog is data (`authz/roles.yaml`), so it can follow Nebius's published roles as parity tests discover them.
   **The public protos carry role names only as strings (`AccessPermitSpec.role`), so this catalog is our model, not a copied one.**
3. **Bindings** are `AccessPermit{parent=group, resource_id, role}`. A principal's effective groups come from
   `GroupMembership`.
4. **Decision**: allow if some permit on the target resource **or any ancestor** (resource → project → tenant) grants a
   role whose permissions match. To find the ancestors, the authz engine walks `parent_id` links in the store, or calls
   `peer.Directory` for resources owned by a remote module.
5. **Target resolution** for each RPC kind:
   - `Create` → `metadata.parent_id`
   - `Get/Update/Delete/custom` → the resource `id` (looked up to find the ancestry; a missing resource → `NOT_FOUND`)
   - `GetByName/List` → `parent_id`
   - `OperationService.Get` → the operation's resource

### Enforcement modes

`NEBIUS_MOCKER_AUTHZ`:

| Mode | Behavior | Use |
|---|---|---|
| `off` | Every authenticated (or anonymous, if allowed) request passes | Quick local hacking |
| `audit` (default) | Decisions are evaluated and logged with the missing permission, **but not enforced** | Find out which permits your Terraform needs before you enforce them |
| `enforce` | Denials return `PERMISSION_DENIED` + `PermissionDenied{resource_id}` | CI that verifies least-privilege setups |

Audit-mode findings are also served at `GET /_mocker/authz/denials`. Developers can turn that output directly into
`nebius_iam_v1_access_permit` resources.

### Visibility

As in the real cloud, `Get` on a resource the caller cannot see returns `NOT_FOUND`, not `PERMISSION_DENIED`, so that
existence does not leak. `List` filters out invisible items. This applies only in `enforce` mode.

## Profiles (`NEBIUS_MOCKER_PROFILE`)

| Profile | Authn | Authz | Default token | Timing |
|---|---|---|---|---|
| `dev` (default) | optional | audit | `nmock-dev-admin` enabled | fast |
| `ci` | required | enforce | `nmock-dev-admin` enabled | instant |
| `strict` | required | enforce | none; only exchanged or minted tokens | realistic |

Any individual setting can override the profile.
