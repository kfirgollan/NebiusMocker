// Package iam is the NebiusMocker module for Nebius IAM (cpl.iam).
//
// Implemented today: tenants (seeded) and nebius.iam.v1.GroupService.
// See docs/services/iam.md for the full surface and plan.
package iam

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/iam/api"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// Module is the IAM service module.
type Module struct{}

var _ mocker.Module = (*Module)(nil)

// New returns the IAM module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "iam" }

// Register seeds the default tenant and registers the IAM gRPC services.
func (*Module) Register(ctx context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	tenants := dal.NewTenantRepo(deps.Store)
	if err := domain.SeedTenant(ctx, tenants, deps.Clock, deps.Config.TenantID, "default"); err != nil {
		return err
	}
	groups, err := domain.NewGroupService(dal.NewGroupRepo(deps.Store), tenants, deps.Ops, deps.IDs, deps.Clock)
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	iampb.RegisterGroupServiceServer(reg, api.NewGroupHandler(groups))
	return nil
}
