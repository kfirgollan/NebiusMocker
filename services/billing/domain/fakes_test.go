package domain_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

const (
	ppProject      = "project-e00mock000000000default"
	ppOtherProject = "project-e00mock000000000other"
	ppWestProject  = "project-e00mock000000000westus"
	ppEast         = "eu-north1"
	ppWest         = "us-central1"
)

var ppT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// ppRepo is a slice-backed domain.PricingPolicyRepository (creation order).
type ppRepo struct{ items []*billingpb.PricingPolicy }

var _ domain.PricingPolicyRepository = (*ppRepo)(nil)

func (f *ppRepo) index(id string) int {
	return slices.IndexFunc(f.items, func(p *billingpb.PricingPolicy) bool { return p.GetMetadata().GetId() == id })
}

func (f *ppRepo) named(parentID, name string) *billingpb.PricingPolicy {
	for _, p := range f.items {
		if md := p.GetMetadata(); md.GetParentId() == parentID && md.GetName() == name {
			return p
		}
	}
	return nil
}

func (f *ppRepo) Get(_ context.Context, id string) (*billingpb.PricingPolicy, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *ppRepo) GetByName(_ context.Context, parentID, name string) (*billingpb.PricingPolicy, error) {
	p := f.named(parentID, name)
	if p == nil {
		return nil, errs.NotFound(name)
	}
	return proto.CloneOf(p), nil
}

func (f *ppRepo) List(_ context.Context, parentID string, size int, _ string) (domain.Page[*billingpb.PricingPolicy], error) {
	out := []*billingpb.PricingPolicy{}
	for _, p := range f.items {
		if p.GetMetadata().GetParentId() == parentID && len(out) < size {
			out = append(out, proto.CloneOf(p))
		}
	}
	return domain.Page[*billingpb.PricingPolicy]{Items: out}, nil
}

func (f *ppRepo) Create(_ context.Context, p *billingpb.PricingPolicy) error {
	md := p.GetMetadata()
	if other := f.named(md.GetParentId(), md.GetName()); other != nil {
		return errs.AlreadyExists(other.GetMetadata().GetId(), "name taken")
	}
	f.items = append(f.items, proto.CloneOf(p))
	return nil
}

func (f *ppRepo) Update(_ context.Context, p *billingpb.PricingPolicy) error {
	md := p.GetMetadata()
	i := f.index(md.GetId())
	if i < 0 {
		return errs.NotFound(md.GetId())
	}
	if other := f.named(md.GetParentId(), md.GetName()); other != nil && other.GetMetadata().GetId() != md.GetId() {
		return errs.AlreadyExists(other.GetMetadata().GetId(), "name taken")
	}
	f.items[i] = proto.CloneOf(p)
	return nil
}

func (f *ppRepo) Delete(_ context.Context, id string) error {
	i := f.index(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// ppRegions maps projects to regions; the default project is in ppEast.
type ppRegions map[string]string

func (r ppRegions) Region(_ context.Context, projectID string) (string, error) {
	if reg, ok := r[projectID]; ok {
		return reg, nil
	}
	return "", errs.NotFound(projectID)
}

// ppPlatforms is a configurable compute catalog.
type ppPlatforms struct {
	enabled    bool
	preemptOK  bool
	notOffered map[string]bool
}

func (p ppPlatforms) Platform(_ context.Context, _, name string) (domain.Platform, bool, error) {
	if !p.enabled {
		return domain.Platform{}, false, nil
	}
	if p.notOffered[name] {
		return domain.Platform{}, true, errs.NotFound(name)
	}
	return domain.Platform{Name: name, AllowedForPreemptibles: p.preemptOK}, true, nil
}

// ppHierarchy knows exactly one project besides the configured one.
type ppHierarchy struct{}

func (ppHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id != ppOtherProject && id != ppWestProject {
		return "", errs.NotFound(id)
	}
	return "tenant-e00mock000000000default", nil
}

// ppOps runs Prepare at once and Work at once, or, with hold set, only when
// the test calls finish (an operation still running).
type ppOps struct {
	hold    bool
	pending []func(context.Context) error
}

func (o *ppOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{
		ID: "op-" + s.ResourceID, Description: s.Description, CreatedBy: s.CreatedBy,
		ResourceID: s.ResourceID, Request: s.Request, CreatedAt: ppT0,
	}
	if s.Work == nil {
		return op, nil
	}
	if o.hold {
		o.pending = append(o.pending, s.Work)
		return op, nil
	}
	op.Err = s.Work(ctx)
	return op, nil
}

// finish runs the Work of every held operation.
func (o *ppOps) finish(t *testing.T) {
	t.Helper()
	for _, w := range o.pending {
		if err := w(t.Context()); err != nil {
			t.Fatalf("operation work: %v", err)
		}
	}
	o.pending = nil
}

// ppEnv is a PricingPolicyService wired to fakes.
type ppEnv struct {
	svc  *domain.PricingPolicyService
	repo *ppRepo
	ops  *ppOps
	clk  *clock.Manual
}

type ppOption func(*ppEnvConfig)

type ppEnvConfig struct {
	prices    *domain.PriceList
	platforms ppPlatforms
}

func ppWithPrices(pl *domain.PriceList) ppOption { return func(c *ppEnvConfig) { c.prices = pl } }

func ppWithPlatforms(p ppPlatforms) ppOption { return func(c *ppEnvConfig) { c.platforms = p } }

func ppDefaultPrices(t *testing.T) *domain.PriceList {
	t.Helper()
	pl, err := domain.DefaultPriceList([]string{ppEast, ppWest}, "e00")
	if err != nil {
		t.Fatalf("DefaultPriceList: %v", err)
	}
	return pl
}

func newPPEnv(t *testing.T, opts ...ppOption) *ppEnv {
	t.Helper()
	cfg := &ppEnvConfig{platforms: ppPlatforms{preemptOK: true}}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.prices == nil {
		cfg.prices = ppDefaultPrices(t)
	}
	ids, err := nid.NewGenerator(nid.Sequential, "e00", 0)
	if err != nil {
		t.Fatalf("nid.NewGenerator: %v", err)
	}
	e := &ppEnv{repo: &ppRepo{}, ops: &ppOps{}, clk: clock.NewManual(ppT0)}
	e.svc, err = domain.NewPricingPolicyService(domain.PricingPolicyDeps{
		Repo: e.repo, Prices: cfg.prices,
		Regions:   ppRegions{ppProject: ppEast, ppOtherProject: ppEast, ppWestProject: ppWest},
		Platforms: cfg.platforms, Hierarchy: ppHierarchy{}, Ops: e.ops, IDs: ids, Clock: e.clk,
		Config: domain.Config{ProjectID: ppProject},
	})
	if err != nil {
		t.Fatalf("NewPricingPolicyService: %v", err)
	}
	return e
}
