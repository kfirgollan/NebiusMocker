package domain_test

import (
	"context"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

const (
	calcProject      = "project-e00mock000000000default"
	calcOtherProject = "project-e00mock000000000other"
	calcGhostProject = "project-e00mock000000000ghost"
	calcInstanceID   = "computeinstance-e00test000000001"
)

var (
	calcRegionNames = []string{"eu-north1", "eu-north2", "eu-west1", "us-central1", "me-west1", "uk-south1", "uk-south2", "eu-south1", "eu-west2", "us-north1"}
	calcStarted     = time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
)

// calcRegions maps project IDs to regions; unknown projects are BadRequest.
type calcRegions map[string]string

func (r calcRegions) Region(_ context.Context, projectID string) (string, error) {
	if region, ok := r[projectID]; ok {
		return region, nil
	}
	return "", errs.BadRequest(errs.Violation{Field: "parent_id", Message: "unknown region"})
}

// calcPlatforms is a fake catalog; enabled=false models compute not loaded.
type calcPlatforms struct {
	disabled bool
	byName   map[string]domain.Platform
}

func (p calcPlatforms) Platform(_ context.Context, _, name string) (domain.Platform, bool, error) {
	if p.disabled {
		return domain.Platform{}, false, nil
	}
	pl, ok := p.byName[name]
	if !ok {
		return domain.Platform{}, true, errs.NotFound(name)
	}
	return pl, true, nil
}

// calcHierarchy maps resource IDs to their parents.
type calcHierarchy map[string]string

func (h calcHierarchy) Parent(_ context.Context, id string) (string, error) {
	if p, ok := h[id]; ok {
		return p, nil
	}
	return "", errs.NotFound(id)
}

func calcCatalog() calcPlatforms {
	return calcPlatforms{byName: map[string]domain.Platform{
		"gpu-h100-sxm": {Name: "gpu-h100-sxm", Presets: []domain.Preset{
			{Name: "8gpu-128vcpu-1600gb", GPUs: 8, VCPUs: 128, MemoryGiB: 1600},
			{Name: "1gpu-16vcpu-200gb", GPUs: 1, VCPUs: 16, MemoryGiB: 200},
		}},
		"gpu-b200-sxm": {Name: "gpu-b200-sxm", Presets: []domain.Preset{{Name: "8gpu-160vcpu-1792gb", GPUs: 8, VCPUs: 160, MemoryGiB: 1792}}},
		"cpu-e2": {Name: "cpu-e2", Presets: []domain.Preset{
			{Name: "4vcpu-16gb", VCPUs: 4, MemoryGiB: 16},
			{Name: "2vcpu-8gb", VCPUs: 2, MemoryGiB: 8},
		}},
		// In the catalog but not in the price list.
		"cpu-unpriced": {Name: "cpu-unpriced", Presets: []domain.Preset{{Name: "2vcpu-8gb", VCPUs: 2, MemoryGiB: 8}}},
	}}
}

func calcDefaultPrices(t *testing.T) *domain.PriceList {
	t.Helper()
	pl, err := domain.DefaultPriceList(calcRegionNames, "e00")
	if err != nil {
		t.Fatalf("default price list: %v", err)
	}
	return pl
}

func calcConfig() domain.Config { return domain.Config{ProjectID: calcProject, Started: calcStarted} }

func newCalcFor(t *testing.T, platforms calcPlatforms) *domain.Calculator {
	t.Helper()
	c, err := domain.NewCalculator(domain.CalculatorDeps{
		Prices: calcDefaultPrices(t),
		Regions: calcRegions{
			calcProject: "eu-north1", calcOtherProject: "us-central1",
		},
		Platforms: platforms,
		Hierarchy: calcHierarchy{calcOtherProject: "tenant-e00mock0000000000default", calcInstanceID: calcOtherProject},
		Config:    calcConfig(),
	})
	if err != nil {
		t.Fatalf("new calculator: %v", err)
	}
	return c
}

func newCalc(t *testing.T) *domain.Calculator {
	t.Helper()
	return newCalcFor(t, calcCatalog())
}

func newCalcSpot(t *testing.T) (*domain.SpotService, *clock.Manual) {
	t.Helper()
	clk := clock.NewManual(calcStarted.Add(2 * time.Hour))
	s, err := domain.NewSpotService(domain.SpotDeps{Prices: calcDefaultPrices(t), Clock: clk, Config: calcConfig()})
	if err != nil {
		t.Fatalf("new spot service: %v", err)
	}
	return s, clk
}

func mustDec(t *testing.T, s string) domain.Decimal {
	t.Helper()
	d, err := domain.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}
