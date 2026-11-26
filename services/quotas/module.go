// Package quotas is the NebiusMocker module for Nebius quota allowances
// (nebius.quotas.v1.QuotaAllowanceService).
//
// Every tenant and project has an allowance of each catalog quota (pkg/quota)
// per region. status.usage is computed live from the usage sources the other
// areas provide (peer.QuotaUsageKey), and the module provides the quota gate
// (peer.QuotaGateKey) those areas pass their creates through, so exceeding an
// allowance fails with RESOURCE_EXHAUSTED and a QuotaFailure detail. See
// docs/services/quotas.md.
package quotas

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/services/quotas/api"
	"github.com/kfirgollan/NebiusMocker/services/quotas/dal"
	"github.com/kfirgollan/NebiusMocker/services/quotas/domain"
)

// Module is the quotas service module.
type Module struct{}

var _ mocker.Module = (*Module)(nil)

// New returns the quotas module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "quotas" }

// Register builds the module, provides the quota gate and registers the
// gRPC service and its authz rules.
func (*Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	catalog, err := quota.DefaultCatalog(deps.Config.Quotas.Limits)
	if err != nil {
		return fmt.Errorf("quotas: NEBIUS_MOCKER_QUOTAS_LIMITS: %w", err)
	}
	regions := make([]domain.Region, 0, len(deps.Config.Regions))
	for _, r := range deps.Config.Regions {
		regions = append(regions, domain.Region{Name: r.Name, RoutingCode: r.RoutingCode})
	}
	repo := dal.NewRepo(deps.Store)
	svc, err := domain.NewService(domain.Deps{
		Repo: repo, Hierarchy: deps.Authz, Regions: projectRegions{deps.Peers}, Catalog: catalog,
		Sources: func() map[string]peer.QuotaUsageSource { return peer.QuotaUsageSources(deps.Peers) },
		Ops:     deps.Ops, Clock: deps.Clock, AllRegions: regions,
	})
	if err != nil {
		return err
	}
	if err := peer.Provide[peer.QuotaGate](deps.Peers, peer.QuotaGateKey, svc); err != nil {
		return fmt.Errorf("quotas: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindAllowance, func(ctx context.Context, id string) (string, error) {
		a, err := repo.Get(ctx, id)
		return a.Msg.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)
	quotaspb.RegisterQuotaAllowanceServiceServer(reg, api.NewHandler(svc))
	return nil
}

// projectRegions looks the IAM project-regions capability up at call time.
type projectRegions struct{ peers *peer.Directory }

func (p projectRegions) Region(ctx context.Context, projectID string) (string, error) {
	r, ok := peer.Lookup(p.peers, peer.ProjectRegionsKey)
	if !ok {
		return "", errs.Internal("quotas: project regions are unavailable (iam module disabled)")
	}
	return r.Region(ctx, projectID)
}
