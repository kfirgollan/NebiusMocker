package domain

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed fabrics.yaml
var defaultFabricsYAML []byte

// FabricCatalog lists the InfiniBand fabrics a GPU cluster can be built on.
// The mocker only keeps the inventory: no fabric or topology is simulated.
type FabricCatalog struct {
	Fabrics []Fabric `yaml:"fabrics" json:"fabrics"`
}

// Fabric is one physical InfiniBand fabric (GpuClusterSpec.infiniband_fabric).
type Fabric struct {
	// Name is the value of spec.infiniband_fabric, e.g. "fabric-3".
	Name string `yaml:"name" json:"name"`
	// Region is the only region whose projects can use the fabric.
	Region string `yaml:"region" json:"region"`
	// Platforms are the GPU platforms whose instances can join a cluster on
	// this fabric, e.g. "gpu-h100-sxm".
	Platforms []string `yaml:"platforms" json:"platforms"`
}

// DefaultFabricCatalog returns the built-in catalog (fabrics.yaml).
func DefaultFabricCatalog() (FabricCatalog, error) {
	return ParseFabricCatalog(defaultFabricsYAML)
}

// ParseFabricCatalog decodes and validates a YAML (or JSON) fabric catalog.
func ParseFabricCatalog(data []byte) (FabricCatalog, error) {
	var c FabricCatalog
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return FabricCatalog{}, fmt.Errorf("decode fabric catalog: %w", err)
	}
	if err := c.validate(); err != nil {
		return FabricCatalog{}, fmt.Errorf("fabric catalog: %w", err)
	}
	return c, nil
}

func (c FabricCatalog) validate() error {
	var problems []error
	seen := map[string]bool{}
	for i, f := range c.Fabrics {
		at := fmt.Sprintf("fabrics[%d] (%s)", i, f.Name)
		switch {
		case f.Name == "":
			problems = append(problems, fmt.Errorf("%s: name is required", at))
		case seen[f.Name]:
			problems = append(problems, fmt.Errorf("%s: duplicate fabric", at))
		}
		seen[f.Name] = true
		if f.Region == "" {
			problems = append(problems, fmt.Errorf("%s: region is required", at))
		}
		if len(f.Platforms) == 0 || slices.Contains(f.Platforms, "") {
			problems = append(problems, fmt.Errorf("%s: at least one non-empty platform is required", at))
		}
	}
	return errors.Join(problems...)
}

// Lookup returns the fabric called name.
func (c FabricCatalog) Lookup(name string) (Fabric, bool) {
	i := slices.IndexFunc(c.Fabrics, func(f Fabric) bool { return f.Name == name })
	if i < 0 {
		return Fabric{}, false
	}
	return c.Fabrics[i], true
}

// InRegion returns the names of the fabrics of region, sorted.
func (c FabricCatalog) InRegion(region string) []string {
	var out []string
	for _, f := range c.Fabrics {
		if f.Region == region {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Allows reports whether instances of platform can join a cluster on f.
func (f Fabric) Allows(platform string) bool { return slices.Contains(f.Platforms, platform) }

func (f Fabric) platformList() string { return strings.Join(f.Platforms, ", ") }
