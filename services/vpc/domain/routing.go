package domain

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// RouteTableService implements nebius.vpc.v1.RouteTableService.
type RouteTableService struct {
	*kind[*vpcpb.RouteTable, *vpcpb.RouteTableSpec]
}

func newRouteTableService(c *core) *RouteTableService {
	s := &RouteTableService{}
	s.kind = &kind[*vpcpb.RouteTable, *vpcpb.RouteTableSpec]{
		c: c, prefix: KindRouteTable, what: "route table", repo: c.r.RouteTables,
		newMsg:  func() *vpcpb.RouteTable { return &vpcpb.RouteTable{} },
		newSpec: func() *vpcpb.RouteTableSpec { return &vpcpb.RouteTableSpec{} },
		quota:   ownQuota[*vpcpb.RouteTable](quota.VPCRouteTableCount), build: s.build, apply: s.apply, checkDelete: s.checkDelete, remove: s.cascade, decorate: s.decorate,
	}
	return s
}

// ListByNetwork returns one page of the route tables of a network.
func (s *RouteTableService) ListByNetwork(ctx context.Context, networkID string, pageSize int64, pageToken string) (Page[*vpcpb.RouteTable], error) {
	return s.listWhere(ctx, func(rt *vpcpb.RouteTable) bool { return rt.GetSpec().GetNetworkId() == networkID }, pageSize, pageToken)
}

func (s *RouteTableService) build(ctx context.Context, md *commonpb.ResourceMetadata, spec *vpcpb.RouteTableSpec) (*vpcpb.RouteTable, error) {
	if err := s.c.checkProject(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	n, err := ref(ctx, s.c.r.Networks, "spec.network_id", spec.GetNetworkId())
	if err != nil {
		return nil, err
	}
	if err := sameParent("spec.network_id", md.GetParentId(), n); err != nil {
		return nil, err
	}
	return &vpcpb.RouteTable{Metadata: md, Spec: spec, Status: &vpcpb.RouteTableStatus{State: vpcpb.RouteTableStatus_READY}}, nil
}

func (s *RouteTableService) apply(_ context.Context, stored, updated *vpcpb.RouteTable) error {
	if stored.GetSpec().GetNetworkId() != updated.GetSpec().GetNetworkId() {
		return immutable("spec.network_id")
	}
	return nil
}

// assigned returns the subnets that route through rt.
func (s *RouteTableService) assigned(ctx context.Context, rt *vpcpb.RouteTable) ([]*vpcpb.Subnet, error) {
	id := rt.GetMetadata().GetId()
	return s.c.r.Subnets.All(ctx, func(sn *vpcpb.Subnet) bool {
		if sn.GetSpec().GetRouteTableId() == "" {
			return rt.GetStatus().GetDefault() && sn.GetSpec().GetNetworkId() == rt.GetSpec().GetNetworkId()
		}
		return sn.GetSpec().GetRouteTableId() == id
	})
}

func (s *RouteTableService) checkDelete(ctx context.Context, rt *vpcpb.RouteTable) error {
	id := rt.GetMetadata().GetId()
	if rt.GetStatus().GetDefault() {
		return errs.BadState(id, "the default route table is deleted with its network")
	}
	subs, err := s.assigned(ctx, rt)
	if err != nil {
		return err
	}
	if len(subs) > 0 {
		return inUse(id, "route table", idsOf(subs))
	}
	return nil
}

// cascade deletes the route table and its routes.
func (s *RouteTableService) cascade(ctx context.Context, rt *vpcpb.RouteTable) error {
	id := rt.GetMetadata().GetId()
	routes, err := s.c.r.Routes.All(ctx, func(r *vpcpb.Route) bool { return r.GetMetadata().GetParentId() == id })
	if err != nil {
		return err
	}
	for _, r := range routes {
		if err := s.c.r.Routes.Delete(ctx, r.GetMetadata().GetId()); err != nil {
			return fmt.Errorf("delete route %s: %w", r.GetMetadata().GetId(), err)
		}
	}
	return s.repo.Delete(ctx, id)
}

func (s *RouteTableService) decorate(ctx context.Context, rt *vpcpb.RouteTable) error {
	subs, err := s.assigned(ctx, rt)
	if err != nil {
		return err
	}
	if rt.GetStatus() == nil {
		rt.Status = &vpcpb.RouteTableStatus{State: vpcpb.RouteTableStatus_READY}
	}
	rt.Status.Assignment = &vpcpb.RouteTableAssignment{Subnets: idsOf(subs)}
	return nil
}

// RouteService implements nebius.vpc.v1.RouteService.
type RouteService struct {
	*kind[*vpcpb.Route, *vpcpb.RouteSpec]
}

func newRouteService(c *core) *RouteService {
	s := &RouteService{}
	s.kind = &kind[*vpcpb.Route, *vpcpb.RouteSpec]{
		c: c, prefix: KindRoute, what: "route", repo: c.r.Routes,
		newMsg: func() *vpcpb.Route { return &vpcpb.Route{} }, newSpec: func() *vpcpb.RouteSpec { return &vpcpb.RouteSpec{} },
		quota: routeQuota(c.r.RouteTables), build: s.build, apply: s.apply,
	}
	return s
}

const defaultRouteCIDR = "0.0.0.0/0"

func (s *RouteService) build(ctx context.Context, md *commonpb.ResourceMetadata, spec *vpcpb.RouteSpec) (*vpcpb.Route, error) {
	rt, err := ref(ctx, s.c.r.RouteTables, "metadata.parent_id", md.GetParentId())
	if err != nil {
		return nil, err
	}
	dst, err := parseCIDR(spec.GetDestination().GetCidr())
	if err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.destination.cidr", Message: err.Error()})
	}
	dup, err := s.repo.All(ctx, func(r *vpcpb.Route) bool {
		return r.GetMetadata().GetParentId() == md.GetParentId() && r.GetSpec().GetDestination().GetCidr() == dst.String()
	})
	if err != nil {
		return nil, err
	}
	if len(dup) > 0 {
		return nil, errs.AlreadyExists(dup[0].GetMetadata().GetId(), fmt.Sprintf("route table already has a route to %s", dst))
	}
	hop, err := s.nextHop(ctx, rt, dst.String(), spec.GetNextHop())
	if err != nil {
		return nil, err
	}
	return &vpcpb.Route{Metadata: md, Spec: spec, Status: &vpcpb.RouteStatus{
		State: vpcpb.RouteStatus_READY, NextHop: hop, Type: vpcpb.RouteStatus_STATIC,
	}}, nil
}

