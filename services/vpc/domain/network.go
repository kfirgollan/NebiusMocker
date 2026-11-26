package domain

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Names of the default resources.
const (
	DefaultNetworkName = "default-network"
	DefaultSubnetName  = "default-subnet"
	// Every network gets a default route table and security group named
	// "<prefix><network unique part>".
	DefaultRouteTablePrefix    = "default-route-table-"
	DefaultSecurityGroupPrefix = "default-security-group-"
)

// NetworkService implements nebius.vpc.v1.NetworkService.
type NetworkService struct {
	*kind[*vpcpb.Network, *vpcpb.NetworkSpec]
}

func newNetworkService(c *core) *NetworkService {
	s := &NetworkService{}
	s.kind = &kind[*vpcpb.Network, *vpcpb.NetworkSpec]{
		c: c, prefix: KindNetwork, what: "network", repo: c.r.Networks,
		newMsg: func() *vpcpb.Network { return &vpcpb.Network{} }, newSpec: func() *vpcpb.NetworkSpec { return &vpcpb.NetworkSpec{} },
		build: s.build, afterCreate: s.createDefaults, apply: s.apply, checkDelete: s.checkDelete, remove: s.remove,
	}
	return s
}

// DefaultNetwork is the default network of a project and its default subnet.
type DefaultNetwork struct {
	Network *vpcpb.Network
	Subnet  *vpcpb.Subnet
}

// GetDefault returns the project's default network and its default subnet.
func (s *NetworkService) GetDefault(ctx context.Context, projectID string) (DefaultNetwork, error) {
	nets, err := s.repo.All(ctx, func(n *vpcpb.Network) bool {
		return n.GetMetadata().GetParentId() == projectID && n.GetStatus().GetDefault()
	})
	if err != nil {
		return DefaultNetwork{}, err
	}
	if len(nets) == 0 {
		return DefaultNetwork{}, errs.NotFound(projectID + "/default-network")
	}
	n := nets[0]
	subs, err := s.c.r.Subnets.All(ctx, func(sn *vpcpb.Subnet) bool {
		return sn.GetSpec().GetNetworkId() == n.GetMetadata().GetId() && sn.GetStatus().GetDefault()
	})
	if err != nil {
		return DefaultNetwork{}, err
	}
	out := DefaultNetwork{Network: n}
	if len(subs) > 0 {
		out.Subnet, err = s.c.subnets.Get(ctx, subs[0].GetMetadata().GetId())
	}
	return out, err
}

// CreateDefault creates the project's default network and subnet.
func (s *NetworkService) CreateDefault(ctx context.Context, md *commonpb.ResourceMetadata, request proto.Message) (*operations.Operation, error) {
	if md.GetParentId() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	id, err := s.c.ids.New(KindNetwork)
	if err != nil {
		return nil, err
	}
	return s.c.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create default network",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.c.mu.Lock()
			defer s.c.mu.Unlock()
			if err := s.c.checkProject(ctx, md.GetParentId()); err != nil {
				return err
			}
			if d, err := s.GetDefault(ctx, md.GetParentId()); err == nil {
				return errs.AlreadyExists(d.Network.GetMetadata().GetId(), "the project already has a default network")
			}
			return s.createDefaultLocked(ctx, s.c.metadata(id, md.GetParentId(), md.GetName(), md.GetLabels()))
		},
	})
}

// createDefaultLocked creates a default network with its default subnet; core.mu must be held.
func (s *NetworkService) createDefaultLocked(ctx context.Context, md *commonpb.ResourceMetadata) error {
	if err := s.c.pools.seedPool(ctx, md.GetParentId(), DefaultPrivatePoolName, s.c.cfg.DefaultPrivateCIDR, vpcpb.IpVisibility_PRIVATE); err != nil {
		return fmt.Errorf("seed private pool: %w", err)
	}
	if err := s.c.pools.seedPool(ctx, md.GetParentId(), DefaultPublicPoolName, s.c.cfg.PublicCIDR, vpcpb.IpVisibility_PUBLIC); err != nil {
		return fmt.Errorf("seed public pool: %w", err)
	}
	n, err := s.createLocked(ctx, md, &vpcpb.NetworkSpec{})
	if err != nil {
		return err
	}
	n.Status.Default = true
	if err := s.repo.Update(ctx, n); err != nil {
		return err
	}
	subnetID, err := s.c.ids.New(KindSubnet)
	if err != nil {
		return err
	}
	sn, err := s.c.subnets.createLocked(ctx, s.c.metadata(subnetID, md.GetParentId(), DefaultSubnetName, nil),
		&vpcpb.SubnetSpec{NetworkId: n.GetMetadata().GetId()})
	if err != nil {
		return err
	}
	sn.Status.Default = true
	return s.c.r.Subnets.Update(ctx, sn)
}

