# VPC — Virtual Private Cloud

> Module: `services/vpc` · Image: `nebius-mocker-vpc` · `NEBIUS_MOCKER_SERVICES=vpc`
> M1 tier: **T2** · Endpoint: `vpc.api.nebius.cloud`

## Implementation status

| Piece | Status |
|---|---|
| Seeded per project: `default-network-pool` (private, `10.0.0.0/13` = eu-north1), `default-public-pool` (public, `203.0.113.0/24`), `default-network` (`status.default`) and `default-subnet` (uses the network's pools) | ✅ |
| Every network gets `default-route-table-<suffix>` (route `default-egress`: `0.0.0.0/0` → default egress gateway) and `default-security-group-<suffix>`; both are removed with the network | ✅ |
| `PoolService`: Get, GetByName, List, ListAggregated, ListBySourcePool, Create, Update, Delete; child pools carved from `source_pool_id` (`"/16"` or explicit), `status.cidrs`, live `status.assignment` | ✅ T2 |
| `NetworkService`: Get, GetByName, GetDefault, List, ListAggregated, Create, CreateDefault, Update, Delete; default pools filled in when omitted | ✅ T2 |
| `SubnetService`: Get, GetByName, List, ListAggregated, ListByNetwork, Create, Update, Delete; `use_network_pools` (shared) or explicit blocks (`"/24"` lowest free, or a free CIDR inside the network's pools), `route_table_id` | ✅ T2 |
| `AllocationService`: Get, GetByName, List, ListAggregated, ListByPool, ListBySubnet, Create, Update, Delete; private and public, lowest free address (skips network, gateway and broadcast) or an explicit free one; Update per upstream (only `subnet_id` → own `pool_id`; moves refused) | ✅ T2 |
| `RouteTableService` (+ ListByNetwork, live `status.assignment`), `RouteService` (next hop: allocation in the same network, or default egress for `0.0.0.0/0`; unique destination per table) | ✅ T2 |
| `SecurityGroupService` (+ ListByNetwork), `SecurityRuleService` (stored and validated, not enforced; priority 500 and `STATEFUL` defaults; `status.effective_priority`, direction, match) | ✅ T2 |
| Delete refused while referenced (table below); cascades: route table → routes, security group → rules, network → default route table and security group | ✅ |
| Authorization: `vpc.<resources>.<verb>` for every RPC, ancestry through project / route table / security group | ✅ |
| All `nebius_vpc_v1_*` resources and data sources (incl. `nebius_vpc_v1_default_network`) with the official Terraform provider | ✅ e2e: `test/terraform/vpc` |
| `nebius vpc network\|subnet list\|get-by-name` with the official CLI | ✅ e2e: `test/terraform/vpc/cli_test.go` |
| `status.assignment` of allocations: set by compute for instance network interfaces through `peer.NetworkInterfaces` (dynamic and user allocations, `PRIMARY`/`PUBLIC`/`ALIAS`) | ✅ unit tests (`services/vpc/domain/nic_test.go`), compute contract and Terraform suites |
| `TargetGroupService` (needs compute instances), load balancer assignments, quotas | Not yet |
| `vpc.v1alpha1` (second view of the same resources, see [v1alpha1](#v1alpha1-a-second-view-of-the-same-resources)): `AllocationService` Get, GetByName, List, Create, Update, Delete; `PoolService`, `SubnetService` (+ ListByNetwork), `NetworkService` (+ GetDefault) Get, GetByName, List; `ScopeService` Get, GetByName, List; `nebius.common.v1alpha1.OperationService` Get, List | ✅ contract: `test/contract/vpc_v1alpha1_*` |
| `nebius_vpc_v1alpha1_allocation` (resource and data source) and the `network`, `pool`, `scope`, `subnet` data sources with the official Terraform provider | ✅ e2e: `test/terraform/vpc/v1alpha1_*` |
| `nebius vpc v1alpha1 allocation\|network\|pool\|scope\|subnet` with the official CLI | ✅ e2e: `test/terraform/vpc/v1alpha1_*` |
| `filter` on v1alpha1 `List*` requests (upstream format "TBD"): refused with `InvalidArgument` naming `filter` | Not yet |
| v1alpha1 `SubnetSpec.enable_egress_nat`: v1 has no such setting; always reported `false` | Not yet (read-only view) |

Verified against: nebius/api@4522189, terraform-provider-nebius v0.6.91, gosdk v0.2.100.

Covered by unit tests (`services/vpc/domain` incl. the IPAM primitives, `services/vpc/dal`), gosdk contract tests
(`test/contract/vpc_*`) and the Terraform + CLI e2e suite (`make e2e`).

## Overview

VPC models IP address management and private networking: **pools** (CIDR sets), **networks** that draw from pools,
**subnets** inside networks, **allocations** (private or public IP addresses), **security groups and rules**,
**route tables and routes**, and **target groups** (load-balancer backends). `v1alpha1` adds **scopes** and older
network/subnet/pool shapes over the same resources, which the provider exposes as data sources plus
`nebius_vpc_v1alpha1_allocation`.

| Resource kind | Message | NID prefix | Parent | States |
|---|---|---|---|---|
| Pool | `Pool` | `vpcpool` | project | `spec.cidrs[]`, `source_pool_id*`, `version*` (IPv4), `visibility*` (PRIVATE/PUBLIC) |
| Network | `Network` | `vpcnetwork` | project | `CREATING, READY, DELETING` |
| Subnet | `Subnet` | `vpcsubnet` | project | `CREATING, READY, DELETING`; `spec.network_id`, pools, `route_table_id` |
| Allocation | `Allocation` | `vpcallocation` | project | `ipv4_private{subnet_id|pool_id, cidr}` or `ipv4_public{…}`; `status.assignment` (who uses it), `static` |
| Security group | `SecurityGroup` | `vpcsecuritygroup` | project | references network |
| Security rule | `SecurityRule` | `vpcsecurityrule` | security group | `access*, priority*, protocol*, ingress*/egress*` all immutable |
| Route table | `RouteTable` | `vpcroutetable` | project (network) | |
| Route | `Route` | `vpcroute` | route table | |
| Target group | `TargetGroup` | `vpctargetgroup` | project | |
| Scope (v1alpha1, read-only) | `Scope` | `vpcscope` | project | `READY`; `spec.type` PRIVATE/PUBLIC; two per project, system-managed |

## Emulation design

VPC is pure control plane: no backend is adopted, and there is no packet path. IPAM is implemented with `net/netip`
in `services/vpc/domain` (`ipam.go`, `space.go`). One lock serializes every VPC mutation, so placement decisions are
atomic.

### IPAM core
Outputs must be deterministic and must survive an empty `terraform plan`, so **specs are stored as written** (a
`"/24"` stays `"/24"`) and resolved blocks live in `status`.

- **Pools.** Each project gets `default-network-pool` (private, `NEBIUS_MOCKER_VPC_DEFAULT_PRIVATE_CIDR`, default
  `10.0.0.0/13`, the documented eu-north1 block) and `default-public-pool` (public, `NEBIUS_MOCKER_VPC_PUBLIC_CIDR`,
  default `203.0.113.0/24` from TEST-NET-3, so mock IPs never collide with real ones). The public pool cannot be
  changed or deleted, as documented. A pool with `source_pool_id` carves its CIDRs (explicit, or `"/N"` for the
  lowest free block) from the source pool, which must have the same version and visibility. A root pool (no source)
  needs explicit CIDRs. `status.cidrs` holds the resolution in spec order. Only IPv4 is supported.
- **Block settings.** Every pool or subnet CIDR entry has `state` (default `AVAILABLE`) and `max_mask_length`
  (default 32). Nothing is placed in a `DISABLED` block, and nothing longer than `max_mask_length` is carved from a
  block (for example a /32 allocation from a block limited to /28 is refused with `INVALID_ARGUMENT`).
- **What a pool's space is used by.** Child pools, subnets with explicit blocks, and allocations. Subnets with
  `use_network_pools` share the network's pools and consume nothing, so many of them can coexist ("all subnets in
  that network can use those pools at the same time").
- **Networks.** `spec.ipv4_private_pools` / `ipv4_public_pools` reference pools of the right visibility in the same
  project; omitted lists are filled with the project's default pools. A network's private pools must not overlap.
  Removing a pool still used by the network's subnets or allocations is refused. `GetDefault` returns the network
  with `status.default` and its default subnet; `CreateDefault` creates them if the project has none.
- **Subnets.** Each family (private, public) either uses the network's pools (the default when no pools are given) or
  lists `pools[].cidrs[]`. Each `cidr` is a block (`10.1.2.0/24`), which must be inside the network's pools and free,
  or a bare prefix length (`/24`), for which the lowest free aligned block is chosen. Overlap → `INVALID_ARGUMENT`;
  no room left → `FAILED_PRECONDITION`. Updates keep blocks already held (a `"/24"` does not move). Status:
  `ipv4_private_pools[] {pool_id, cidrs}` (plus the deprecated flat `ipv4_private_cidrs`, in spec order) and
  `route_table {id, default}`.
- **Allocations.** From a subnet (its explicit blocks, or the network's pools for a shared family) or directly from a
  pool. Without `cidr` the lowest free address is used (skipping network, gateway `.1` and broadcast). `cidr` may be
  an address, a CIDR or `/N`. `status.details` records `allocated_cidr`, `pool_id` and `subnet_id`; `static` is
  `true`. `status.assignment` is set by compute (see below); an assigned allocation cannot be deleted.
- **Instance network interfaces** (`peer.NetworkInterfaces`, provided to compute, `services/vpc/domain/nic.go`). Every
  address of an instance NIC is an allocation with `status.assignment.network_interface{instance_id, name, type}`
  (`PRIMARY`, `PUBLIC`, `ALIAS`), state `ASSIGNED` and `details.subnet_id` = the NIC's subnet. A NIC without an
  `allocation_id` gets a dynamic allocation (lowest free address of the subnet; public ones from the subnet's public
  space, or the project's `default-public-pool`), named `<instance>-<nic>-private|public`, with `static = false`: it is
  deleted when the NIC releases it. A user allocation must be in the project, have the right visibility, lie in (or be
  associated with) the subnet and not be assigned elsewhere (`FAILED_PRECONDITION`); on release it is unassigned
  (`ALLOCATED`) and survives. Binding is idempotent and atomic (under the VPC lock); security groups must belong to the
  subnet's network, and an empty list resolves to the network's default group. A subnet with assigned allocations
  cannot be deleted; AllocationService.Update keeps the assignment.
- **Allocation updates** follow `allocation.proto`. `cidr` is `IMMUTABLE`, and the provider plans a replacement when
  it changes. The `pool` oneof "may be changed only from subnet_id to pool_id, and the new pool_id must be equal to
  status.details.pool_id". The provider plans every `subnet_id`/`pool_id` change and a private↔public switch as an
  in-place update (only `cidr` is `RequiresReplace` in provider 0.6.91), so the API is what decides:

  | Change | Result |
  |---|---|
  | `subnet_id` → `pool_id` = `status.details.pool_id` (also: naming the pool a public allocation was placed in by default) | Accepted. The address does not move; `status.details.subnet_id` is cleared ("populated when created with explicit subnet_id, from a subnet-specific pool, or when assigned"). The allocation stays listed by `ListBySubnet` while its address is in the subnet, no longer blocks deleting the subnet, and keeps its address when the subnet goes. |
  | `subnet_id` → another subnet | `INVALID_ARGUMENT` on `spec.ipv4_*.subnet_id` |
  | `subnet_id` → a pool other than `status.details.pool_id` | `INVALID_ARGUMENT` on `spec.ipv4_*.pool_id` |
  | `pool_id` → another pool, or back to `subnet_id` | `INVALID_ARGUMENT` on `spec.ipv4_*.pool_id` / `subnet_id` |
  | `ipv4_private` ↔ `ipv4_public` | `INVALID_ARGUMENT` on `spec` (needs another address and pool, which both rules forbid) |
  | `cidr` | `INVALID_ARGUMENT` on `spec.ipv4_*.cidr`; Terraform replaces instead |

  A refused update changes nothing (not even `resource_version`). The messages name the upstream rule, e.g.
  `spec.ipv4_private.subnet_id: may be changed only from subnet_id to pool_id <pool> (status.details.pool_id); an
  allocation cannot move to another subnet`. Nebius's exact wording is not documented, so the code and field (`InvalidArgument`
  with a field violation) follow the same convention as every other validation error in the mock.

### Routing
Every network gets a default route table (`status.default`, referenced by `network.status.default_route_table_id`)
with one route, `0.0.0.0/0` → default egress gateway. Subnets without `route_table_id` use it. A custom route table
can only be assigned to subnets of its network. Route destinations are unique per table. A next hop is an allocation
in a subnet of the same network (or, with no subnet, from one of the network's private pools), or `default_egress_gateway = true`, which is valid only for `0.0.0.0/0`.
`destination` and `next_hop` are immutable; `description` is updatable.

### Security groups and rules
Stored and validated, not enforced. Every network gets a default security group. Rule spec fields are all
`IMMUTABLE`, so Terraform plans a replacement on any change. Priority defaults to 500 and type to `STATEFUL`;
several rules may share a priority (DENY is evaluated first), which `status.effective_priority` reflects
(`2·priority`, `+1` for ALLOW). A referenced security group must be in the same network; ports are rejected for ICMP.

### Integrity
| Delete | Refused while |
|---|---|
| Pool | Networks reference it, child pools exist, subnets carve from it, or allocations come from it; always for `default-public-pool` |
| Network | Subnets, non-default security groups or non-default route tables exist (the defaults are deleted with it) |
| Subnet | Allocations reference it (instance NICs and mk8s clusters: later, via `peer`) |
| Allocation | It is assigned, or a route uses it as next hop |
| Route table | Subnets use it; always for a default route table |
| Security group | Another group's rule references it; always for a default security group |

### Aggregated lists
`ListAggregated` currently returns the same as `List` (the mock has one project per tenant). `ListByNetwork`,
`ListBySubnet`, `ListByPool` and `ListBySourcePool` filter across all parents and page in creation order.
`ListBySubnet` returns the allocations associated with the subnet plus, as documented, "unassigned allocations from
compatible pools": those with no subnet whose address lies in the subnet's blocks (or, for a shared family, in the
network's pools).

### v1alpha1: a second view of the same resources
Real Nebius serves `vpc.v1` and `vpc.v1alpha1` over the same backing resources, so the mock has **one domain** and
two API views. A network, subnet, pool or allocation has one NID and one stored form (the v1 message); the
`services/vpc/api/v1alpha1_*.go` handlers only convert. So a network created through v1 is readable through
v1alpha1, an allocation created through v1alpha1 is readable, updatable and deletable through v1, and both draw from
the same IPAM under the same lock: addresses never collide across versions. Mapping:

| v1alpha1 | stored v1 |
|---|---|
| `Pool.spec.source_pool_id` / `source_scope_id` | `spec.source_pool_id`; a root pool shows `source_scope_id` = its `status.scope_id` |
| `PoolCidr{cidr, state, allowed_mask}` | `PoolCidr{cidr, state, max_mask_length}` (same enum numbers) |
| `Network.spec.pools`, `status.scope_id` | `spec.ipv4_private_pools.pools`; the private pools' scope. Public pools have no v1alpha1 field |
| `Subnet.spec.pools` | `use_network_pools` → `{pool_id}` per `status.ipv4_private_pools` entry; explicit blocks → `{spec: {version: IPV4, cidrs}}` |
| `Subnet.status.ipv4_cidrs` | the CIDRs of `status.ipv4_private_pools` |
| `Allocation.spec` (`ipv4_private{cidr, subnet_id\|pool_id}`, `ipv4_public{cidr, pool_id}`) | 1:1. `ipv4_public = {}` (no pool) takes an address from the project's `default-public-pool`, like v1, and the spec stays empty so Terraform sees no drift. A v1 public allocation made with `subnet_id` shows `ipv4_public` without it |
| `Allocation.status` | 1:1, except `details.subnet_id`, which v1alpha1 lacks |

v1alpha1 Create/Update/Delete return `nebius.common.v1alpha1.Operation`, served by the shared
`nebius.common.v1alpha1.OperationService` (`pkg/operations/api/v1alpha1.go`), which reads the same operations as the v1
service. Allocation updates follow the v1 rules (only name and labels in practice; the provider replaces on any spec
change, since every spec field is `RequiresReplace`).

**Scopes** exist only in v1alpha1 and the API cannot create or change them. A scope is the address space of a pool
tree (`pool.proto`: a root pool is "a top-level pool within scope", a child pool gets "the same scope" as its source).
Undocumented details are chosen conservatively: each project has exactly two system scopes, `default-private-scope`
(`PRIVATE`) and `default-public-scope` (`PUBLIC`), created the first time a root pool of that visibility is created in
the project (so with the seeded pools), and deleted with the last pool in it, so they never keep an emptied project
from being deleted. Every pool records its scope in `status.scope_id`, which v1 now reports too.

### Permissions and quotas
- Permissions: `vpc.pools.*`, `vpc.networks.*`, `vpc.subnets.*`, `vpc.allocations.*`, `vpc.securityGroups.*`,
  `vpc.securityRules.*`, `vpc.routeTables.*`, `vpc.routes.*`, `vpc.scopes.*` (v1alpha1 reuses the v1 permissions). `ListBy*` need `list` on the filtered resource;
  `GetDefault` needs `vpc.networks.get` on the project.
- Quotas: not enforced yet.

## Terraform

| Kind | Terraform type | Service | RPCs called | Tier |
|---|---|---|---|---|
| Resource | `nebius_vpc_v1_allocation` | `nebius.vpc.v1.AllocationService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_vpc_v1_network` | `nebius.vpc.v1.NetworkService` | Get, GetByName, GetDefault, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_vpc_v1_pool` | `nebius.vpc.v1.PoolService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_vpc_v1_route` | `nebius.vpc.v1.RouteService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_vpc_v1_route_table` | `nebius.vpc.v1.RouteTableService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_vpc_v1_security_group` | `nebius.vpc.v1.SecurityGroupService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_vpc_v1_security_rule` | `nebius.vpc.v1.SecurityRuleService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_vpc_v1_subnet` | `nebius.vpc.v1.SubnetService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T2 |
| Resource | `nebius_vpc_v1alpha1_allocation` | `nebius.vpc.v1alpha1.AllocationService` | Get, GetByName, Create, Update, Delete + OperationService.Get | T1 |
| Data source | `nebius_vpc_v1_allocation` | `nebius.vpc.v1.AllocationService` | Get, GetByName | T2 |
| Data source | `nebius_vpc_v1_default_network` | `nebius.vpc.v1.NetworkService` | GetDefault | T2 |
| Data source | `nebius_vpc_v1_network` | `nebius.vpc.v1.NetworkService` | Get, GetByName, GetDefault | T2 |
| Data source | `nebius_vpc_v1_pool` | `nebius.vpc.v1.PoolService` | Get, GetByName | T2 |
| Data source | `nebius_vpc_v1_route` | `nebius.vpc.v1.RouteService` | Get, GetByName | T2 |
| Data source | `nebius_vpc_v1_route_table` | `nebius.vpc.v1.RouteTableService` | Get, GetByName | T2 |
| Data source | `nebius_vpc_v1_security_group` | `nebius.vpc.v1.SecurityGroupService` | Get, GetByName | T2 |
| Data source | `nebius_vpc_v1_security_rule` | `nebius.vpc.v1.SecurityRuleService` | Get, GetByName | T2 |
| Data source | `nebius_vpc_v1_subnet` | `nebius.vpc.v1.SubnetService` | Get, GetByName | T2 |
| Data source | `nebius_vpc_v1alpha1_allocation` | `nebius.vpc.v1alpha1.AllocationService` | Get, GetByName | T1 |
| Data source | `nebius_vpc_v1alpha1_network` | `nebius.vpc.v1alpha1.NetworkService` | Get, GetByName | T1 |
| Data source | `nebius_vpc_v1alpha1_pool` | `nebius.vpc.v1alpha1.PoolService` | Get, GetByName | T1 |
| Data source | `nebius_vpc_v1alpha1_scope` | `nebius.vpc.v1alpha1.ScopeService` | Get, GetByName | T1 |
| Data source | `nebius_vpc_v1alpha1_subnet` | `nebius.vpc.v1alpha1.SubnetService` | Get, GetByName | T1 |

`nebius_vpc_v1_default_network` (data source) calls `NetworkService.GetDefault`, so seeding a default network per project
is required for M1.

## Configuration

| Env | Default | Description |
|---|---|---|
| `NEBIUS_MOCKER_VPC_DEFAULT_PRIVATE_CIDR` | `10.0.0.0/13` | CIDR of each project's `default-network-pool` |
| `NEBIUS_MOCKER_VPC_PUBLIC_CIDR` | `203.0.113.0/24` | CIDR of `default-public-pool` |

## Open questions
- Real default subnet prefix sizes and whether upstream echoes resolved CIDRs into `spec`.
- Whether upstream refuses deleting a route table that still has routes (the mock cascades them).
- Exact `status.assignment` shape for each consumer type.
- Whether upstream clears `status.details.subnet_id` when an allocation changes from `subnet_id` to `pool_id`. The
  mock clears it, which is what the field's documentation implies, and what lets Terraform delete the old subnet.

## API surface

<!-- generated from nebius/api; do not hand-edit tables -->

#### `nebius.vpc.v1.AllocationService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/allocation_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/allocation_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetAllocationRequest` | `Allocation` | ✅ |
| `GetByName` | `GetAllocationByNameRequest` | `Allocation` | ✅ |
| `List` | `ListAllocationsRequest` | `ListAllocationsResponse` |  |
| `ListAggregated` | `ListAggregatedAllocationsRequest` | `ListAllocationsResponse` |  |
| `ListByPool` | `ListAllocationsByPoolRequest` | `ListAllocationsResponse` |  |
| `ListBySubnet` | `ListAllocationsBySubnetRequest` | `ListAllocationsResponse` |  |
| `Create` | `CreateAllocationRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateAllocationRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteAllocationRequest` | `common.v1.Operation` | ✅ |

#### `nebius.vpc.v1.NetworkService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/network_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/network_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetNetworkRequest` | `Network` | ✅ |
| `GetByName` | `GetNetworkByNameRequest` | `Network` | ✅ |
| `GetDefault` | `GetDefaultNetworkRequest` | `GetDefaultNetworkResponse` | ✅ |
| `List` | `ListNetworksRequest` | `ListNetworksResponse` |  |
| `ListAggregated` | `ListAggregatedNetworksRequest` | `ListNetworksResponse` |  |
| `Create` | `CreateNetworkRequest` | `common.v1.Operation` | ✅ |
| `CreateDefault` | `CreateDefaultNetworkRequest` | `common.v1.Operation` |  |
| `Update` | `UpdateNetworkRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteNetworkRequest` | `common.v1.Operation` | ✅ |

#### `nebius.vpc.v1.PoolService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/pool_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/pool_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetPoolRequest` | `Pool` | ✅ |
| `GetByName` | `GetPoolByNameRequest` | `Pool` | ✅ |
| `List` | `ListPoolsRequest` | `ListPoolsResponse` |  |
| `ListAggregated` | `ListAggregatedPoolsRequest` | `ListPoolsResponse` |  |
| `ListBySourcePool` | `ListPoolsBySourcePoolRequest` | `ListPoolsResponse` |  |
| `Create` | `CreatePoolRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdatePoolRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeletePoolRequest` | `common.v1.Operation` | ✅ |

#### `nebius.vpc.v1.RouteService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/route_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/route_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetRouteRequest` | `Route` | ✅ |
| `GetByName` | `GetRouteByNameRequest` | `Route` | ✅ |
| `List` | `ListRoutesRequest` | `ListRoutesResponse` |  |
| `Create` | `CreateRouteRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateRouteRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteRouteRequest` | `common.v1.Operation` | ✅ |

#### `nebius.vpc.v1.RouteTableService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/route_table_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/route_table_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetRouteTableRequest` | `RouteTable` | ✅ |
| `GetByName` | `GetRouteTableByNameRequest` | `RouteTable` | ✅ |
| `List` | `ListRouteTablesRequest` | `ListRouteTablesResponse` |  |
| `ListAggregated` | `ListAggregatedRouteTablesRequest` | `ListRouteTablesResponse` |  |
| `ListByNetwork` | `ListRouteTablesByNetworkRequest` | `ListRouteTablesResponse` |  |
| `Create` | `CreateRouteTableRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateRouteTableRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteRouteTableRequest` | `common.v1.Operation` | ✅ |

#### `nebius.vpc.v1.SecurityGroupService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/security_group_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/security_group_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetSecurityGroupRequest` | `SecurityGroup` | ✅ |
| `GetByName` | `GetSecurityGroupByNameRequest` | `SecurityGroup` | ✅ |
| `List` | `ListSecurityGroupsRequest` | `ListSecurityGroupsResponse` |  |
| `ListAggregated` | `ListAggregatedSecurityGroupsRequest` | `ListSecurityGroupsResponse` |  |
| `ListByNetwork` | `ListSecurityGroupsByNetworkRequest` | `ListSecurityGroupsResponse` |  |
| `Create` | `CreateSecurityGroupRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateSecurityGroupRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteSecurityGroupRequest` | `common.v1.Operation` | ✅ |

#### `nebius.vpc.v1.SecurityRuleService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/security_rule_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/security_rule_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetSecurityRuleRequest` | `SecurityRule` | ✅ |
| `GetByName` | `GetSecurityRuleByNameRequest` | `SecurityRule` | ✅ |
| `List` | `ListSecurityRulesRequest` | `ListSecurityRulesResponse` |  |
| `Create` | `CreateSecurityRuleRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateSecurityRuleRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteSecurityRuleRequest` | `common.v1.Operation` | ✅ |

#### `nebius.vpc.v1.SubnetService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/subnet_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/subnet_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetSubnetRequest` | `Subnet` | ✅ |
| `GetByName` | `GetSubnetByNameRequest` | `Subnet` | ✅ |
| `List` | `ListSubnetsRequest` | `ListSubnetsResponse` |  |
| `ListAggregated` | `ListAggregatedSubnetsRequest` | `ListSubnetsResponse` |  |
| `ListByNetwork` | `ListSubnetsByNetworkRequest` | `ListSubnetsResponse` |  |
| `Create` | `CreateSubnetRequest` | `common.v1.Operation` | ✅ |
| `Update` | `UpdateSubnetRequest` | `common.v1.Operation` | ✅ |
| `Delete` | `DeleteSubnetRequest` | `common.v1.Operation` | ✅ |

#### `nebius.vpc.v1.TargetGroupService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1/target_group_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1/target_group_service.proto)

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetTargetGroupRequest` | `TargetGroup` |  |
| `Update` | `UpdateTargetGroupRequest` | `common.v1.Operation` |  |

#### `nebius.vpc.v1alpha1.AllocationService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1alpha1/allocation_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1alpha1/allocation_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetAllocationRequest` | `Allocation` | ✅ |
| `GetByName` | `GetAllocationByNameRequest` | `Allocation` | ✅ |
| `List` | `ListAllocationsRequest` | `ListAllocationsResponse` |  |
| `Create` | `CreateAllocationRequest` | `common.v1alpha1.Operation` | ✅ |
| `Update` | `UpdateAllocationRequest` | `common.v1alpha1.Operation` | ✅ |
| `Delete` | `DeleteAllocationRequest` | `common.v1alpha1.Operation` | ✅ |

#### `nebius.vpc.v1alpha1.NetworkService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1alpha1/network_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1alpha1/network_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetNetworkRequest` | `Network` | ✅ |
| `GetByName` | `GetNetworkByNameRequest` | `Network` | ✅ |
| `GetDefault` | `GetDefaultNetworkRequest` | `GetDefaultNetworkResponse` |  |
| `List` | `ListNetworksRequest` | `ListNetworksResponse` |  |

#### `nebius.vpc.v1alpha1.PoolService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1alpha1/pool_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1alpha1/pool_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetPoolRequest` | `Pool` | ✅ |
| `GetByName` | `GetPoolByNameRequest` | `Pool` | ✅ |
| `List` | `ListPoolsRequest` | `ListPoolsResponse` |  |

#### `nebius.vpc.v1alpha1.ScopeService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1alpha1/scope_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1alpha1/scope_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetScopeRequest` | `Scope` | ✅ |
| `GetByName` | `GetScopeByNameRequest` | `Scope` | ✅ |
| `List` | `ListScopesRequest` | `ListScopesResponse` |  |

#### `nebius.vpc.v1alpha1.SubnetService`

Endpoint: `vpc.api.nebius.cloud:443` · Proto: [`nebius/vpc/v1alpha1/subnet_service.proto`](https://github.com/nebius/api/blob/main/nebius/vpc/v1alpha1/subnet_service.proto) · **used by Terraform**

| RPC | Request | Response | TF |
|---|---|---|---|
| `Get` | `GetSubnetRequest` | `Subnet` | ✅ |
| `GetByName` | `GetSubnetByNameRequest` | `Subnet` | ✅ |
| `List` | `ListSubnetsRequest` | `ListSubnetsResponse` |  |
| `ListByNetwork` | `ListSubnetsByNetworkRequest` | `ListSubnetsResponse` |  |
