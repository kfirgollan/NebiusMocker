// Package billing is the NebiusMocker module for Nebius billing
// (nebius.billing.v1, nebius.billing.v1alpha1).
//
// Implemented: PricingPolicyService (maximum prices of preemptible VMs),
// SpotPriceService (the spot market), and the v1 and v1alpha1
// CalculatorService (cost estimates), all from one price list (built-in
// prices.yaml or NEBIUS_MOCKER_BILLING_PRICES_FILE). OneTimeExportService
// answers UNIMPLEMENTED. See docs/services/billing.md.
package billing

import (
	"context"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	billingv1alpha1pb "github.com/nebius/gosdk/proto/nebius/billing/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/billing/api"
	"github.com/kfirgollan/NebiusMocker/services/billing/dal"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

// Module is the billing service module.
type Module struct{}

var _ mocker.Module = (*Module)(nil)

// New returns the billing module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "billing" }

// Register wires the billing services and their authorization.
func (*Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	prices, err := loadPrices(deps.Config)
	if err != nil {
		return fmt.Errorf("billing: %w", err)
	}
	regions := projectRegions{dir: deps.Peers, regions: deps.Config.Regions}
	platforms := computePlatforms{dir: deps.Peers}
	cfg := domain.Config{ProjectID: deps.Config.ProjectID, Started: deps.Clock.Now()}

	repo := dal.NewPricingPolicyRepo(deps.Store)
	policies, err := domain.NewPricingPolicyService(domain.PricingPolicyDeps{
		Repo: repo, Prices: prices, Regions: regions, Platforms: platforms, Hierarchy: deps.Authz,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Config: cfg,
	})
	if err != nil {
		return fmt.Errorf("billing: %w", err)
	}
	spot, err := domain.NewSpotService(domain.SpotDeps{Prices: prices, Clock: deps.Clock, Config: cfg})
	if err != nil {
		return fmt.Errorf("billing: %w", err)
	}
	calc, err := domain.NewCalculator(domain.CalculatorDeps{
		Prices: prices, Regions: regions, Platforms: platforms, Hierarchy: deps.Authz, Config: cfg,
	})
	if err != nil {
		return fmt.Errorf("billing: %w", err)
	}

	deps.Authz.RegisterKind(domain.KindPricingPolicy, func(ctx context.Context, id string) (string, error) {
		p, err := repo.Get(ctx, id)
		return p.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)
	billingpb.RegisterPricingPolicyServiceServer(reg, api.NewPricingPolicyHandler(policies))
	billingpb.RegisterCalculatorServiceServer(reg, api.NewCalculatorHandler(calc))
	billingv1alpha1pb.RegisterCalculatorServiceServer(reg, api.NewCalculatorAlphaHandler(calc))
	billingv1alpha1pb.RegisterSpotPriceServiceServer(reg, api.NewSpotPriceHandler(spot))
	billingv1alpha1pb.RegisterOneTimeExportServiceServer(reg, api.NewOneTimeExportHandler())
	return nil
}

// loadPrices returns the built-in price list, or the one in
// NEBIUS_MOCKER_BILLING_PRICES_FILE.
func loadPrices(cfg config.Config) (*domain.PriceList, error) {
	names := make([]string, 0, len(cfg.Regions))
	for _, r := range cfg.Regions {
		names = append(names, r.Name)
	}
	rc := cfg.RoutingCode
	if cfg.Billing.PricesFile == "" {
		return domain.DefaultPriceList(names, rc)
	}
	data, err := os.ReadFile(cfg.Billing.PricesFile)
	if err != nil {
		return nil, fmt.Errorf("read price list: %w", err)
	}
	return domain.ParsePriceList(data, names, rc)
}

// projectRegions resolves a project's region through the iam module's
// capability (looked up per call), else from the routing code of the
// project NID, as the compute platform catalog does.
type projectRegions struct {
	dir     *peer.Directory
	regions []config.Region
}

var _ domain.Regions = projectRegions{}

func (p projectRegions) Region(ctx context.Context, projectID string) (string, error) {
	if r, ok := peer.Lookup(p.dir, peer.ProjectRegionsKey); ok {
		return r.Region(ctx, projectID)
	}
	if rest, ok := strings.CutPrefix(projectID, domain.KindProject+"-"); ok && len(rest) >= 3 {
		for _, r := range p.regions {
			if r.RoutingCode == rest[:3] {
				return r.Name, nil
			}
		}
	}
	return "", errs.BadRequest(errs.Violation{Field: "parent_id", Message: "the region of project " + projectID + " is unknown"})
}

// computePlatforms resolves platforms through the compute module's catalog
// (looked up per call; compute may be disabled).
type computePlatforms struct{ dir *peer.Directory }

var _ domain.Platforms = computePlatforms{}

func (c computePlatforms) Platform(ctx context.Context, projectID, name string) (domain.Platform, bool, error) {
	cp, ok := peer.Lookup(c.dir, peer.ComputePlatformsKey)
	if !ok {
		return domain.Platform{}, false, nil
	}
	pl, err := cp.Platform(ctx, projectID, name)
	if err != nil {
		return domain.Platform{}, true, err
	}
	out := domain.Platform{Name: pl.Name, AllowedForPreemptibles: pl.AllowedForPreemptibles}
	for _, pr := range pl.Presets {
		out.Presets = append(out.Presets, domain.Preset{Name: pr.Name, GPUs: pr.GPUs, VCPUs: pr.VCPUs, MemoryGiB: pr.MemoryGiB})
	}
	return out, true, nil
}
