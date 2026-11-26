# DNS

> Module: `services/dns` · Image: `nebius-mocker-dns` · `NEBIUS_MOCKER_SERVICES=dns`
> M1 tier: **T2**, plus a T3 data plane (a DNS responder) · Endpoint: `dns.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| `nebius.dns.v1.ZoneService`: Create, Get (`resource_version`), GetByName, List (paged), ListByNetwork (paged), Update (reset mask, `resource_version`), Delete (`resource_version`) | ✅ T2 |
| `nebius.dns.v1.RecordService`: Create, Get (`resource_version`), GetByName, List (paged), Update (reset mask, `resource_version`), Delete (`resource_version`, `force`) | ✅ T2 |
| Zones bound to a VPC network through `peer.Networks` (vpc): the network must exist in the zone's project, and cannot be deleted while a zone is bound | ✅ |
| Service-managed `SOA @` and `NS @` records, `status.record_count`, `status.effective_fqdn` / `zone_domain_name` | ✅ |
| Per-type record data validation, duplicates, per-zone and per-name limits, deletion protection, reserved domain names | ✅ |
| DNS responder (`miekg/dns`, UDP and TCP on `NEBIUS_MOCKER_DNS_LISTEN`, default `:4653`): authoritative answers, wildcards, NXDOMAIN/NODATA with the SOA | ✅ T3 |
| Authorization: rules for every RPC (`dns.zones.*`, `dns.records.*`); `auditor` reads zones but not records | ✅ |
| `nebius_dns_v1_zone`, `nebius_dns_v1_record` resources and data sources with the official provider | ✅ e2e: `test/terraform/dns` |
| `nebius dns zone` / `nebius dns record` CLI flows from the docs, verified with `dig` | ✅ e2e: `test/cli/dns` |
| System-managed `<network_ID>.compute.internal.` zones with VM FQDNs | Not yet |
| Quota on zones (100 per region) | Not yet |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100, nebius CLI 0.12.312.

Covered by unit tests (`services/dns/{domain,dal,responder}`), gosdk contract tests with real DNS queries
(`test/contract/dns_*`), the Terraform e2e suite and the CLI e2e suite (`make e2e`).

### Behavior notes

These follow [docs.nebius.com/vpc/dns](https://docs.nebius.com/vpc/dns/overview) and the protos. Where neither says
what happens, the mock's choice is marked *(mock)*.

- **Zones are private.** The API has one scope, `spec.vpc`: every zone is attached to exactly one network, and there
  are no public zones. `spec.vpc.primary_network_id` is required. The network must exist (`NOT_FOUND`) and be in the
  zone's project (`INVALID_ARGUMENT`) *(mock: the docs do not mention cross-project networks)*. `domain_name` and the
  network are immutable: sending the stored value again (as Terraform does) is accepted, a different one is
  `INVALID_ARGUMENT`. Terraform replaces the zone instead.
- **Domain names.** A zone needs at least two labels (`example.` is `INVALID_ARGUMENT`). Names Nebius reserves are
  `PERMISSION_DENIED`: `ns.internal.`, `compute.internal.`, `nebius.internal.`, `console.nebius.`, `example.invalid.`
  and everything under them (the docs list these as examples; other names under `internal.` are allowed). A network
  cannot have two zones with the same domain name (`ALREADY_EXISTS`). Nested zones are allowed, and the longest one
  answers.
- **Service-managed records.** Creating a zone creates `SOA @` and `NS @`, without a resource name, so
  `status.record_count` is 2 right away (the docs show the same). They cannot be created, updated or deleted
  (`INVALID_ARGUMENT` on create, `FAILED_PRECONDITION` otherwise), and they go away with the zone. Their data is the
  mock's: `NS` is `ns.internal.`, `SOA` is `ns.internal. hostmaster.nebius.internal. 1 3600 600 604800 <negative TTL>`,
  both with TTL 3600 *(mock)*. The last SOA field follows `spec.soa_spec.negative_ttl`, or 900 when it is unset or below 5.
- **Zone delete** fails with `FAILED_PRECONDITION` while the zone has any record other than its SOA and NS (docs:
  "first delete all records except the default `NS @` and `SOA @` records"). `DeleteZoneRequest` has no `force`.
- **Network delete.** A VPC network with a bound zone cannot be deleted (`FAILED_PRECONDITION`, "network is in use by
  dnszone-…"), like a network with subnets *(mock: the docs say the network cannot change, not what deleting it does)*.
  The binding and the delete check run under the VPC lock, so they cannot race.
- **Record types.** Users can create `A`, `MX`, `TXT`, `SRV`, `CAA`, `SVCB` and `HTTPS` (the docs' list). `CNAME` and
  `AAAA` ("not supported in custom DNS zones"), `NS`, `SOA` and `PTR` (not in the docs' list) are `INVALID_ARGUMENT`
  with a message that names the type. With no CNAME there is no CNAME-exclusivity rule to enforce.
- **Record data** is the zone-file RDATA, parsed with `miekg/dns`. Domain names in it must be fully qualified (`mail`
  is rejected, `mail.my.example.` is accepted), the `.` shorthand is rejected, TXT data must be quoted strings of 1–255
  characters, CAA tags are `issue`, `issuewild` or `iodef`, and a `;` outside quotes (a zone-file comment) is rejected.
  The data is stored as sent. Two records with the same relative name, type and data (compared in canonical form) are
  `ALREADY_EXISTS`, as the docs say.
- **Limits** (docs constraints): 1000 records per zone and 20 per relative name and type, both `FAILED_PRECONDITION`.
  Names are limited by the proto patterns (252 characters) and by the 253-character DNS name limit.
- **TTL.** `spec.ttl` is stored as sent; 0 (absent) or negative means 600 when served (record.proto). Keeping 0 in the API
  avoids a Terraform diff, since `ttl` is optional and not computed in the provider.
- **Deletion protection.** Deleting a protected record without `force` is `FAILED_PRECONDITION`, as the docs say.
- **Versions.** `resource_version` on Get, Update or Delete that does not match is `FAILED_PRECONDITION` (the protos say
  so). An update that changes nothing keeps the version and `updated_at`.
- **Operations** complete at once (or after `NEBIUS_MOCKER_OP_DELAY`); `status.reconciling` is always false.

### DNS responder

The mock runs an authoritative DNS server for its zones, so names created through the API resolve with ordinary
clients. On Nebius, VMs query the network resolver at `169.254.169.2`; locally, query the mock instead:

```bash
dig @127.0.0.1 -p 4653 www.my.example.
```

- It answers for names under the mock's zones (authoritative, `AA` set): exact matches, `ANY`, wildcards (`*` as the
  leftmost label, synthesized at the closest encloser per RFC 4592), NXDOMAIN and NODATA with the zone's SOA in the
  authority section (TTL `min(3600, negative TTL)`). It truncates UDP answers that do not fit (use TCP or EDNS0).
- It does not recurse: a name outside every zone gets `REFUSED`. On Nebius, the network resolver forwards those
  upstream.
- It is not inside a network, so it serves the zones of **every** network. When zones of several networks share a
  domain name, the oldest zone answers *(mock)*.
- `NEBIUS_MOCKER_DNS_LISTEN` sets the address (UDP and TCP on the same port); `off` disables it.

## Overview

DNS **zones** (private, bound to a VPC network; `ZoneService.ListByNetwork`) and **records**
(`relative_name`, `type`, `data`, `ttl`, `deletion_protection`; `status.effective_fqdn`).

| Resource kind | Message | NID prefix | Parent |
|---|---|---|---|
| Zone | `Zone` | `dnszone` | project |
| Record | `Record` | `dnsrecord` | zone |

## Emulation design

- `services/dns/domain` holds the rules above. One mutex serializes DNS writes, so duplicate, limit and "zone has
  records" checks are atomic with the writes they guard. `services/dns/dal` stores zones and records in `pkg/store`.
- Networks come from the vpc module through `pkg/peer.Networks` (`Network`, `Bind`, `Unbind`); vpc records bindings
  and refuses network deletion while any exist. Without the vpc module, zone creation fails with `INVALID_ARGUMENT`.
- `services/dns/responder` is the data plane, a `github.com/miekg/dns` server over the domain's `FindZone`.
- Permissions: `dns.zones.*`, `dns.records.*` (`ListByNetwork` needs `dns.zones.list` on the network). Records are
  parented to their zone, so record permissions inherit from the zone's project.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NEBIUS_MOCKER_DNS_LISTEN` | `:4653` | UDP and TCP address of the DNS responder; `off` disables it |

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_dns_v1_record` | `nebius.dns.v1.RecordService` | Get, GetByName, Create, Update, Delete + OperationService.Get | ✅ T2 |
| Resource | `nebius_dns_v1_zone` | `nebius.dns.v1.ZoneService` | Get, GetByName, Create, Update, Delete + OperationService.Get | ✅ T2 |
| Data source | `nebius_dns_v1_record` | `nebius.dns.v1.RecordService` | Get, GetByName | ✅ T2 |
| Data source | `nebius_dns_v1_zone` | `nebius.dns.v1.ZoneService` | Get, GetByName | ✅ T2 |

Immutable (`RequiresReplace`) attributes: zone `domain_name`, `vpc.primary_network_id`, `parent_id`; record
`relative_name`, `type`, `parent_id`.

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
