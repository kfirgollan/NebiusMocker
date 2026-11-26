package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

var platformRegions = []Region{{"eu-north1", "e00"}, {"eu-west1", "e01"}, {"us-central1", "u00"}}

const (
	projEU = "project-e00mock000000000default"
	projUS = "project-u00mock000000000default"
)

func newPlatforms(t *testing.T, doc string) *PlatformService {
	t.Helper()
	var (
		cat PlatformCatalog
		err error
	)
	if doc == "" {
		cat, err = DefaultPlatformCatalog()
	} else {
		cat, err = ParsePlatformCatalog([]byte(doc))
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewPlatformService(PlatformDeps{Catalog: cat, Regions: platformRegions, Projects: fakeProjects{projEU: true, projUS: true, "project-e01mock000000000default": true, "project-x99mock000000000default": true}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func presetNames(t *testing.T, s *PlatformService, project, name string) []string {
	t.Helper()
	p, err := s.GetByName(t.Context(), project, name)
	if err != nil {
		t.Fatalf("GetByName(%s): %v", name, err)
	}
	var out []string
	for _, pr := range p.GetSpec().GetPresets() {
		out = append(out, pr.GetName())
	}
	return out
}

func TestParsePlatformCatalogErrors(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		"platforms: [{name: '', presets: [{name: a, vcpu_count: 1, memory_gibibytes: 1}]}]",
		"platforms: [{name: a}]",
		"platforms: [{name: a, presets: [{name: p, vcpu_count: 1, memory_gibibytes: 1}]}, {name: a, presets: [{name: p, vcpu_count: 1, memory_gibibytes: 1}]}]",
		"platforms: [{name: a, presets: [{name: p, vcpu_count: 1, memory_gibibytes: 1}, {name: p, vcpu_count: 1, memory_gibibytes: 1}]}]",
		"platforms: [{name: a, presets: [{name: p, vcpu_count: 0, memory_gibibytes: 1}]}]",
		"platforms: [{name: a, presets: [{name: p, vcpu_count: 1, memory_gibibytes: 0}]}]",
		"platforms: [{name: a, colour: red, presets: [{name: p, vcpu_count: 1, memory_gibibytes: 1}]}]",
		"{",
	} {
		if _, err := ParsePlatformCatalog([]byte(doc)); err == nil {
			t.Errorf("ParsePlatformCatalog(%q) succeeded, want error", doc)
		}
	}
}

func TestParsePlatformCatalogJSON(t *testing.T) {
	t.Parallel()
	cat, err := ParsePlatformCatalog([]byte(`{"platforms":[{"name":"a","regions":["eu-north1"],"presets":[{"name":"p","vcpu_count":2,"memory_gibibytes":8}]}]}`))
	if err != nil || len(cat.Platforms) != 1 || cat.Platforms[0].Presets[0].VCPUCount != 2 {
		t.Fatalf("catalog = %+v, err %v", cat, err)
	}
}

func TestDefaultPlatformsPerRegion(t *testing.T) {
	t.Parallel()
	s := newPlatforms(t, "")
	ctx := t.Context()
	eu, err := s.List(ctx, projEU, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range eu.Items {
		names = append(names, p.GetMetadata().GetName())
		if p.GetMetadata().GetParentId() != projEU || p.GetMetadata().GetResourceVersion() != 1 || !strings.HasPrefix(p.GetMetadata().GetId(), "computeplatform-e00") {
			t.Errorf("bad metadata %v", p.GetMetadata())
		}
	}
	want := []string{"gpu-h100-sxm", "gpu-h200-sxm", "gpu-gb200", "gpu-gb300", "gpu-l40s-a", "gpu-l40s-d", "cpu-d3", "cpu-e2"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("eu-north1 platforms = %v, want %v", names, want)
	}
	if got := presetNames(t, s, projEU, "gpu-h100-sxm"); strings.Join(got, ",") != "1gpu-16vcpu-200gb,8gpu-128vcpu-1600gb" {
		t.Errorf("h100 presets = %v", got)
	}
	// Preset exclusion: cpu-d3 160vcpu and up are not in eu-north1 but are in us-central1.
	if got := presetNames(t, s, projEU, "cpu-d3"); len(got) != 9 || got[len(got)-1] != "128vcpu-512gb" {
		t.Errorf("cpu-d3 in eu-north1 = %v", got)
	}
	if got := presetNames(t, s, projUS, "cpu-d3"); len(got) != 13 {
		t.Errorf("cpu-d3 in us-central1 = %v", got)
	}
	h100, _ := s.GetByName(ctx, projEU, "gpu-h100-sxm")
	if !h100.GetStatus().GetAllowedForPreemptibles() || h100.GetSpec().GetGpuMemoryGigabytes() != 80 || !h100.GetSpec().GetPresets()[1].GetAllowGpuClustering() {
		t.Errorf("h100 = %v", h100)
	}
	// IDs are deterministic and per region.
	again, _ := newPlatforms(t, "").GetByName(ctx, projEU, "gpu-h100-sxm")
	other, _ := s.GetByName(ctx, projUS, "gpu-h200-sxm")
	h200, _ := s.GetByName(ctx, projEU, "gpu-h200-sxm")
	if again.GetMetadata().GetId() != h100.GetMetadata().GetId() || other.GetMetadata().GetId() == h200.GetMetadata().GetId() {
		t.Errorf("ids not stable/per-region: %s %s %s %s", again.GetMetadata().GetId(), h100.GetMetadata().GetId(), other.GetMetadata().GetId(), h200.GetMetadata().GetId())
	}
}

func TestPlatformPaging(t *testing.T) {
	t.Parallel()
	s := newPlatforms(t, "")
	var got []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("too many pages")
		}
		page, err := s.List(t.Context(), projEU, 2, token)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 2 {
			t.Fatalf("page of %d", len(page.Items))
		}
		for _, p := range page.Items {
			got = append(got, p.GetMetadata().GetName())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if len(got) != 8 {
		t.Errorf("paged = %v", got)
	}
	if _, err := s.List(t.Context(), projEU, 0, "garbage"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("bad token err = %v", err)
	}
}

func TestPlatformGetByNameErrors(t *testing.T) {
	t.Parallel()
	s := newPlatforms(t, "")
	ctx := t.Context()
	for name, tc := range map[string]struct {
		parent, name string
		kind         errs.Kind
	}{
		"empty parent":       {"", "cpu-e2", errs.KindBadRequest},
		"empty name":         {projEU, "", errs.KindBadRequest},
		"not in region":      {projEU, "gpu-b200-sxm", errs.KindNotFound},
		"missing project":    {"project-e00nothere0000000000", "cpu-e2", errs.KindNotFound},
		"unconfigured code":  {"project-x99mock000000000default", "cpu-e2", errs.KindBadRequest},
		"cpu-e2 not in west": {"project-e01mock000000000default", "cpu-e2", errs.KindNotFound},
	} {
		if _, err := s.GetByName(ctx, tc.parent, tc.name); !errs.IsKind(err, tc.kind) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.kind)
		}
	}
	if _, err := s.List(ctx, "", 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("List empty parent: %v", err)
	}
}

func TestPlatformResolve(t *testing.T) {
	t.Parallel()
	s := newPlatforms(t, "")
	ctx := t.Context()
	pp, err := s.Resolve(ctx, projEU, "gpu-h100-sxm", "8gpu-128vcpu-1600gb")
	if err != nil || pp.Preset.GetResources().GetGpuCount() != 8 || pp.Platform.GetMetadata().GetName() != "gpu-h100-sxm" {
		t.Fatalf("Resolve = %+v, %v", pp, err)
	}
	for _, tc := range []struct{ platform, preset, field, contains string }{
		{"gpu-b200-sxm", "1gpu-20vcpu-224gb", "spec.resources.platform", "eu-north1"},
		{"nope", "x", "spec.resources.platform", "nope"},
		{"gpu-h100-sxm", "2gpu", "spec.resources.preset", "1gpu-16vcpu-200gb, 8gpu-128vcpu-1600gb"},
		{"cpu-d3", "160vcpu-640gb", "spec.resources.preset", "128vcpu-512gb"},
	} {
		_, err := s.Resolve(ctx, projEU, tc.platform, tc.preset)
		e, ok := errors.AsType[*errs.Error](err)
		if !ok || e.Kind != errs.KindBadRequest {
			t.Errorf("%s/%s: err = %v", tc.platform, tc.preset, err)
			continue
		}
		if len(e.Violations) != 1 || e.Violations[0].Field != tc.field || !strings.Contains(e.Violations[0].Message, tc.contains) {
			t.Errorf("%s/%s: violations = %+v", tc.platform, tc.preset, e.Violations)
		}
	}
}

func TestPlatformUnconfiguredRegionNeverOffered(t *testing.T) {
	t.Parallel()
	s := newPlatforms(t, "platforms: [{name: a, regions: [mars1, eu-north1], presets: [{name: p, vcpu_count: 1, memory_gibibytes: 1}]}]")
	if got := presetNames(t, s, projEU, "a"); len(got) != 1 {
		t.Errorf("presets = %v", got)
	}
	if _, err := s.GetByName(t.Context(), projUS, "a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("us-central1: %v", err)
	}
}
