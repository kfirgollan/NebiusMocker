// Package capacity is the NebiusMocker module for Nebius capacity
// (nebius.capacity.v1): capacity block groups (GPU reservations) and their
// intervals, seeded from a catalog (NEBIUS_MOCKER_CAPACITY_BLOCK_GROUPS_FILE);
// capacity allowances (project limits); resource advice; and the
// peer.CapacityReservations capability compute consumers draw from. See
// docs/services/capacity.md.
package capacity

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/capacity/api"
	"github.com/kfirgollan/NebiusMocker/services/capacity/dal"
	"github.com/kfirgollan/NebiusMocker/services/capacity/domain"
)

// Module is the capacity service module.
type Module struct{}

var _ mocker.Module = (*Module)(nil)

// New returns the capacity module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "capacity" }

// Register wires the capacity services, their authorization and the
// reservations capability.
func (*Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	catalog, err := loadCatalog(deps.Config.Capacity.BlockGroupsFile)
	if err != nil {
		return fmt.Errorf("capacity: %w", err)
	}
	routing := map[string]string{}
	for _, r := range deps.Config.Regions {
		routing[r.Name] = r.RoutingCode
	}
	inv, err := catalog.Build(deps.Clock.Now(), deps.Config.TenantID, routing)
	if err != nil {
		return fmt.Errorf("capacity: %w", err)
	}
	allowances := dal.NewAllowanceRepo(deps.Store)
	svc, err := domain.New(domain.Deps{
		Inventory: inv, Allowances: allowances, Usage: dal.NewUsageRepo(deps.Store), Hierarchy: deps.Authz,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Config: domain.Config{TenantID: deps.Config.TenantID, ProjectID: deps.Config.ProjectID},
	})
	if err != nil {
		return fmt.Errorf("capacity: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindBlockGroup, func(_ context.Context, id string) (string, error) {
		g, ok := inv.Group(id)
		if !ok {
			return "", errs.NotFound(id)
		}
		return g.TenantID, nil
	})
	deps.Authz.RegisterKind(domain.KindInterval, func(_ context.Context, id string) (string, error) {
		iv, ok := inv.Interval(id)
		if !ok {
			return "", errs.NotFound(id)
		}
		return iv.BlockGroupID, nil
	})
	deps.Authz.RegisterKind(domain.KindAllowance, func(ctx context.Context, id string) (string, error) {
		a, err := allowances.Get(ctx, id)
		return a.GetMetadata().GetParentId(), err
	})
	if err := peer.Provide(deps.Peers, peer.CapacityReservationsKey, peer.CapacityReservations(svc)); err != nil {
		return fmt.Errorf("capacity: %w", err)
	}
	api.RegisterAuthzRules(deps.Rules)
	capacitypb.RegisterCapacityAllowanceServiceServer(reg, api.NewAllowanceHandler(svc))
	capacitypb.RegisterCapacityBlockGroupServiceServer(reg, api.NewBlockGroupHandler(svc))
	capacitypb.RegisterCapacityIntervalServiceServer(reg, api.NewIntervalHandler(svc))
	capacitypb.RegisterResourceAdviceServiceServer(reg, api.NewAdviceHandler(svc))
	return nil
}

// loadCatalog returns the built-in catalog, or the one in path when set.
func loadCatalog(path string) (domain.Catalog, error) {
	if path == "" {
		return domain.DefaultCatalog()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Catalog{}, fmt.Errorf("read capacity catalog: %w", err)
	}
	return domain.ParseCatalog(data)
}
