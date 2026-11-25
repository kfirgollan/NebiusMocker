# VPC — Virtual Private Cloud

> Module: `services/vpc` · Image: `nebius-mocker-vpc` · `NEBIUS_MOCKER_SERVICES=vpc`
> M1 tier: **T2** · Endpoint: `vpc.api.nebius.cloud`

## Overview

VPC models IP address management and private networking: **pools** (CIDR sets), **networks** that draw from pools,
**subnets** inside networks, **allocations** (private or public IP addresses), **security groups and rules**,
**route tables and routes**, and **target groups** (load-balancer backends). `v1alpha1` adds **scopes** and older
network/subnet/pool shapes, which the provider exposes as data sources plus `nebius_vpc_v1alpha1_allocation`.

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
| Scope (v1alpha1) | `Scope` | `vpcscope` | project | |

## Emulation design

### IPAM core
This module carries the most real logic in M1, because outputs must be consistent and must survive an empty
`terraform plan`:

- **Pools.** Each project gets a seeded **default private pool** (`10.0.0.0/8`, configurable) and the tenant gets a
  **public pool** (default `203.0.113.0/24`, from the TEST-NET-3 documentation range, so mock IPs can never collide with
  real ones). A user pool with `source_pool_id` must carve its CIDRs from the source pool. Prefix length rules match
  upstream validation.
- **Networks.** `spec.ipv4_private_pools.pools[].id` must reference pools in the same project. With no pools given, the
  network uses the project default pool. `GetDefault` returns the project's default network.
  `CreateDefault` creates one if missing (seeded by default, named `default-network`).
- **Subnets.** CIDRs come from the network's pools (`use_network_pools`) or from explicit pools. A
  `cidrs[].cidr` value is either a block (`10.1.2.0/24`), which must be free and inside the pool, or a bare prefix length
  (`/24`), for which the mocker auto-allocates the lowest free block. Pools work the same way. `max_mask_length` bounds
  later allocations. Overlapping subnets in the same network → `INVALID_ARGUMENT`.
  `status.ipv4_private_cidrs` echoes what was allocated. The default subnet of the default network has `status.default=true`.
- **Allocations.** Private allocations pick the next free address in the subnet's CIDR (deterministic: lowest free
  address, skipping network/gateway/broadcast addresses). Public allocations draw from the public pool.
  `status.assignment` records the consumer (an instance NIC or a load balancer) and is set and cleared through
  `peer` calls from `compute`. Deleting an assigned allocation → `FAILED_PRECONDITION`.
- **Dynamic addresses.** When `compute` creates an instance NIC without an `allocation_id`, it asks `vpc` (via `peer`)
  for an ephemeral allocation that is owned by the instance and released with it.

### Security groups, rules and routes
These are stored and validated (port ranges, CIDR syntax, protocol enums, priority uniqueness per group/direction) but not
enforced: there is no packet path. Rules are fully immutable (every spec field is `IMMUTABLE`), so Terraform plans a
replacement on any change. The conformance kit checks this. Route next hops must reference existing allocations or
default egress.

### Integrity
| Delete | Refused while |
|---|---|
| Pool | Networks or subnets reference it, or child pools exist |
| Network | Subnets, security groups or route tables exist |
| Subnet | Allocations, instance NICs (via `peer` to compute) or mk8s clusters (via `peer` to mk8s) reference it |
| Security group | NICs reference it |

### Aggregated lists
`ListAggregated` returns resources across all projects of the tenant the caller can see. `ListByNetwork`, `ListBySubnet`
and `ListByPool` are index queries.

### Permissions and quotas
- Permissions: `vpc.pools.*`, `vpc.networks.*`, `vpc.subnets.*`, `vpc.allocations.*`, `vpc.securityGroups.*`, `vpc.securityRules.*`, `vpc.routeTables.*`, `vpc.routes.*`, `vpc.targetGroups.*`.
- Quotas: `vpc.network.count`, `vpc.subnet.count` (project), `vpc.allocation.public.count` (tenant), `vpc.securitygroup.count`.

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
| `NEBIUS_MOCKER_VPC_DEFAULT_PRIVATE_CIDR` | `10.0.0.0/8` | Per-project default private pool |
| `NEBIUS_MOCKER_VPC_PUBLIC_CIDR` | `203.0.113.0/24` | Tenant public pool |
| `NEBIUS_MOCKER_VPC_SEED_DEFAULT_NETWORK` | `1` | Create `default-network` + default subnet per project |
| `NEBIUS_MOCKER_VPC_ALLOCATION_STRATEGY` | `lowest-free` | `lowest-free` \| `random` |

## Open questions
- Real default pool CIDRs and subnet prefix defaults.
- Whether network deletion cascades to the default subnet.
- Exact `status.assignment` shape for each consumer type.

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
