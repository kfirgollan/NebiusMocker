// Package iam is the NebiusMocker module for Nebius IAM (cpl.iam).
//
// Implemented: seeded tenant/project and default groups, GroupService,
// GroupMembershipService, AccessPermitService (and the authorization bindings
// they define), ServiceAccountService and v2 AccessKeyService (the S3
// credentials Object Storage authenticates with). See docs/services/iam.md.
package iam

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
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
	projects    *dal.ProjectRepo
	groups      *dal.GroupRepo
	memberships *dal.MembershipRepo
	permits     *dal.PermitRepo
	sas         *dal.ServiceAccountRepo
	keys        *dal.AccessKeyRepo
}

// Register seeds IAM data, installs authorization bindings and registers the
// IAM gRPC services.
func (*Module) Register(ctx context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	r := repos{
		tenants:     dal.NewTenantRepo(deps.Store),
		projects:    dal.NewProjectRepo(deps.Store),
		groups:      dal.NewGroupRepo(deps.Store),
		memberships: dal.NewMembershipRepo(deps.Store),
		permits:     dal.NewPermitRepo(deps.Store),
		sas:         dal.NewServiceAccountRepo(deps.Store),
		keys:        dal.NewAccessKeyRepo(deps.Store),
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
	if err := registerCredentials(reg, r, deps); err != nil {
		return err
	}
	iampb.RegisterGroupServiceServer(reg, api.NewGroupHandler(groups))
	iampb.RegisterGroupMembershipServiceServer(reg, api.NewMembershipHandler(memberships))
	iampb.RegisterAccessPermitServiceServer(reg, api.NewPermitHandler(permits))
	iampb.RegisterProfileServiceServer(reg, api.NewProfileHandler(deps.Config.TenantID, deps.Config.ProjectID))
	api.RegisterProfileAuthzRule(deps.Rules, deps.Config.TenantID)
	iampb.RegisterProjectServiceServer(reg, api.NewProjectHandler(r.projects)) //nolint:staticcheck // SA1019: "nebius iam project get" (CLI 0.12) still calls the v1 ProjectService
	api.RegisterProjectAuthzRules(deps.Rules)
	return nil
}

// registerCredentials serves service accounts and access keys, and installs
// the access-key resolver S3 data planes authenticate with.
func registerCredentials(reg grpc.ServiceRegistrar, r repos, deps mocker.Deps) error {
	sas, err := domain.NewServiceAccountService(domain.ServiceAccountDeps{
		ServiceAccounts: r.sas, Projects: r.projects, AccessKeys: r.keys, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	keys, err := domain.NewAccessKeyService(domain.AccessKeyDeps{
		AccessKeys: r.keys, ServiceAccounts: r.sas, Projects: r.projects, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Peers: deps.Peers,
	})
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	if err := peer.Provide[peer.AccessKeys](deps.Peers, peer.AccessKeysKey, keys); err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	if err := peer.Provide[peer.GroupMembers](deps.Peers, peer.GroupMembersKey, domain.NewGroupMembers(r.memberships, deps.Clock)); err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	api.RegisterCredentialRules(deps.Rules, func(ctx context.Context, awsID string) (string, error) {
		k, err := r.keys.GetByAwsID(ctx, awsID)
		return k.GetMetadata().GetId(), err
	})
	iampb.RegisterServiceAccountServiceServer(reg, api.NewServiceAccountHandler(sas))
	iamv2pb.RegisterAccessKeyServiceServer(reg, api.NewAccessKeyHandler(keys))
	return nil
}

func seed(ctx context.Context, r repos, deps mocker.Deps) error {
	tenantID := deps.Config.TenantID
	if err := domain.SeedTenant(ctx, r.tenants, deps.Clock, tenantID, "default"); err != nil {
		return err
	}
	if err := domain.SeedProject(ctx, r.projects, deps.Clock, deps.Config.ProjectID, tenantID, "default"); err != nil {
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
	e.RegisterKind(domain.KindProject, func(ctx context.Context, id string) (string, error) {
		p, err := r.projects.Get(ctx, id)
		return p.GetMetadata().GetParentId(), err
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
	e.RegisterKind(domain.KindServiceAccount, func(ctx context.Context, id string) (string, error) {
		sa, err := r.sas.Get(ctx, id)
		return sa.GetMetadata().GetParentId(), err
	})
	e.RegisterKind(domain.KindAccessKey, func(ctx context.Context, id string) (string, error) {
		k, err := r.keys.Get(ctx, id)
		return k.GetMetadata().GetParentId(), err
	})
	e.SetBindings(domain.NewBindings(r.memberships, r.permits, deps.Clock))
	api.RegisterAuthzRules(deps.Rules, deps.Config.TenantID)
}
