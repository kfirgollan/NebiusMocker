// Package vpc is the NebiusMocker module for Nebius VPC (nebius.vpc.v1).
//
// Implemented: pools, networks (with the seeded default network), subnets,
// allocations, route tables and routes, security groups and rules. IPAM is
// real (lowest-free placement, overlap and reference checks); security rules
// and routes are stored and validated, not enforced. See docs/services/vpc.md.
package vpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/vpc/api"
	"github.com/kfirgollan/NebiusMocker/services/vpc/dal"
	"github.com/kfirgollan/NebiusMocker/services/vpc/domain"
)

// Module is the VPC service module.
type Module struct{}

var _ mocker.Module = (*Module)(nil)

// New returns the VPC module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "vpc" }

// Register seeds the default network, registers resource kinds and
// authorization rules, and registers the VPC gRPC services.
func (*Module) Register(ctx context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	repos := dal.NewRepos(deps.Store)
	svc, err := domain.New(domain.Deps{
		Repos: repos, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Hierarchy: deps.Authz,
		Config: domain.Config{
			ProjectID:          deps.Config.ProjectID,
			DefaultPrivateCIDR: deps.Config.VPC.DefaultPrivateCIDR,
			PublicCIDR:         deps.Config.VPC.PublicCIDR,
		},
	})
	if err != nil {
		return fmt.Errorf("vpc: %w", err)
	}
	if err := svc.Networks().SeedDefault(ctx, deps.Config.ProjectID); err != nil {
		return fmt.Errorf("vpc: seed default network: %w", err)
	}
	registerKinds(deps, repos)
	api.RegisterAuthzRules(deps.Rules)

	vpcpb.RegisterPoolServiceServer(reg, api.NewPoolHandler(svc.Pools()))
	vpcpb.RegisterNetworkServiceServer(reg, api.NewNetworkHandler(svc.Networks()))
	vpcpb.RegisterSubnetServiceServer(reg, api.NewSubnetHandler(svc.Subnets()))
	vpcpb.RegisterAllocationServiceServer(reg, api.NewAllocationHandler(svc.Allocations()))
	vpcpb.RegisterRouteTableServiceServer(reg, api.NewRouteTableHandler(svc.RouteTables()))
	vpcpb.RegisterRouteServiceServer(reg, api.NewRouteHandler(svc.Routes()))
	vpcpb.RegisterSecurityGroupServiceServer(reg, api.NewSecurityGroupHandler(svc.SecurityGroups()))
	vpcpb.RegisterSecurityRuleServiceServer(reg, api.NewSecurityRuleHandler(svc.SecurityRules()))
	return nil
}

// registerKinds tells the authorization engine how VPC resources nest.
func registerKinds(deps mocker.Deps, r domain.Repos) {
	e := deps.Authz
	e.RegisterKind(domain.KindPool, parentOf(r.Pools))
	e.RegisterKind(domain.KindNetwork, parentOf(r.Networks))
	e.RegisterKind(domain.KindSubnet, parentOf(r.Subnets))
	e.RegisterKind(domain.KindAllocation, parentOf(r.Allocations))
	e.RegisterKind(domain.KindRouteTable, parentOf(r.RouteTables))
	e.RegisterKind(domain.KindRoute, parentOf(r.Routes))
	e.RegisterKind(domain.KindSecurityGroup, parentOf(r.SecurityGroups))
	e.RegisterKind(domain.KindSecurityRule, parentOf(r.SecurityRules))
}

func parentOf[T domain.Resource](repo domain.Repository[T]) func(context.Context, string) (string, error) {
	return func(ctx context.Context, id string) (string, error) {
		res, err := repo.Get(ctx, id)
		if err != nil {
			return "", err
		}
		return res.GetMetadata().GetParentId(), nil
	}
}
