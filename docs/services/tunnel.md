# Application Tunnel

> Module: `services/tunnel` · Image: `nebius-mocker-tunnel` · `NEBIUS_MOCKER_SERVICES=tunnel`
> M1 tier: **T1** (inventory only) · Endpoint: `applicationtunnel.mkt.api.nebius.cloud`

## Overview

[Nebius Tunnels](https://docs.nebius.com/tunnels/overview) expose a service on a VM, a laptop or any host behind a
firewall at `https://<service>-<masked_id>.tunnel.applications.<region>.nebius.cloud`. Nothing needs inbound ports.
Three pieces make it work:

- the **tunnel** resource (`applicationtunnel-…`, parent: a project; `spec.title`, `spec.description`);
- the **tunnel agent**, Nebius's own binary (`cr.eu-north1.nebius.cloud/marketplace/docker/nebius-tunnel-agent`, a Helm
  chart, or a VM tarball). It signs in as a service account that holds `applicationtunnel.agent` on the tunnel, and
  dials a regional Nebius edge over TLS;
- the **services** in the agent's config (`name`, `target`, `type: http|tcp`). The agent announces them to the edge,
  and they show up in `status.services` with their public `endpoint`.

Tunnels add no user authentication: the edge only secures transport.

## Implementation status

| Piece | Status |
|---|---|
| `nebius.tunnel.v1.TunnelService`: Create, Get, List (paged), Update (full replace with `X-ResetMask`), Delete | ✅ T1, **inventory only** |
| `status`: `state` `CREATED`, `connection_state` `CONNECTION_STATE_DISCONNECTED`, no `services` | ✅ (always, see "Not yet") |
| Role `applicationtunnel.agent` (grants `applicationtunnel.tunnels.get`), grantable with access permits on a tunnel | ✅ |
| Authorization: `applicationtunnel.tunnels.*` for every RPC | ✅ |
| `nebius_tunnel_v1_tunnel` resource and data source with the official provider | ✅ e2e: `test/terraform/tunnel` |
| `nebius tunnel` CLI flows (create, get, list, update, delete; the agent's IAM setup) | ✅ e2e: `test/cli/tunnel` |
| Tunnel data plane: agent connection, edge, public endpoints, `nebius tunnel connect` / `ssh-config` | Not yet (see below) |
| `List` `filter` | Rejected with `INVALID_ARGUMENT` |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312,
nebius-tunnel-agent 1.2.0.

Covered by unit tests (`services/tunnel/{domain,dal,api}`), gosdk contract tests (`test/contract/tunnel_*`: CRUD,
reset mask, error kinds, per-role authorization, the agent role), the Terraform e2e suite (`test/terraform/tunnel`) and
the CLI e2e suite (`test/cli/tunnel`). `make e2e` runs the last two.

### Behavior notes

These follow the protos. Where they don't say what happens, the mock's choice is marked *(mock)*.

- **Unnamed.** `Tunnel` has `resource_behavior = UNNAMED`: `metadata.name` is stored but needn't be unique, and there
  is no `GetByName`.
- **Create** needs `metadata.parent_id` to be an existing project. A missing project is `NOT_FOUND`, a non-project ID
  is `INVALID_ARGUMENT`. `metadata.id` and `resource_version` must not be set. `spec` is optional. `title` (≤ 256) and
  `description` (≤ 1024) are checked by `buf.validate`. The operation completes at once *(mock)*.
- **Update** replaces `metadata.name`, `labels`, `spec.title` and `spec.description`. A field sent empty is cleared
  only when `X-ResetMask` names it, which is how Terraform removes an attribute. `id`, `parent_id`, `created_at`,
  `resource_version` and `status` are server-owned and survive any reset mask. A different `parent_id` is
  `INVALID_ARGUMENT`, and Terraform replaces the resource on that change (`RequiresReplace`). A `resource_version` that
  doesn't match is `FAILED_PRECONDITION`. An update that changes nothing keeps `resource_version` and `updated_at`.
- **Delete** removes the tunnel at once. There is no lingering `DELETED` state *(mock)*.
- **`List`** takes `parent_id` and pages in creation order. A `filter` is `INVALID_ARGUMENT` ("filtering is not
  implemented in NebiusMocker yet"), not silently ignored.
- **Status** is always `CREATED` / `CONNECTION_STATE_DISCONNECTED` with no services. This is the true state of a
  tunnel no agent has connected to, and on the mock no agent can connect (see "Not yet").

### Authorization

The permissions are `applicationtunnel.tunnels.{get,list,create,update,delete}`. They are checked on the tunnel, or on
the project for Create and List. `viewer` reads, `editor` and `admin` write. `applicationtunnel.agent` grants
`applicationtunnel.tunnels.get` only, on the resource it is bound to and its descendants. That supports the documented
agent setup: a service account in a group, with an access permit for that group on the tunnel. Such an account can read
its tunnel, but can't modify it or list the project. Reading another tunnel answers `NOT_FOUND`, because a denied read
doesn't reveal that the resource exists.

### Not yet

- **The tunnel data plane: agent, edge, public endpoints, `status.services` and `connection_state`.**
  - The only agent is Nebius's proprietary `nebius-tunnel-agent`.
  - It reaches the regional edge over an **undocumented** HTTP/2 registration and announce protocol, and its config has
    no documented way to point it at another edge.
  - Supporting it would mean reverse-engineering that protocol or shipping a mock-specific agent. Both break "official
    tools only".
  - So nothing can connect. Tunnels stay `DISCONNECTED`, `status.services` stays empty, and no
    `*.tunnel.applications.*` endpoint is served. `nebius tunnel connect` and `ssh-config` (plain TLS with SNI to such
    an endpoint) have nothing to reach.
  - This changes when Nebius documents the agent↔edge protocol, or the agent gets a documented, configurable edge
    address: then the edge becomes a local listener, and `status` follows the connected agents.
- `List` `filter`.
- Quotas (owned by the quotas work).

## Terraform

`test/terraform/tunnel` runs the official provider under `enforce`. The steps are create, an empty plan, an in-place
update, another empty plan, removing `description` (cleared through `X-ResetMask`), an empty plan, replacement on
`parent_id`, the data source, and destroy. The Go SDK checks the mock's state after every step.

| Kind | Terraform type | Service | RPCs called | Tested |
|---|---|---|---|---|
| Resource | `nebius_tunnel_v1_tunnel` | `nebius.tunnel.v1.TunnelService` | Get, Create, Update, Delete + OperationService.Get | ✅ |
| Data source | `nebius_tunnel_v1_tunnel` | `nebius.tunnel.v1.TunnelService` | Get | ✅ |

## CLI

`test/cli/tunnel` runs `nebius tunnel create`, `get`, `list`, `update` and `delete`. It then runs the agent's IAM setup
from [Connect to a tunnel](https://docs.nebius.com/tunnels/connect), all with the CLI:

1. `nebius iam service-account create`, `group create`, `access-permit create --role applicationtunnel.agent` on the
   tunnel, `group-membership create`, and `auth-public-key create`.
2. As that service account, `nebius tunnel get` on its tunnel works, while `update`, `delete` and `list` are
   `PERMISSION_DENIED`, and `get` of another tunnel is `NOT_FOUND`.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.tunnel.v1.TunnelService`

Endpoint: `applicationtunnel.mkt.api.nebius.cloud:443` · Proto: [`nebius/tunnel/v1/tunnel_service.proto`](https://github.com/nebius/api/blob/main/nebius/tunnel/v1/tunnel_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetTunnelRequest` | `Tunnel` | ✅ |
| `List` | `ListTunnelRequest` | `ListTunnelsResponse` |  |
| `Create` | `CreateTunnelRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateTunnelRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteTunnelRequest` | `common.v1.Operation` | ✅ |