// SeedDefault creates the project's default pools, network and subnet unless
// they exist. It runs at startup, outside any operation.
func (s *NetworkService) SeedDefault(ctx context.Context, projectID string) error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if _, err := s.GetDefault(ctx, projectID); err == nil {
		return nil
	}
	id, err := s.c.ids.New(KindNetwork)
	if err != nil {
		return err
	}
	return s.createDefaultLocked(ctx, s.c.metadata(id, projectID, DefaultNetworkName, nil))
}

func (s *NetworkService) build(ctx context.Context, md *commonpb.ResourceMetadata, spec *vpcpb.NetworkSpec) (*vpcpb.Network, error) {
	if err := s.c.checkProject(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	n := &vpcpb.Network{Metadata: md, Spec: spec, Status: &vpcpb.NetworkStatus{State: vpcpb.NetworkStatus_READY}}
	if err := s.normalize(ctx, n); err != nil {
		return nil, err
	}
	return n, nil
}

// normalize fills default pools and validates the pool references.
func (s *NetworkService) normalize(ctx context.Context, n *vpcpb.Network) error {
	parent := n.GetMetadata().GetParentId()
	if len(n.GetSpec().GetIpv4PrivatePools().GetPools()) == 0 {
		p, err := s.c.r.Pools.GetByName(ctx, parent, DefaultPrivatePoolName)
		if err != nil {
			return fmt.Errorf("default private pool of %s: %w", parent, err)
		}
		n.Spec.Ipv4PrivatePools = &vpcpb.IPv4PrivateNetworkPools{Pools: []*vpcpb.NetworkPool{{Id: p.GetMetadata().GetId()}}}
	}
	if len(n.GetSpec().GetIpv4PublicPools().GetPools()) == 0 {
		p, err := s.c.r.Pools.GetByName(ctx, parent, DefaultPublicPoolName)
		if err != nil {
			return fmt.Errorf("default public pool of %s: %w", parent, err)
		}
		n.Spec.Ipv4PublicPools = &vpcpb.IPv4PublicNetworkPools{Pools: []*vpcpb.NetworkPool{{Id: p.GetMetadata().GetId()}}}
	}
	if err := s.checkPools(ctx, n, "spec.ipv4_private_pools", vpcpb.IpVisibility_PRIVATE); err != nil {
		return err
	}
	return s.checkPools(ctx, n, "spec.ipv4_public_pools", vpcpb.IpVisibility_PUBLIC)
}

func (s *NetworkService) checkPools(ctx context.Context, n *vpcpb.Network, field string, vis vpcpb.IpVisibility) error {
	ids := networkPoolIDs(n, vis)
	pools, err := s.c.loadPools(ctx, field, ids)
	if err != nil {
		return err
	}
	for i, p := range pools {
		f := fmt.Sprintf("%s.pools[%d].id", field, i)
		if slices.Index(ids, ids[i]) != i {
			return errs.BadRequest(errs.Violation{Field: f, Message: "duplicate pool"})
		}
		if p.GetSpec().GetVisibility() != vis {
			return errs.BadRequest(errs.Violation{Field: f, Message: fmt.Sprintf("pool %s is not %s", ids[i], vis)})
		}
		if err := sameParent(f, n.GetMetadata().GetParentId(), p); err != nil {
			return err
		}
	}
	var all []block
	for _, p := range pools {
		all = append(all, poolBlocks(p)...)
	}
	if a, b, bad := anyOverlap(prefixes(all)); bad {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("pools overlap: %s and %s", a, b)})
	}
	return nil
}

// createDefaults creates the network's default route table (with a default
// egress route) and default security group.
func (s *NetworkService) createDefaults(ctx context.Context, n *vpcpb.Network) error {
	netID := n.GetMetadata().GetId()
	_, suffix, _ := strings.Cut(netID, "-")
	rtID, err := s.c.ids.New(KindRouteTable)
	if err != nil {
		return err
	}
	rt, err := s.c.routeTables.createLocked(ctx, s.c.metadata(rtID, n.GetMetadata().GetParentId(), DefaultRouteTablePrefix+suffix, nil),
		&vpcpb.RouteTableSpec{NetworkId: netID})
	if err != nil {
		return fmt.Errorf("default route table: %w", err)
	}
	rt.Status.Default = true
	if err := s.c.r.RouteTables.Update(ctx, rt); err != nil {
		return err
	}
	routeID, err := s.c.ids.New(KindRoute)
	if err != nil {
		return err
	}
	if _, err := s.c.routes.createLocked(ctx, s.c.metadata(routeID, rtID, "default-egress", nil), &vpcpb.RouteSpec{
		Description: "Default egress to the internet",
		Destination: &vpcpb.DestinationMatch{Cidr: "0.0.0.0/0"},
		NextHop:     &vpcpb.NextHop{NextHop: &vpcpb.NextHop_DefaultEgressGateway{DefaultEgressGateway: true}},
	}); err != nil {
		return fmt.Errorf("default route: %w", err)
	}
	sgID, err := s.c.ids.New(KindSecurityGroup)
	if err != nil {
		return err
	}
	sg, err := s.c.securityGroups.createLocked(ctx, s.c.metadata(sgID, n.GetMetadata().GetParentId(), DefaultSecurityGroupPrefix+suffix, nil),
		&vpcpb.SecurityGroupSpec{NetworkId: netID})
	if err != nil {
		return fmt.Errorf("default security group: %w", err)
	}
	sg.Status.Default = true
	if err := s.c.r.SecurityGroups.Update(ctx, sg); err != nil {
		return err
	}
	n.Status.DefaultRouteTableId = rtID
	return s.repo.Update(ctx, n)
}

