// Package iam is the NebiusMocker module for Nebius IAM (cpl.iam).
//
// Implemented: seeded tenant and default groups, GroupService,
// GroupMembershipService, AccessPermitService, and the authorization bindings
// they define. See docs/services/iam.md.
package iam

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
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

type repos struct {
	tenants     *dal.TenantRepo
	groups      *dal.GroupRepo
	memberships *dal.MembershipRepo
	permits     *dal.PermitRepo
}

// Register seeds IAM data, installs authorization bindings and registers the
// IAM gRPC services.
func (*Module) Register(ctx context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	r := repos{
		tenants:     dal.NewTenantRepo(deps.Store),
		groups:      dal.NewGroupRepo(deps.Store),
		memberships: dal.NewMembershipRepo(deps.Store),
		permits:     dal.NewPermitRepo(deps.Store),
	}
	if err := seed(ctx, r, deps); err != nil {
		return err
	}
	registerAuthz(r, deps)

	groups, err := domain.NewGroupService(domain.GroupDeps{
		Groups: r.groups, Tenants: r.tenants, Memberships: r.memberships, Permits: r.permits,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	memberships, err := domain.NewMembershipService(r.memberships, r.groups, deps.Ops, deps.IDs, deps.Clock)
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	permits, err := domain.NewPermitService(domain.PermitDeps{
		Permits: r.permits, Groups: r.groups, Hierarchy: deps.Authz, Roles: deps.Authz.Catalog(),
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	iampb.RegisterGroupServiceServer(reg, api.NewGroupHandler(groups))
	iampb.RegisterGroupMembershipServiceServer(reg, api.NewMembershipHandler(memberships))
	iampb.RegisterAccessPermitServiceServer(reg, api.NewPermitHandler(permits))
	return nil
}

func seed(ctx context.Context, r repos, deps mocker.Deps) error {
	tenantID := deps.Config.TenantID
	if err := domain.SeedTenant(ctx, r.tenants, deps.Clock, tenantID, "default"); err != nil {
		return err
	}
	return domain.SeedAuthz(ctx, domain.AuthzRepos{Groups: r.groups, Memberships: r.memberships, Permits: r.permits},
		deps.IDs, deps.Clock, domain.AuthzSeed{
			TenantID: tenantID,
			Members: map[string][]string{
				"admins":  {config.DefaultDevPrincipal},
				"editors": {config.DefaultEditorPrincipal},
				"viewers": {config.DefaultViewerPrincipal},
			},
		})
}

// registerAuthz tells the engine how IAM resources nest and where roles come from.
func registerAuthz(r repos, deps mocker.Deps) {
	e := deps.Authz
	e.RegisterKind(domain.KindTenant, func(ctx context.Context, id string) (string, error) {
		_, err := r.tenants.Get(ctx, id)
		return "", err // tenants are roots
	})
	e.RegisterKind(domain.KindGroup, func(ctx context.Context, id string) (string, error) {
		g, err := r.groups.Get(ctx, id)
		return g.GetMetadata().GetParentId(), err
	})
	e.RegisterKind(domain.KindGroupMembership, func(ctx context.Context, id string) (string, error) {
		m, err := r.memberships.Get(ctx, id)
		return m.GetMetadata().GetParentId(), err
	})
	e.RegisterKind(domain.KindAccessPermit, func(ctx context.Context, id string) (string, error) {
		p, err := r.permits.Get(ctx, id)
		return p.GetMetadata().GetParentId(), err
	})
	e.SetBindings(domain.NewBindings(r.memberships, r.permits, deps.Clock))
	api.RegisterAuthzRules(deps.Rules, deps.Config.TenantID)
}
