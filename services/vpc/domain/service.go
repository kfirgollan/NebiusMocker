package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Repos are the VPC repositories, one per kind.
type Repos struct {
	Pools          Repository[*vpcpb.Pool]
	Networks       Repository[*vpcpb.Network]
	Subnets        Repository[*vpcpb.Subnet]
	Allocations    Repository[*vpcpb.Allocation]
	RouteTables    Repository[*vpcpb.RouteTable]
	Routes         Repository[*vpcpb.Route]
	SecurityGroups Repository[*vpcpb.SecurityGroup]
	SecurityRules  Repository[*vpcpb.SecurityRule]
	Scopes         Repository[*vpcv1alpha1pb.Scope]
	NetworkUsers   NetworkUserRepository
}

// Config holds the seeded address space.
type Config struct {
	// ProjectID is the seeded project; it always exists, even without IAM.
	ProjectID string
	// DefaultPrivateCIDR is the CIDR of each project's default-network-pool.
	DefaultPrivateCIDR string
	// PublicCIDR is the CIDR of the default-public-pool.
	PublicCIDR string
}

// Deps are the dependencies of a Service. All are required.
type Deps struct {
	Repos     Repos
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Hierarchy Hierarchy
	Quota     QuotaGate
	Config    Config
}

// core is the state shared by every kind. mu serializes all VPC mutations, so
// IPAM decisions (lowest free block, overlap checks) are atomic.
type core struct {
	mu   sync.Mutex
	ops  Operations
	ids  IDGenerator
	clk  clock.Clock
	hier Hierarchy
	gate QuotaGate
	cfg  Config
	r    Repos

	pools          *PoolService
	networks       *NetworkService
	subnets        *SubnetService
	allocations    *AllocationService
	routeTables    *RouteTableService
	routes         *RouteService
	securityGroups *SecurityGroupService
	securityRules  *SecurityRuleService
	scopes         *ScopeService
}

// Service is the VPC domain: one sub-service per resource kind.
type Service struct{ c *core }

// New wires the VPC domain.
func New(d Deps) (*Service, error) {
	r := d.Repos
	if d.Ops == nil || d.IDs == nil || d.Clock == nil || d.Hierarchy == nil || d.Quota == nil ||
		r.Pools == nil || r.Networks == nil || r.Subnets == nil || r.Allocations == nil ||
		r.RouteTables == nil || r.Routes == nil || r.SecurityGroups == nil || r.SecurityRules == nil || r.Scopes == nil || r.NetworkUsers == nil {
		return nil, errors.New("vpc: service dependencies must not be nil")
	}
	c := &core{ops: d.Ops, ids: d.IDs, clk: d.Clock, hier: d.Hierarchy, gate: d.Quota, cfg: d.Config, r: r}
	c.pools = newPoolService(c)
	c.networks = newNetworkService(c)
	c.subnets = newSubnetService(c)
	c.allocations = newAllocationService(c)
	c.routeTables = newRouteTableService(c)
	c.routes = newRouteService(c)
	c.securityGroups = newSecurityGroupService(c)
	c.securityRules = newSecurityRuleService(c)
	c.scopes = newScopeService(c)
	return &Service{c: c}, nil
}

// Pools returns the pool service.
func (s *Service) Pools() *PoolService { return s.c.pools }

// Networks returns the network service.
func (s *Service) Networks() *NetworkService { return s.c.networks }

// Subnets returns the subnet service.
func (s *Service) Subnets() *SubnetService { return s.c.subnets }

// Allocations returns the allocation service.
func (s *Service) Allocations() *AllocationService { return s.c.allocations }

// RouteTables returns the route table service.
func (s *Service) RouteTables() *RouteTableService { return s.c.routeTables }

// Routes returns the route service.
func (s *Service) Routes() *RouteService { return s.c.routes }

// SecurityGroups returns the security group service.
func (s *Service) SecurityGroups() *SecurityGroupService { return s.c.securityGroups }

// SecurityRules returns the security rule service.
func (s *Service) SecurityRules() *SecurityRuleService { return s.c.securityRules }

// Scopes returns the (v1alpha1, read-only) scope service.
func (s *Service) Scopes() *ScopeService { return s.c.scopes }

// checkProject verifies that id is an existing project.
func (c *core) checkProject(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, KindProject+"-") {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a project ID"})
	}
	if id == c.cfg.ProjectID {
		return nil
	}
	if _, err := c.hier.Parent(ctx, id); err != nil {
		return fmt.Errorf("parent project %s: %w", id, err)
	}
	return nil
}

// sameParent requires a referenced resource to live in the same project.
func sameParent(field, parentID string, ref Resource) error {
	if ref.GetMetadata().GetParentId() != parentID {
		return errs.BadRequest(errs.Violation{
			Field:   field,
			Message: fmt.Sprintf("%s belongs to %s, not %s", ref.GetMetadata().GetId(), ref.GetMetadata().GetParentId(), parentID),
		})
	}
	return nil
}

// ref loads a resource referenced by field (errs.NotFound when it is missing).
func ref[T Resource](ctx context.Context, repo Repository[T], field, id string) (T, error) {
	if id == "" {
		var zero T
		return zero, errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	}
	r, err := repo.Get(ctx, id)
	if err != nil {
		return r, fmt.Errorf("%s: %w", field, err)
	}
	return r, nil
}
