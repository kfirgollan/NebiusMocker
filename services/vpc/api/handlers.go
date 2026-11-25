// One handler per VPC gRPC service; they differ only in message types.

package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/services/vpc/domain"
)

// PoolHandler implements nebius.vpc.v1.PoolService.
type PoolHandler struct {
	vpcpb.UnimplementedPoolServiceServer
	svc *domain.PoolService
}

var _ vpcpb.PoolServiceServer = (*PoolHandler)(nil)

// NewPoolHandler returns a handler backed by svc.
func NewPoolHandler(svc *domain.PoolService) *PoolHandler { return &PoolHandler{svc: svc} }

// Get returns a pool by ID.
func (h *PoolHandler) Get(ctx context.Context, req *vpcpb.GetPoolRequest) (*vpcpb.Pool, error) {
	return reply[*vpcpb.Pool](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a pool by parent and name.
func (h *PoolHandler) GetByName(ctx context.Context, req *vpcpb.GetPoolByNameRequest) (*vpcpb.Pool, error) {
	return reply[*vpcpb.Pool](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the pools in a parent.
func (h *PoolHandler) List(ctx context.Context, req *vpcpb.ListPoolsRequest) (*vpcpb.ListPoolsResponse, error) {
	return listPools(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListAggregated returns one page of pools (ListAggregated).
func (h *PoolHandler) ListAggregated(ctx context.Context, req *vpcpb.ListAggregatedPoolsRequest) (*vpcpb.ListPoolsResponse, error) {
	return listPools(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListBySourcePool returns one page of pools (ListBySourcePool).
func (h *PoolHandler) ListBySourcePool(ctx context.Context, req *vpcpb.ListPoolsBySourcePoolRequest) (*vpcpb.ListPoolsResponse, error) {
	return listPools(ctx)(h.svc.ListBySourcePool(ctx, req.GetPoolId(), req.GetPageSize(), req.GetPageToken()))
}

// Create starts a pool creation operation.
func (h *PoolHandler) Create(ctx context.Context, req *vpcpb.CreatePoolRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.PoolSpec]{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a pool update operation; X-ResetMask selects fields to clear.
func (h *PoolHandler) Update(ctx context.Context, req *vpcpb.UpdatePoolRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return opReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.PoolSpec]{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req,
	}))
}

// Delete starts a pool deletion operation.
func (h *PoolHandler) Delete(ctx context.Context, req *vpcpb.DeletePoolRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

func listPools(ctx context.Context) func(domain.Page[*vpcpb.Pool], error) (*vpcpb.ListPoolsResponse, error) {
	return func(p domain.Page[*vpcpb.Pool], err error) (*vpcpb.ListPoolsResponse, error) {
		return reply[*vpcpb.ListPoolsResponse](ctx)(&vpcpb.ListPoolsResponse{Items: p.Items, NextPageToken: p.NextPageToken}, err)
	}
}

// NetworkHandler implements nebius.vpc.v1.NetworkService.
type NetworkHandler struct {
	vpcpb.UnimplementedNetworkServiceServer
	svc *domain.NetworkService
}

var _ vpcpb.NetworkServiceServer = (*NetworkHandler)(nil)

// NewNetworkHandler returns a handler backed by svc.
func NewNetworkHandler(svc *domain.NetworkService) *NetworkHandler { return &NetworkHandler{svc: svc} }

// Get returns a network by ID.
func (h *NetworkHandler) Get(ctx context.Context, req *vpcpb.GetNetworkRequest) (*vpcpb.Network, error) {
	return reply[*vpcpb.Network](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a network by parent and name.
func (h *NetworkHandler) GetByName(ctx context.Context, req *vpcpb.GetNetworkByNameRequest) (*vpcpb.Network, error) {
	return reply[*vpcpb.Network](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the networks in a parent.
func (h *NetworkHandler) List(ctx context.Context, req *vpcpb.ListNetworksRequest) (*vpcpb.ListNetworksResponse, error) {
	return listNetworks(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListAggregated returns one page of networks (ListAggregated).
func (h *NetworkHandler) ListAggregated(ctx context.Context, req *vpcpb.ListAggregatedNetworksRequest) (*vpcpb.ListNetworksResponse, error) {
	return listNetworks(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// Create starts a network creation operation.
func (h *NetworkHandler) Create(ctx context.Context, req *vpcpb.CreateNetworkRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.NetworkSpec]{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a network update operation; X-ResetMask selects fields to clear.
func (h *NetworkHandler) Update(ctx context.Context, req *vpcpb.UpdateNetworkRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return opReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.NetworkSpec]{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req,
	}))
}

// Delete starts a network deletion operation.
func (h *NetworkHandler) Delete(ctx context.Context, req *vpcpb.DeleteNetworkRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

func listNetworks(ctx context.Context) func(domain.Page[*vpcpb.Network], error) (*vpcpb.ListNetworksResponse, error) {
	return func(p domain.Page[*vpcpb.Network], err error) (*vpcpb.ListNetworksResponse, error) {
		return reply[*vpcpb.ListNetworksResponse](ctx)(&vpcpb.ListNetworksResponse{Items: p.Items, NextPageToken: p.NextPageToken}, err)
	}
}

// SubnetHandler implements nebius.vpc.v1.SubnetService.
type SubnetHandler struct {
	vpcpb.UnimplementedSubnetServiceServer
	svc *domain.SubnetService
}

var _ vpcpb.SubnetServiceServer = (*SubnetHandler)(nil)

// NewSubnetHandler returns a handler backed by svc.
func NewSubnetHandler(svc *domain.SubnetService) *SubnetHandler { return &SubnetHandler{svc: svc} }

// Get returns a subnet by ID.
func (h *SubnetHandler) Get(ctx context.Context, req *vpcpb.GetSubnetRequest) (*vpcpb.Subnet, error) {
	return reply[*vpcpb.Subnet](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a subnet by parent and name.
func (h *SubnetHandler) GetByName(ctx context.Context, req *vpcpb.GetSubnetByNameRequest) (*vpcpb.Subnet, error) {
	return reply[*vpcpb.Subnet](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the subnets in a parent.
func (h *SubnetHandler) List(ctx context.Context, req *vpcpb.ListSubnetsRequest) (*vpcpb.ListSubnetsResponse, error) {
	return listSubnets(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListAggregated returns one page of subnets (ListAggregated).
func (h *SubnetHandler) ListAggregated(ctx context.Context, req *vpcpb.ListAggregatedSubnetsRequest) (*vpcpb.ListSubnetsResponse, error) {
	return listSubnets(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListByNetwork returns one page of subnets (ListByNetwork).
func (h *SubnetHandler) ListByNetwork(ctx context.Context, req *vpcpb.ListSubnetsByNetworkRequest) (*vpcpb.ListSubnetsResponse, error) {
	return listSubnets(ctx)(h.svc.ListByNetwork(ctx, req.GetNetworkId(), req.GetPageSize(), req.GetPageToken()))
}

// Create starts a subnet creation operation.
func (h *SubnetHandler) Create(ctx context.Context, req *vpcpb.CreateSubnetRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.SubnetSpec]{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a subnet update operation; X-ResetMask selects fields to clear.
func (h *SubnetHandler) Update(ctx context.Context, req *vpcpb.UpdateSubnetRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return opReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.SubnetSpec]{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req,
	}))
}

// Delete starts a subnet deletion operation.
func (h *SubnetHandler) Delete(ctx context.Context, req *vpcpb.DeleteSubnetRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

func listSubnets(ctx context.Context) func(domain.Page[*vpcpb.Subnet], error) (*vpcpb.ListSubnetsResponse, error) {
	return func(p domain.Page[*vpcpb.Subnet], err error) (*vpcpb.ListSubnetsResponse, error) {
		return reply[*vpcpb.ListSubnetsResponse](ctx)(&vpcpb.ListSubnetsResponse{Items: p.Items, NextPageToken: p.NextPageToken}, err)
	}
}

// AllocationHandler implements nebius.vpc.v1.AllocationService.
type AllocationHandler struct {
	vpcpb.UnimplementedAllocationServiceServer
	svc *domain.AllocationService
}

var _ vpcpb.AllocationServiceServer = (*AllocationHandler)(nil)

// NewAllocationHandler returns a handler backed by svc.
func NewAllocationHandler(svc *domain.AllocationService) *AllocationHandler {
	return &AllocationHandler{svc: svc}
}

// Get returns a allocation by ID.
func (h *AllocationHandler) Get(ctx context.Context, req *vpcpb.GetAllocationRequest) (*vpcpb.Allocation, error) {
	return reply[*vpcpb.Allocation](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a allocation by parent and name.
func (h *AllocationHandler) GetByName(ctx context.Context, req *vpcpb.GetAllocationByNameRequest) (*vpcpb.Allocation, error) {
	return reply[*vpcpb.Allocation](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the allocations in a parent.
func (h *AllocationHandler) List(ctx context.Context, req *vpcpb.ListAllocationsRequest) (*vpcpb.ListAllocationsResponse, error) {
	return listAllocations(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListAggregated returns one page of allocations (ListAggregated).
func (h *AllocationHandler) ListAggregated(ctx context.Context, req *vpcpb.ListAggregatedAllocationsRequest) (*vpcpb.ListAllocationsResponse, error) {
	return listAllocations(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListByPool returns one page of allocations (ListByPool).
func (h *AllocationHandler) ListByPool(ctx context.Context, req *vpcpb.ListAllocationsByPoolRequest) (*vpcpb.ListAllocationsResponse, error) {
	return listAllocations(ctx)(h.svc.ListByPool(ctx, req.GetPoolId(), req.GetPageSize(), req.GetPageToken()))
}

// ListBySubnet returns one page of allocations (ListBySubnet).
func (h *AllocationHandler) ListBySubnet(ctx context.Context, req *vpcpb.ListAllocationsBySubnetRequest) (*vpcpb.ListAllocationsResponse, error) {
	return listAllocations(ctx)(h.svc.ListBySubnet(ctx, req.GetSubnetId(), req.GetPageSize(), req.GetPageToken()))
}

// Create starts a allocation creation operation.
func (h *AllocationHandler) Create(ctx context.Context, req *vpcpb.CreateAllocationRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.AllocationSpec]{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a allocation update operation; X-ResetMask selects fields to clear.
func (h *AllocationHandler) Update(ctx context.Context, req *vpcpb.UpdateAllocationRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return opReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.AllocationSpec]{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req,
	}))
}

// Delete starts a allocation deletion operation.
func (h *AllocationHandler) Delete(ctx context.Context, req *vpcpb.DeleteAllocationRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

func listAllocations(ctx context.Context) func(domain.Page[*vpcpb.Allocation], error) (*vpcpb.ListAllocationsResponse, error) {
	return func(p domain.Page[*vpcpb.Allocation], err error) (*vpcpb.ListAllocationsResponse, error) {
		return reply[*vpcpb.ListAllocationsResponse](ctx)(&vpcpb.ListAllocationsResponse{Items: p.Items, NextPageToken: p.NextPageToken}, err)
	}
}

// RouteTableHandler implements nebius.vpc.v1.RouteTableService.
type RouteTableHandler struct {
	vpcpb.UnimplementedRouteTableServiceServer
	svc *domain.RouteTableService
}

var _ vpcpb.RouteTableServiceServer = (*RouteTableHandler)(nil)

// NewRouteTableHandler returns a handler backed by svc.
func NewRouteTableHandler(svc *domain.RouteTableService) *RouteTableHandler {
	return &RouteTableHandler{svc: svc}
}

// Get returns a route table by ID.
func (h *RouteTableHandler) Get(ctx context.Context, req *vpcpb.GetRouteTableRequest) (*vpcpb.RouteTable, error) {
	return reply[*vpcpb.RouteTable](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a route table by parent and name.
func (h *RouteTableHandler) GetByName(ctx context.Context, req *vpcpb.GetRouteTableByNameRequest) (*vpcpb.RouteTable, error) {
	return reply[*vpcpb.RouteTable](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the route tables in a parent.
func (h *RouteTableHandler) List(ctx context.Context, req *vpcpb.ListRouteTablesRequest) (*vpcpb.ListRouteTablesResponse, error) {
	return listRouteTables(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListAggregated returns one page of route tables (ListAggregated).
func (h *RouteTableHandler) ListAggregated(ctx context.Context, req *vpcpb.ListAggregatedRouteTablesRequest) (*vpcpb.ListRouteTablesResponse, error) {
	return listRouteTables(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListByNetwork returns one page of route tables (ListByNetwork).
func (h *RouteTableHandler) ListByNetwork(ctx context.Context, req *vpcpb.ListRouteTablesByNetworkRequest) (*vpcpb.ListRouteTablesResponse, error) {
	return listRouteTables(ctx)(h.svc.ListByNetwork(ctx, req.GetNetworkId(), req.GetPageSize(), req.GetPageToken()))
}

// Create starts a route table creation operation.
func (h *RouteTableHandler) Create(ctx context.Context, req *vpcpb.CreateRouteTableRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.RouteTableSpec]{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a route table update operation; X-ResetMask selects fields to clear.
func (h *RouteTableHandler) Update(ctx context.Context, req *vpcpb.UpdateRouteTableRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return opReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.RouteTableSpec]{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req,
	}))
}

// Delete starts a route table deletion operation.
func (h *RouteTableHandler) Delete(ctx context.Context, req *vpcpb.DeleteRouteTableRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

func listRouteTables(ctx context.Context) func(domain.Page[*vpcpb.RouteTable], error) (*vpcpb.ListRouteTablesResponse, error) {
	return func(p domain.Page[*vpcpb.RouteTable], err error) (*vpcpb.ListRouteTablesResponse, error) {
		return reply[*vpcpb.ListRouteTablesResponse](ctx)(&vpcpb.ListRouteTablesResponse{Items: p.Items, NextPageToken: p.NextPageToken}, err)
	}
}

// RouteHandler implements nebius.vpc.v1.RouteService.
type RouteHandler struct {
	vpcpb.UnimplementedRouteServiceServer
	svc *domain.RouteService
}

var _ vpcpb.RouteServiceServer = (*RouteHandler)(nil)

// NewRouteHandler returns a handler backed by svc.
func NewRouteHandler(svc *domain.RouteService) *RouteHandler { return &RouteHandler{svc: svc} }

// Get returns a route by ID.
func (h *RouteHandler) Get(ctx context.Context, req *vpcpb.GetRouteRequest) (*vpcpb.Route, error) {
	return reply[*vpcpb.Route](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a route by parent and name.
func (h *RouteHandler) GetByName(ctx context.Context, req *vpcpb.GetRouteByNameRequest) (*vpcpb.Route, error) {
	return reply[*vpcpb.Route](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the routes in a parent.
func (h *RouteHandler) List(ctx context.Context, req *vpcpb.ListRoutesRequest) (*vpcpb.ListRoutesResponse, error) {
	return listRoutes(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// Create starts a route creation operation.
func (h *RouteHandler) Create(ctx context.Context, req *vpcpb.CreateRouteRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.RouteSpec]{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a route update operation; X-ResetMask selects fields to clear.
func (h *RouteHandler) Update(ctx context.Context, req *vpcpb.UpdateRouteRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return opReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.RouteSpec]{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req,
	}))
}

// Delete starts a route deletion operation.
func (h *RouteHandler) Delete(ctx context.Context, req *vpcpb.DeleteRouteRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

func listRoutes(ctx context.Context) func(domain.Page[*vpcpb.Route], error) (*vpcpb.ListRoutesResponse, error) {
	return func(p domain.Page[*vpcpb.Route], err error) (*vpcpb.ListRoutesResponse, error) {
		return reply[*vpcpb.ListRoutesResponse](ctx)(&vpcpb.ListRoutesResponse{Items: p.Items, NextPageToken: p.NextPageToken}, err)
	}
}

// SecurityGroupHandler implements nebius.vpc.v1.SecurityGroupService.
type SecurityGroupHandler struct {
	vpcpb.UnimplementedSecurityGroupServiceServer
	svc *domain.SecurityGroupService
}

var _ vpcpb.SecurityGroupServiceServer = (*SecurityGroupHandler)(nil)

// NewSecurityGroupHandler returns a handler backed by svc.
func NewSecurityGroupHandler(svc *domain.SecurityGroupService) *SecurityGroupHandler {
	return &SecurityGroupHandler{svc: svc}
}

// Get returns a security group by ID.
func (h *SecurityGroupHandler) Get(ctx context.Context, req *vpcpb.GetSecurityGroupRequest) (*vpcpb.SecurityGroup, error) {
	return reply[*vpcpb.SecurityGroup](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a security group by parent and name.
func (h *SecurityGroupHandler) GetByName(ctx context.Context, req *vpcpb.GetSecurityGroupByNameRequest) (*vpcpb.SecurityGroup, error) {
	return reply[*vpcpb.SecurityGroup](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the security groups in a parent.
func (h *SecurityGroupHandler) List(ctx context.Context, req *vpcpb.ListSecurityGroupsRequest) (*vpcpb.ListSecurityGroupsResponse, error) {
	return listSecurityGroups(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListAggregated returns one page of security groups (ListAggregated).
func (h *SecurityGroupHandler) ListAggregated(ctx context.Context, req *vpcpb.ListAggregatedSecurityGroupsRequest) (*vpcpb.ListSecurityGroupsResponse, error) {
	return listSecurityGroups(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListByNetwork returns one page of security groups (ListByNetwork).
func (h *SecurityGroupHandler) ListByNetwork(ctx context.Context, req *vpcpb.ListSecurityGroupsByNetworkRequest) (*vpcpb.ListSecurityGroupsResponse, error) {
	return listSecurityGroups(ctx)(h.svc.ListByNetwork(ctx, req.GetNetworkId(), req.GetPageSize(), req.GetPageToken()))
}

// Create starts a security group creation operation.
func (h *SecurityGroupHandler) Create(ctx context.Context, req *vpcpb.CreateSecurityGroupRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.SecurityGroupSpec]{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a security group update operation; X-ResetMask selects fields to clear.
func (h *SecurityGroupHandler) Update(ctx context.Context, req *vpcpb.UpdateSecurityGroupRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return opReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.SecurityGroupSpec]{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req,
	}))
}

// Delete starts a security group deletion operation.
func (h *SecurityGroupHandler) Delete(ctx context.Context, req *vpcpb.DeleteSecurityGroupRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

func listSecurityGroups(ctx context.Context) func(domain.Page[*vpcpb.SecurityGroup], error) (*vpcpb.ListSecurityGroupsResponse, error) {
	return func(p domain.Page[*vpcpb.SecurityGroup], err error) (*vpcpb.ListSecurityGroupsResponse, error) {
		return reply[*vpcpb.ListSecurityGroupsResponse](ctx)(&vpcpb.ListSecurityGroupsResponse{Items: p.Items, NextPageToken: p.NextPageToken}, err)
	}
}

// SecurityRuleHandler implements nebius.vpc.v1.SecurityRuleService.
type SecurityRuleHandler struct {
	vpcpb.UnimplementedSecurityRuleServiceServer
	svc *domain.SecurityRuleService
}

var _ vpcpb.SecurityRuleServiceServer = (*SecurityRuleHandler)(nil)

// NewSecurityRuleHandler returns a handler backed by svc.
func NewSecurityRuleHandler(svc *domain.SecurityRuleService) *SecurityRuleHandler {
	return &SecurityRuleHandler{svc: svc}
}

// Get returns a security rule by ID.
func (h *SecurityRuleHandler) Get(ctx context.Context, req *vpcpb.GetSecurityRuleRequest) (*vpcpb.SecurityRule, error) {
	return reply[*vpcpb.SecurityRule](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a security rule by parent and name.
func (h *SecurityRuleHandler) GetByName(ctx context.Context, req *vpcpb.GetSecurityRuleByNameRequest) (*vpcpb.SecurityRule, error) {
	return reply[*vpcpb.SecurityRule](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the security rules in a parent.
func (h *SecurityRuleHandler) List(ctx context.Context, req *vpcpb.ListSecurityRulesRequest) (*vpcpb.ListSecurityRulesResponse, error) {
	return listSecurityRules(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// Create starts a security rule creation operation.
func (h *SecurityRuleHandler) Create(ctx context.Context, req *vpcpb.CreateSecurityRuleRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.SecurityRuleSpec]{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a security rule update operation; X-ResetMask selects fields to clear.
func (h *SecurityRuleHandler) Update(ctx context.Context, req *vpcpb.UpdateSecurityRuleRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return opReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.SecurityRuleSpec]{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req,
	}))
}

// Delete starts a security rule deletion operation.
func (h *SecurityRuleHandler) Delete(ctx context.Context, req *vpcpb.DeleteSecurityRuleRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

func listSecurityRules(ctx context.Context) func(domain.Page[*vpcpb.SecurityRule], error) (*vpcpb.ListSecurityRulesResponse, error) {
	return func(p domain.Page[*vpcpb.SecurityRule], err error) (*vpcpb.ListSecurityRulesResponse, error) {
		return reply[*vpcpb.ListSecurityRulesResponse](ctx)(&vpcpb.ListSecurityRulesResponse{Items: p.Items, NextPageToken: p.NextPageToken}, err)
	}
}
