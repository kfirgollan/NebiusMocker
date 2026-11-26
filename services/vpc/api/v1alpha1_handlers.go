// One handler per v1alpha1 VPC gRPC service. They share the v1 domain: each
// call goes to the same service as its v1 twin and converts the result.

package api

import (
	"context"

	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/vpc/domain"
)

// AlphaPoolHandler implements nebius.vpc.v1alpha1.PoolService (read-only).
type AlphaPoolHandler struct {
	vpcv1alpha1pb.UnimplementedPoolServiceServer
	svc *domain.PoolService
}

var _ vpcv1alpha1pb.PoolServiceServer = (*AlphaPoolHandler)(nil)

// NewAlphaPoolHandler returns a handler backed by svc.
func NewAlphaPoolHandler(svc *domain.PoolService) *AlphaPoolHandler {
	return &AlphaPoolHandler{svc: svc}
}

// Get returns a pool by ID.
func (h *AlphaPoolHandler) Get(ctx context.Context, req *vpcv1alpha1pb.GetPoolRequest) (*vpcv1alpha1pb.Pool, error) {
	p, err := reply[*vpcpb.Pool](ctx)(h.svc.Get(ctx, req.GetId()))
	return alphaPool(p), err
}

// GetByName returns a pool by parent and name.
func (h *AlphaPoolHandler) GetByName(ctx context.Context, req *vpcv1alpha1pb.GetPoolByNameRequest) (*vpcv1alpha1pb.Pool, error) {
	p, err := reply[*vpcpb.Pool](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
	return alphaPool(p), err
}

// List returns one page of the pools in a parent.
func (h *AlphaPoolHandler) List(ctx context.Context, req *vpcv1alpha1pb.ListPoolsRequest) (*vpcv1alpha1pb.ListPoolsResponse, error) {
	if err := domain.CheckFilter(req.GetFilter()); err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	page, err := reply[domain.Page[*vpcpb.Pool]](ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
	if err != nil {
		return nil, err
	}
	out := &vpcv1alpha1pb.ListPoolsResponse{NextPageToken: page.NextPageToken}
	for _, p := range page.Items {
		out.Items = append(out.Items, alphaPool(p))
	}
	return out, nil
}

// AlphaNetworkHandler implements nebius.vpc.v1alpha1.NetworkService (read-only).
type AlphaNetworkHandler struct {
	vpcv1alpha1pb.UnimplementedNetworkServiceServer
	svc *domain.NetworkService
}

var _ vpcv1alpha1pb.NetworkServiceServer = (*AlphaNetworkHandler)(nil)

// NewAlphaNetworkHandler returns a handler backed by svc.
func NewAlphaNetworkHandler(svc *domain.NetworkService) *AlphaNetworkHandler {
	return &AlphaNetworkHandler{svc: svc}
}

// view converts a stored network, looking up its scope.
func (h *AlphaNetworkHandler) view(ctx context.Context, n *vpcpb.Network) (*vpcv1alpha1pb.Network, error) {
	scope, err := reply[string](ctx)(h.svc.Scope(ctx, n))
	if err != nil {
		return nil, err
	}
	return alphaNetwork(n, scope), nil
}

// Get returns a network by ID.
func (h *AlphaNetworkHandler) Get(ctx context.Context, req *vpcv1alpha1pb.GetNetworkRequest) (*vpcv1alpha1pb.Network, error) {
	n, err := reply[*vpcpb.Network](ctx)(h.svc.Get(ctx, req.GetId()))
	if err != nil {
		return nil, err
	}
	return h.view(ctx, n)
}

// GetByName returns a network by parent and name.
func (h *AlphaNetworkHandler) GetByName(ctx context.Context, req *vpcv1alpha1pb.GetNetworkByNameRequest) (*vpcv1alpha1pb.Network, error) {
	n, err := reply[*vpcpb.Network](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
	if err != nil {
		return nil, err
	}
	return h.view(ctx, n)
}

// GetDefault returns the project's default network and its default subnet.
func (h *AlphaNetworkHandler) GetDefault(ctx context.Context, req *vpcv1alpha1pb.GetDefaultNetworkRequest) (*vpcv1alpha1pb.GetDefaultNetworkResponse, error) {
	d, err := reply[domain.DefaultNetwork](ctx)(h.svc.GetDefault(ctx, req.GetParentId()))
	if err != nil {
		return nil, err
	}
	n, err := h.view(ctx, d.Network)
	if err != nil {
		return nil, err
	}
	return &vpcv1alpha1pb.GetDefaultNetworkResponse{Network: n, Subnet: alphaSubnet(d.Subnet)}, nil
}

// List returns one page of the networks in a parent.
func (h *AlphaNetworkHandler) List(ctx context.Context, req *vpcv1alpha1pb.ListNetworksRequest) (*vpcv1alpha1pb.ListNetworksResponse, error) {
	if err := domain.CheckFilter(req.GetFilter()); err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	page, err := reply[domain.Page[*vpcpb.Network]](ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
	if err != nil {
		return nil, err
	}
	out := &vpcv1alpha1pb.ListNetworksResponse{NextPageToken: page.NextPageToken}
	for _, n := range page.Items {
		v, err := h.view(ctx, n)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

// AlphaSubnetHandler implements nebius.vpc.v1alpha1.SubnetService (read-only).
type AlphaSubnetHandler struct {
	vpcv1alpha1pb.UnimplementedSubnetServiceServer
	svc *domain.SubnetService
}

var _ vpcv1alpha1pb.SubnetServiceServer = (*AlphaSubnetHandler)(nil)

// NewAlphaSubnetHandler returns a handler backed by svc.
func NewAlphaSubnetHandler(svc *domain.SubnetService) *AlphaSubnetHandler {
	return &AlphaSubnetHandler{svc: svc}
}

// Get returns a subnet by ID.
func (h *AlphaSubnetHandler) Get(ctx context.Context, req *vpcv1alpha1pb.GetSubnetRequest) (*vpcv1alpha1pb.Subnet, error) {
	sn, err := reply[*vpcpb.Subnet](ctx)(h.svc.Get(ctx, req.GetId()))
	return alphaSubnet(sn), err
}

// GetByName returns a subnet by parent and name.
func (h *AlphaSubnetHandler) GetByName(ctx context.Context, req *vpcv1alpha1pb.GetSubnetByNameRequest) (*vpcv1alpha1pb.Subnet, error) {
	sn, err := reply[*vpcpb.Subnet](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
	return alphaSubnet(sn), err
}

// List returns one page of the subnets in a parent.
func (h *AlphaSubnetHandler) List(ctx context.Context, req *vpcv1alpha1pb.ListSubnetsRequest) (*vpcv1alpha1pb.ListSubnetsResponse, error) {
	if err := domain.CheckFilter(req.GetFilter()); err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return alphaSubnets(ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
}

// ListByNetwork returns one page of the subnets of a network.
func (h *AlphaSubnetHandler) ListByNetwork(ctx context.Context, req *vpcv1alpha1pb.ListSubnetsByNetworkRequest) (*vpcv1alpha1pb.ListSubnetsResponse, error) {
	if err := domain.CheckFilter(req.GetFilter()); err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return alphaSubnets(ctx)(h.svc.ListByNetwork(ctx, req.GetNetworkId(), req.GetPageSize(), req.GetPageToken()))
}

func alphaSubnets(ctx context.Context) func(domain.Page[*vpcpb.Subnet], error) (*vpcv1alpha1pb.ListSubnetsResponse, error) {
	return func(p domain.Page[*vpcpb.Subnet], err error) (*vpcv1alpha1pb.ListSubnetsResponse, error) {
		if err != nil {
			return nil, serviceerror.FromDomain(ctx, err)
		}
		out := &vpcv1alpha1pb.ListSubnetsResponse{NextPageToken: p.NextPageToken}
		for _, sn := range p.Items {
			out.Items = append(out.Items, alphaSubnet(sn))
		}
		return out, nil
	}
}

// AlphaAllocationHandler implements nebius.vpc.v1alpha1.AllocationService.
type AlphaAllocationHandler struct {
	vpcv1alpha1pb.UnimplementedAllocationServiceServer
	svc *domain.AllocationService
}

var _ vpcv1alpha1pb.AllocationServiceServer = (*AlphaAllocationHandler)(nil)

// NewAlphaAllocationHandler returns a handler backed by svc.
func NewAlphaAllocationHandler(svc *domain.AllocationService) *AlphaAllocationHandler {
	return &AlphaAllocationHandler{svc: svc}
}

// Get returns an allocation by ID.
func (h *AlphaAllocationHandler) Get(ctx context.Context, req *vpcv1alpha1pb.GetAllocationRequest) (*vpcv1alpha1pb.Allocation, error) {
	a, err := reply[*vpcpb.Allocation](ctx)(h.svc.Get(ctx, req.GetId()))
	return alphaAllocation(a), err
}

// GetByName returns an allocation by parent and name.
func (h *AlphaAllocationHandler) GetByName(ctx context.Context, req *vpcv1alpha1pb.GetAllocationByNameRequest) (*vpcv1alpha1pb.Allocation, error) {
	a, err := reply[*vpcpb.Allocation](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
	return alphaAllocation(a), err
}

// List returns one page of the allocations in a parent.
func (h *AlphaAllocationHandler) List(ctx context.Context, req *vpcv1alpha1pb.ListAllocationsRequest) (*vpcv1alpha1pb.ListAllocationsResponse, error) {
	if err := domain.CheckFilter(req.GetFilter()); err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	page, err := reply[domain.Page[*vpcpb.Allocation]](ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
	if err != nil {
		return nil, err
	}
	out := &vpcv1alpha1pb.ListAllocationsResponse{NextPageToken: page.NextPageToken}
	for _, a := range page.Items {
		out.Items = append(out.Items, alphaAllocation(a))
	}
	return out, nil
}

// Create starts an allocation creation operation.
//
//nolint:staticcheck // SA1019: common.v1alpha1.Operation is deprecated upstream, still returned by v1alpha1 APIs
func (h *AlphaAllocationHandler) Create(ctx context.Context, req *vpcv1alpha1pb.CreateAllocationRequest) (*commonv1alpha1pb.Operation, error) {
	return alphaOpReply(ctx)(h.svc.Create(ctx, domain.CreateCmd[*vpcpb.AllocationSpec]{
		Metadata: req.GetMetadata(), Spec: v1AllocationSpec(req.GetSpec()), Request: req,
	}))
}

// Update starts an allocation update operation; X-ResetMask selects fields to clear.
//
//nolint:staticcheck // SA1019: common.v1alpha1.Operation is deprecated upstream, still returned by v1alpha1 APIs
func (h *AlphaAllocationHandler) Update(ctx context.Context, req *vpcv1alpha1pb.UpdateAllocationRequest) (*commonv1alpha1pb.Operation, error) {
	// The reset mask names v1alpha1 fields. ipv4_private, ipv4_public, cidr,
	// subnet_id and pool_id are spelled the same in v1, so it is passed through.
	m, err := resetMask(ctx)
	if err != nil {
		return nil, err
	}
	return alphaOpReply(ctx)(h.svc.Update(ctx, domain.UpdateCmd[*vpcpb.AllocationSpec]{
		Metadata: req.GetMetadata(), Spec: v1AllocationSpec(req.GetSpec()), ResetMask: m, Request: req,
	}))
}

// Delete starts an allocation deletion operation.
//
//nolint:staticcheck // SA1019: common.v1alpha1.Operation is deprecated upstream, still returned by v1alpha1 APIs
func (h *AlphaAllocationHandler) Delete(ctx context.Context, req *vpcv1alpha1pb.DeleteAllocationRequest) (*commonv1alpha1pb.Operation, error) {
	return alphaOpReply(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

// AlphaScopeHandler implements nebius.vpc.v1alpha1.ScopeService (read-only).
type AlphaScopeHandler struct {
	vpcv1alpha1pb.UnimplementedScopeServiceServer
	svc *domain.ScopeService
}

var _ vpcv1alpha1pb.ScopeServiceServer = (*AlphaScopeHandler)(nil)

// NewAlphaScopeHandler returns a handler backed by svc.
func NewAlphaScopeHandler(svc *domain.ScopeService) *AlphaScopeHandler {
	return &AlphaScopeHandler{svc: svc}
}

// Get returns a scope by ID.
func (h *AlphaScopeHandler) Get(ctx context.Context, req *vpcv1alpha1pb.GetScopeRequest) (*vpcv1alpha1pb.Scope, error) {
	return reply[*vpcv1alpha1pb.Scope](ctx)(h.svc.Get(ctx, req.GetId()))
}

// GetByName returns a scope by parent and name.
func (h *AlphaScopeHandler) GetByName(ctx context.Context, req *vpcv1alpha1pb.GetScopeByNameRequest) (*vpcv1alpha1pb.Scope, error) {
	return reply[*vpcv1alpha1pb.Scope](ctx)(h.svc.GetByName(ctx, req.GetParentId(), req.GetName()))
}

// List returns one page of the scopes in a parent.
func (h *AlphaScopeHandler) List(ctx context.Context, req *vpcv1alpha1pb.ListScopesRequest) (*vpcv1alpha1pb.ListScopesResponse, error) {
	if err := domain.CheckFilter(req.GetFilter()); err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	page, err := reply[domain.Page[*vpcv1alpha1pb.Scope]](ctx)(h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken()))
	if err != nil {
		return nil, err
	}
	return &vpcv1alpha1pb.ListScopesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}
