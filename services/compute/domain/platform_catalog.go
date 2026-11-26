package domain

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"slices"

	"go.yaml.in/yaml/v3"
)

//go:embed platforms.yaml
var defaultPlatformsYAML []byte

// PlatformCatalog is the read-only set of Compute platforms offered per region.
type PlatformCatalog struct {
	Platforms []PlatformDef `yaml:"platforms" json:"platforms"`
}

// PlatformDef is one platform (metadata.name) and its presets.
type PlatformDef struct {
	Name                   string `yaml:"name" json:"name"`
	HumanReadableName      string `yaml:"human_readable_name" json:"human_readable_name"`
	ShortHumanReadableName string `yaml:"short_human_readable_name" json:"short_human_readable_name"`
	GPUCountQuotaType      string `yaml:"gpu_count_quota_type" json:"gpu_count_quota_type"`
	GPUMemoryGigabytes     int32  `yaml:"gpu_memory_gigabytes" json:"gpu_memory_gigabytes"`
	AllowPresetChange      bool   `yaml:"allow_preset_change" json:"allow_preset_change"`
	AllowedForPreemptibles bool   `yaml:"allowed_for_preemptibles" json:"allowed_for_preemptibles"`
	// NVLInstanceGroupType is the NVL instance group type (GB200, GB300) whose
	// groups instances of this platform can join; empty: none.
	NVLInstanceGroupType string `yaml:"nvl_instance_group_type" json:"nvl_instance_group_type"`
	// Regions limits the platform to these regions; empty means every region.
	Regions []string    `yaml:"regions" json:"regions"`
	Presets []PresetDef `yaml:"presets" json:"presets"`
}

// PresetDef is one preset of a platform.
type PresetDef struct {
	Name               string `yaml:"name" json:"name"`
	VCPUCount          int32  `yaml:"vcpu_count" json:"vcpu_count"`
	MemoryGibibytes    int32  `yaml:"memory_gibibytes" json:"memory_gibibytes"`
	GPUCount           int32  `yaml:"gpu_count" json:"gpu_count"`
	AllowGPUClustering bool   `yaml:"allow_gpu_clustering" json:"allow_gpu_clustering"`
	// ExcludeRegions hides the preset in these regions.
	ExcludeRegions []string `yaml:"exclude_regions" json:"exclude_regions"`
}

// DefaultPlatformCatalog returns the built-in catalog (platforms.yaml).
func DefaultPlatformCatalog() (PlatformCatalog, error) {
	return ParsePlatformCatalog(defaultPlatformsYAML)
}

// ParsePlatformCatalog decodes and validates a YAML (or JSON) catalog.
func ParsePlatformCatalog(data []byte) (PlatformCatalog, error) {
	var c PlatformCatalog
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return PlatformCatalog{}, fmt.Errorf("decode platform catalog: %w", err)
	}
	if err := c.validate(); err != nil {
		return PlatformCatalog{}, fmt.Errorf("platform catalog: %w", err)
	}
	return c, nil
}

func (c PlatformCatalog) validate() error {
	var problems []error
	seen := map[string]bool{}
	for i := range c.Platforms {
		p := &c.Platforms[i]
		at := fmt.Sprintf("platforms[%d] (%s)", i, p.Name)
		switch {
		case p.Name == "":
			problems = append(problems, fmt.Errorf("%s: name is required", at))
		case seen[p.Name]:
			problems = append(problems, fmt.Errorf("%s: duplicate platform", at))
		case len(p.Presets) == 0:
			problems = append(problems, fmt.Errorf("%s: at least one preset is required", at))
		}
		if t := p.NVLInstanceGroupType; t != "" && !validNVLType(t) {
			problems = append(problems, fmt.Errorf("%s: nvl_instance_group_type %q is not one of %s", at, t, nvlTypeNames()))
		}
		seen[p.Name] = true
		names := map[string]bool{}
		for _, pr := range p.Presets {
			switch {
			case pr.Name == "" || names[pr.Name]:
				problems = append(problems, fmt.Errorf("%s: empty or duplicate preset %q", at, pr.Name))
			case pr.VCPUCount <= 0 || pr.MemoryGibibytes <= 0 || pr.GPUCount < 0:
				problems = append(problems, fmt.Errorf("%s: preset %s needs vcpu_count > 0, memory_gibibytes > 0 and gpu_count >= 0", at, pr.Name))
			}
			names[pr.Name] = true
		}
	}
	return errors.Join(problems...)
}

func (p *PlatformDef) offeredIn(region string) bool {
	return len(p.Regions) == 0 || slices.Contains(p.Regions, region)
}

func (p *PresetDef) offeredIn(region string) bool {
	return !slices.Contains(p.ExcludeRegions, region)
}
