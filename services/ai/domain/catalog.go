package domain

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Platform is one compute platform endpoints may run on.
//
// The catalog lives in the AI area until a compute PlatformService exists;
// it will then be replaced by a lookup through pkg/peer.
type Platform struct {
	Name string
	// GPU is the GPU model selected by spec.gpu ("H100"); empty for CPU platforms.
	GPU string
	// CPU is the CPU family selected by spec.cpu ("amd"); empty for GPU platforms.
	CPU     string
	Presets []Preset
}

// Preset is one resource preset of a platform.
type Preset struct {
	Name  string
	GPUs  int
	VCPUs int
}

// Catalog is the set of known platforms.
type Catalog []Platform

// Minimum disk sizes from the endpoint docs.
const (
	gib            = int64(1) << 30
	minCPUDiskSize = 20 * gib
	minGPUDiskSize = 50 * gib
)

func (c Catalog) platform(name string) (Platform, bool) {
	for _, p := range c {
		if p.Name == name {
			return p, true
		}
	}
	return Platform{}, false
}

func (p Platform) preset(name string) (Preset, bool) {
	for _, pr := range p.Presets {
		if pr.Name == name {
			return pr, true
		}
	}
	return Preset{}, false
}

func (p Platform) presetNames() []string {
	out := make([]string, 0, len(p.Presets))
	for _, pr := range p.Presets {
		out = append(out, pr.Name)
	}
	return out
}

// hardware is the resolved platform and preset of a spec.
type hardware struct {
	platform Platform
	preset   Preset
}

// resolveExplicit checks an explicit platform/preset pair.
func (c Catalog) resolveExplicit(platform, preset string) (hardware, error) {
	p, ok := c.platform(platform)
	if !ok {
		return hardware{}, errs.BadRequest(errs.Violation{Field: "spec.platform", Message: fmt.Sprintf("unknown platform %q", platform)})
	}
	pr, ok := p.preset(preset)
	if !ok {
		return hardware{}, errs.BadRequest(errs.Violation{
			Field:   "spec.preset",
			Message: fmt.Sprintf("unknown preset %q for platform %s (available: %s)", preset, platform, strings.Join(p.presetNames(), ", ")),
		})
	}
	return hardware{platform: p, preset: pr}, nil
}

// resolveGPU resolves a "MODEL[:COUNT]" selector; an omitted count means one GPU.
func (c Catalog) resolveGPU(sel string) (hardware, error) {
	model, count, err := parseSelector(sel, 1)
	if err != nil {
		return hardware{}, errs.BadRequest(errs.Violation{Field: "spec.gpu", Message: err.Error()})
	}
	for _, p := range c {
		if p.GPU == "" || !strings.EqualFold(p.GPU, model) {
			continue
		}
		for _, pr := range p.Presets {
			if pr.GPUs == count {
				return hardware{platform: p, preset: pr}, nil
			}
		}
	}
	return hardware{}, errs.BadRequest(errs.Violation{Field: "spec.gpu", Message: fmt.Sprintf("no platform offers %d x %s", count, model)})
}

// resolveCPU resolves a "FAMILY[:VCPUS]" selector; an omitted count picks the
// smallest preset of the family.
func (c Catalog) resolveCPU(sel string) (hardware, error) {
	family, vcpus, err := parseSelector(sel, 0)
	if err != nil {
		return hardware{}, errs.BadRequest(errs.Violation{Field: "spec.cpu", Message: err.Error()})
	}
	for _, p := range c {
		if p.CPU == "" || !strings.EqualFold(p.CPU, family) {
			continue
		}
		for _, pr := range p.Presets {
			if pr.GPUs == 0 && (vcpus == 0 || pr.VCPUs == vcpus) {
				return hardware{platform: p, preset: pr}, nil
			}
		}
	}
	return hardware{}, errs.BadRequest(errs.Violation{Field: "spec.cpu", Message: fmt.Sprintf("no platform offers %q", sel)})
}

func parseSelector(sel string, defaultCount int) (name string, count int, err error) {
	name, countStr, hasCount := strings.Cut(sel, ":")
	if name == "" {
		return "", 0, fmt.Errorf("selector %q has no model", sel)
	}
	if !hasCount {
		return name, defaultCount, nil
	}
	n, convErr := strconv.Atoi(countStr)
	if convErr != nil || n <= 0 {
		return "", 0, fmt.Errorf("selector %q has an invalid count", sel)
	}
	return name, n, nil
}

func (h hardware) minDiskSize() int64 {
	if h.platform.GPU != "" {
		return minGPUDiskSize
	}
	return minCPUDiskSize
}
