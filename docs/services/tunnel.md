# Application Tunnel

> Module: `services/tunnel` · Image: `nebius-mocker-tunnel` · `NEBIUS_MOCKER_SERVICES=tunnel`
> M1 tier: **T1** · Endpoint: `applicationtunnel.mkt.api.nebius.cloud`

## Overview

Tunnels (`applicationtunnel`) expose services from a private environment through Nebius-managed connectivity.
`spec` has `title` and `description`. `status` has `state`, the exposed `services` and `connection_state`.

## Emulation design

- T1 CRUD. `connection_state` reports "disconnected" until an agent connects. An admin API endpoint
  (`POST /_mocker/tunnel/<id>/connect`) simulates an agent connection and populates `status.services`, so that tests can
  cover both states.
- The provider calls Create/Get/Update/Delete (no `GetByName`).

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_tunnel_v1_tunnel` | `nebius.tunnel.v1.TunnelService` | Get, Create, Update, Delete + OperationService.Get | T1 |
| Data source | `nebius_tunnel_v1_tunnel` | `nebius.tunnel.v1.TunnelService` | Get | T1 |

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