func (s *NetworkService) apply(ctx context.Context, stored, updated *vpcpb.Network) error {
	if err := s.normalize(ctx, updated); err != nil {
		return err
	}
	for _, vis := range []vpcpb.IpVisibility{vpcpb.IpVisibility_PRIVATE, vpcpb.IpVisibility_PUBLIC} {
		kept := networkPoolIDs(updated, vis)
		for _, id := range networkPoolIDs(stored, vis) {
			if slices.Contains(kept, id) {
				continue
			}
			users, err := s.poolUsers(ctx, stored.GetMetadata().GetId(), id)
			if err != nil {
				return err
			}
			if len(users) > 0 {
				return errs.BadState(stored.GetMetadata().GetId(), fmt.Sprintf(
					"cannot remove pool %s from the network: used by %s", id, strings.Join(users, ", ")))
			}
		}
	}
	return nil
}

// poolUsers lists the network's subnets and allocations that draw from poolID.
func (s *NetworkService) poolUsers(ctx context.Context, networkID, poolID string) ([]string, error) {
	subs, err := s.c.r.Subnets.All(ctx, func(sn *vpcpb.Subnet) bool {
		_, ok := carved(sn)[poolID]
		return ok && sn.GetSpec().GetNetworkId() == networkID
	})
	if err != nil {
		return nil, err
	}
	inNet := map[string]bool{}
	all, err := s.c.r.Subnets.All(ctx, func(sn *vpcpb.Subnet) bool { return sn.GetSpec().GetNetworkId() == networkID })
	if err != nil {
		return nil, err
	}
	for _, sn := range all {
		inNet[sn.GetMetadata().GetId()] = true
	}
	allocs, err := s.c.r.Allocations.All(ctx, func(a *vpcpb.Allocation) bool {
		d := a.GetStatus().GetDetails()
		return d.GetPoolId() == poolID && inNet[d.GetSubnetId()]
	})
	if err != nil {
		return nil, err
	}
	return append(idsOf(subs), idsOf(allocs)...), nil
}

func (s *NetworkService) checkDelete(ctx context.Context, n *vpcpb.Network) error {
	id := n.GetMetadata().GetId()
	onNet := func(networkID string) bool { return networkID == id }
	subs, err := s.c.r.Subnets.All(ctx, func(sn *vpcpb.Subnet) bool { return onNet(sn.GetSpec().GetNetworkId()) })
	if err != nil {
		return err
	}
	sgs, err := s.c.r.SecurityGroups.All(ctx, func(sg *vpcpb.SecurityGroup) bool {
		return onNet(sg.GetSpec().GetNetworkId()) && !sg.GetStatus().GetDefault()
	})
	if err != nil {
		return err
	}
	rts, err := s.c.r.RouteTables.All(ctx, func(rt *vpcpb.RouteTable) bool {
		return onNet(rt.GetSpec().GetNetworkId()) && !rt.GetStatus().GetDefault()
	})
	if err != nil {
		return err
	}
	bound, err := s.c.r.NetworkUsers.List(ctx, id)
	if err != nil {
		return err
	}
	if users := slices.Concat(idsOf(subs), idsOf(sgs), idsOf(rts), bound); len(users) > 0 {
		return inUse(id, "network", users)
	}
	return nil
}

// remove deletes the network with its default route table and security group.
func (s *NetworkService) remove(ctx context.Context, n *vpcpb.Network) error {
	id := n.GetMetadata().GetId()
	rts, err := s.c.r.RouteTables.All(ctx, func(rt *vpcpb.RouteTable) bool { return rt.GetSpec().GetNetworkId() == id })
	if err != nil {
		return err
	}
	for _, rt := range rts {
		if err := s.c.routeTables.cascade(ctx, rt); err != nil {
			return err
		}
	}
	sgs, err := s.c.r.SecurityGroups.All(ctx, func(sg *vpcpb.SecurityGroup) bool { return sg.GetSpec().GetNetworkId() == id })
	if err != nil {
		return err
	}
	for _, sg := range sgs {
		if err := s.c.securityGroups.cascade(ctx, sg); err != nil {
			return err
		}
	}
	return s.repo.Delete(ctx, id)
}
