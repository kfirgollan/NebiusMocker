# DNS

> Module: `services/dns` · Image: `nebius-mocker-dns` · `NEBIUS_MOCKER_SERVICES=dns`
> M1 tier: **T1** · Endpoint: `dns.api.nebius.cloud`

## Overview

DNS **zones** (public, or private and bound to VPC networks; `ZoneService.ListByNetwork`) and **records**
(`relative_name`, `type`, `data`, `ttl`, `deletion_protection`; `status.effective_fqdn`).

| Resource kind | Message | NID prefix | Parent |
|---|---|---|---|
| Zone | `Zone` | `dnszone` | project |
| Record | `Record` | `dnsrecord` | zone |

## Emulation design

- Zone `domain_name` is validated as an FQDN. Private zones reference networks (via `peer` → vpc). `status.zone_domain_name`
  is normalized with a trailing dot.
- Records: type-specific `data` validation (A/AAAA addresses, CNAME targets, MX priority, TXT quoting, SRV). The
  `CNAME`-exclusivity rule applies. `effective_fqdn = relative_name + "." + zone`, where `@` means the apex.
  `deletion_protection` blocks Delete. Deleting a zone with records needs `force` (from `DeleteRecordRequest.force` /
  the zone equivalent).
- **Later (T2)**: an optional authoritative DNS responder (`miekg/dns`) on `:5353/udp`, serving the mock zones, so that
  workloads in the k3s backend can resolve them.
- Permissions: `dns.zones.*`, `dns.records.*`. Quotas: `dns.zone.count`, `dns.record.count` (zone).

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_dns_v1_record` | `nebius.dns.v1.RecordService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 |
| Resource | `nebius_dns_v1_zone` | `nebius.dns.v1.ZoneService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 |
| Data source | `nebius_dns_v1_record` | `nebius.dns.v1.RecordService` | Get, GetByName | T1 |
| Data source | `nebius_dns_v1_zone` | `nebius.dns.v1.ZoneService` | Get, GetByName | T1 |

## Open questions
- Supported record types, and the TTL range.
- The zone-delete behavior when records exist.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.dns.v1.RecordService`

Endpoint: `dns.api.nebius.cloud:443` · Proto: [`nebius/dns/v1/record_service.proto`](https://github.com/nebius/api/blob/main/nebius/dns/v1/record_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetRecordRequest` | `Record` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Record` | ✅ |
| `List` | `ListRecordsRequest` | `ListRecordsResponse` |  |
| `Create` | `CreateRecordRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateRecordRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteRecordRequest` | `common.v1.Operation` | ✅ |

#### `nebius.dns.v1.ZoneService`

Endpoint: `dns.api.nebius.cloud:443` · Proto: [`nebius/dns/v1/zone_service.proto`](https://github.com/nebius/api/blob/main/nebius/dns/v1/zone_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetZoneRequest` | `Zone` | ✅ |
| `GetByName` | `common.v1.GetByNameRequest` | `Zone` | ✅ |
| `List` | `ListZonesRequest` | `ListZonesResponse` |  |
| `ListByNetwork` | `ListZonesByNetworkRequest` | `ListZonesResponse` |  |
| `Create` | `CreateZoneRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateZoneRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteZoneRequest` | `common.v1.Operation` | ✅ |
