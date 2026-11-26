package iam

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/iam/api"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// registerUsers seeds the tenant user accounts of the static principals and
// serves TenantUserAccountService, TenantUserAccountWithAttributesService and
// InvitationService, plus the mock endpoint that accepts an invitation.
func registerUsers(ctx context.Context, reg grpc.ServiceRegistrar, r repos, deps mocker.Deps) error {
	e := deps.Authz
	e.RegisterKind(domain.KindTenantUserAccount, func(ctx context.Context, id string) (string, error) {
		a, err := r.users.Get(ctx, id)
		return a.GetMetadata().GetParentId(), err
	})
	e.RegisterKind(domain.KindInvitation, func(ctx context.Context, id string) (string, error) {
		inv, err := r.invitations.Get(ctx, id)
		return inv.GetMetadata().GetParentId(), err
	})

	// Sorted so seeding is deterministic.
	principals := make([]string, 0, len(deps.Config.Tokens))
	for _, p := range deps.Config.Tokens {
		if strings.HasPrefix(p, domain.KindTenantUserAccount+"-") && !slices.Contains(principals, p) {
			principals = append(principals, p)
		}
	}
	slices.Sort(principals)
	for _, p := range principals {
		if err := domain.SeedTenantUserAccount(ctx, r.users, deps.Clock, deps.Config.TenantID, p); err != nil {
			return err
		}
	}

	members, err := domain.NewMemberDirectory(r.users, r.users, r.invitations, r.sas, deps.Clock)
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	accounts, err := domain.NewTenantUserAccountService(r.users, members, deps.Ops, deps.Clock)
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	invitations, err := domain.NewInvitationService(domain.InvitationDeps{
		Invitations: r.invitations, Users: r.users, Attributes: r.users, Memberships: r.memberships,
		Tenants: r.tenants, Federations: dal.NewFederationRepo(deps.Store),
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	h := api.NewInvitationHandler(invitations, deps.Log)
	iampb.RegisterTenantUserAccountServiceServer(reg, api.NewTenantUserAccountHandler(accounts))
	iampb.RegisterTenantUserAccountWithAttributesServiceServer(reg, api.NewTenantUserAccountWithAttributesHandler(accounts))
	iampb.RegisterInvitationServiceServer(reg, h)
	api.RegisterTenantUserAccountRules(deps.Rules)
	api.RegisterInvitationRules(deps.Rules)
	if deps.HTTP != nil {
		deps.HTTP.HandleHTTP(api.AcceptInvitationPattern, h)
	}
	return nil
}
