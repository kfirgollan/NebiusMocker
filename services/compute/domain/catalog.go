package domain

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

//go:embed catalog.yaml
var defaultCatalogYAML []byte

// Catalog is the public image catalog seeded into every region.
type Catalog struct {
	Families []Family `yaml:"families" json:"families"`
}

// Family is one public image family and its versions.
type Family struct {
	// Name is spec.image_family, e.g. "ubuntu24.04-driverless".
	Name          string `yaml:"name" json:"name"`
	HumanReadable string `yaml:"human_readable" json:"human_readable"`
	Description   string `yaml:"description" json:"description"`
	// CPUArchitecture is AMD64 (default) or ARM64.
	CPUArchitecture      string            `yaml:"cpu_architecture" json:"cpu_architecture"`
	MinDiskSizeGiB       int64             `yaml:"min_disk_size_gibibytes" json:"min_disk_size_gibibytes"`
	Labels               map[string]string `yaml:"labels" json:"labels"`
	RecommendedPlatforms []string          `yaml:"recommended_platforms" json:"recommended_platforms"`
	UnsupportedPlatforms map[string]string `yaml:"unsupported_platforms" json:"unsupported_platforms"`
	// Regions limits the family to these regions; empty means every region.
	Regions []string `yaml:"regions" json:"regions"`
	// Deprecation applies to every version of the family.
	Deprecation *Deprecation `yaml:"deprecation" json:"deprecation"`
	Versions    []Version    `yaml:"versions" json:"versions"`
}

// Version is one image of a family.
type Version struct {
	Version string `yaml:"version" json:"version"`
	// ReleasedAt is the image's created_at; default: Version parsed as YYYYMMDD.
	ReleasedAt time.Time `yaml:"released_at" json:"released_at"`
	// Deprecation overrides the family's deprecation for this version.
	Deprecation *Deprecation `yaml:"deprecation" json:"deprecation"`
}

// Deprecation is status.image_family_deprecation.
type Deprecation struct {
	At      time.Time `yaml:"at" json:"at"`
	Message string    `yaml:"message" json:"message"`
}

// DefaultCatalog returns the built-in catalog (catalog.yaml).
func DefaultCatalog() (Catalog, error) {
	return ParseCatalog(defaultCatalogYAML)
}

// ParseCatalog decodes and validates a YAML (or JSON) catalog.
func ParseCatalog(data []byte) (Catalog, error) {
	var c Catalog
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("decode image catalog: %w", err)
	}
	if err := c.validate(); err != nil {
		return Catalog{}, fmt.Errorf("image catalog: %w", err)
	}
	return c, nil
}

func (c Catalog) validate() error {
	var problems []error
	seen := map[string]bool{}
	for i := range c.Families {
		f := &c.Families[i]
		at := fmt.Sprintf("families[%d] (%s)", i, f.Name)
		switch {
		case f.Name == "":
			problems = append(problems, fmt.Errorf("%s: name is required", at))
		case seen[f.Name]:
			problems = append(problems, fmt.Errorf("%s: duplicate family", at))
		case len(f.Versions) == 0:
			problems = append(problems, fmt.Errorf("%s: at least one version is required", at))
		}
		seen[f.Name] = true
		if _, err := parseArch(f.CPUArchitecture); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", at, err))
		}
		if f.MinDiskSizeGiB < 0 {
			problems = append(problems, fmt.Errorf("%s: min_disk_size_gibibytes must not be negative", at))
		}
		versions := map[string]bool{}
		for _, v := range f.Versions {
			if v.Version == "" || versions[v.Version] {
				problems = append(problems, fmt.Errorf("%s: empty or duplicate version %q", at, v.Version))
			}
			versions[v.Version] = true
		}
	}
	return errors.Join(problems...)
}

// CheckRegions fails if a family names a region that is not configured.
func (c Catalog) CheckRegions(regions []Region) error {
	var problems []error
	for i := range c.Families {
		f := &c.Families[i]
		for _, r := range f.Regions {
			if !slices.ContainsFunc(regions, func(x Region) bool { return x.Name == r }) {
				problems = append(problems, fmt.Errorf("image family %s: unknown region %q", f.Name, r))
			}
		}
	}
	return errors.Join(problems...)
}

func (f *Family) inRegion(region string) bool {
	return len(f.Regions) == 0 || slices.Contains(f.Regions, region)
}

// releasedAt is the created_at of a seeded version.
func (v Version) releasedAt(fallback time.Time) time.Time {
	if !v.ReleasedAt.IsZero() {
		return v.ReleasedAt.UTC()
	}
	if t, err := time.Parse("20060102", v.Version); err == nil {
		return t
	}
	return fallback
}

func parseArch(s string) (computepb.ImageSpec_CPUArchitecture, error) {
	switch s {
	case "", "AMD64", "amd64":
		return computepb.ImageSpec_AMD64, nil
	case "ARM64", "arm64":
		return computepb.ImageSpec_ARM64, nil
	}
	return 0, fmt.Errorf("cpu_architecture %q: want AMD64 or ARM64", s)
}

// Region is an emulated region and its NID routing code.
type Region struct {
	Name        string
	RoutingCode string
}

// PublicProjectID is the project that owns a region's public images. Upstream:
// GetImageLatestByFamilyRequest.parent_id "The default is
// `project-{region}public-images`", where {region} is the routing code.
func PublicProjectID(routingCode string) string {
	return "project-" + routingCode + "public-images"
}