// nextHop validates a next hop and returns its status.
func (s *RouteService) nextHop(ctx context.Context, rt *vpcpb.RouteTable, dst string, hop *vpcpb.NextHop) (*vpcpb.NextHopState, error) {
	switch {
	case hop.GetAllocation() != nil:
		a, err := ref(ctx, s.c.r.Allocations, "spec.next_hop.allocation.id", hop.GetAllocation().GetId())
		if err != nil {
			return nil, err
		}
		in, err := s.c.inNetwork(ctx, a, rt.GetSpec().GetNetworkId())
		if err != nil {
			return nil, err
		}
		if !in {
			return nil, errs.BadRequest(errs.Violation{
				Field: "spec.next_hop.allocation.id", Message: "the allocation must be in the route table's network (a subnet of it, or one of its private pools)",
			})
		}
		return &vpcpb.NextHopState{NextHop: &vpcpb.NextHopState_Allocation{
			Allocation: &vpcpb.AllocationNextHopState{Cidr: a.GetStatus().GetDetails().GetAllocatedCidr()},
		}}, nil
	case hop.GetDefaultEgressGateway():
		if dst != defaultRouteCIDR {
			return nil, errs.BadRequest(errs.Violation{Field: "spec.next_hop.default_egress_gateway", Message: "only valid with destination 0.0.0.0/0"})
		}
		return &vpcpb.NextHopState{NextHop: &vpcpb.NextHopState_DefaultEgressGateway{DefaultEgressGateway: &vpcpb.DefaultEgressGatewayState{}}}, nil
	default:
		return nil, errs.BadRequest(errs.Violation{Field: "spec.next_hop", Message: "one of allocation or default_egress_gateway=true is required"})
	}
}

func (s *RouteService) apply(_ context.Context, stored, updated *vpcpb.Route) error {
	if !proto.Equal(stored.GetSpec().GetDestination(), updated.GetSpec().GetDestination()) {
		return immutable("spec.destination")
	}
	if !proto.Equal(stored.GetSpec().GetNextHop(), updated.GetSpec().GetNextHop()) {
		return immutable("spec.next_hop")
	}
	return nil
}

// inNetwork reports whether an allocation belongs to a network: through its
// subnet, or (for an allocation from a pool, with no subnet) through one of the
// network's private pools. A public address outside a subnet is not in it.
func (c *core) inNetwork(ctx context.Context, a *vpcpb.Allocation, networkID string) (bool, error) {
	if id := a.GetStatus().GetDetails().GetSubnetId(); id != "" {
		sn, err := c.r.Subnets.Get(ctx, id)
		switch {
		case errs.IsKind(err, errs.KindNotFound):
			return false, nil
		case err != nil:
			return false, err
		}
		return sn.GetSpec().GetNetworkId() == networkID, nil
	}
	n, err := c.r.Networks.Get(ctx, networkID)
	if err != nil {
		return false, err
	}
	return slices.Contains(networkPoolIDs(n, vpcpb.IpVisibility_PRIVATE), a.GetStatus().GetDetails().GetPoolId()), nil
}
