package domain

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

//go:embed catalog.yaml
var defaultCatalogYAML []byte

// Catalog is the seed of capacity block groups and the GPU presets resource
// advice is computed for. Users cannot create block groups: in Nebius a
// manager creates them, so the mocker seeds them.
type Catalog struct {
	BlockGroups []BlockGroupDef `yaml:"block_groups" json:"block_groups"`
	Presets     []PlatformDef   `yaml:"presets" json:"presets"`
}

// BlockGroupDef is one capacity block group of the catalog.
type BlockGroupDef struct {
	// Key names the group; its NID is derived from Tenant and Key.
	Key string `yaml:"key" json:"key"`
	// Tenant owns the group (default: the seeded tenant).
	Tenant    string        `yaml:"tenant" json:"tenant"`
	Region    string        `yaml:"region" json:"region"`
	Platform  string        `yaml:"platform" json:"platform"`
	Fabric    string        `yaml:"fabric" json:"fabric"`
	Intervals []IntervalDef `yaml:"intervals" json:"intervals"`
}

// IntervalDef is one capacity interval: Quantity GPUs from start to end,
// given either as days relative to the mocker's start day or as RFC 3339
// timestamps.
type IntervalDef struct {
	Quantity uint64 `yaml:"quantity" json:"quantity"`
	StartDay *int   `yaml:"start_day" json:"start_day"`
	EndDay   *int   `yaml:"end_day" json:"end_day"`
	Start    string `yaml:"start" json:"start"`
	End      string `yaml:"end" json:"end"`
}

// PlatformDef holds the VM presets of one GPU platform.
type PlatformDef struct {
	Platform           string      `yaml:"platform" json:"platform"`
	GPUMemoryGigabytes int32       `yaml:"gpu_memory_gigabytes" json:"gpu_memory_gigabytes"`
	Presets            []PresetDef `yaml:"presets" json:"presets"`
}

// PresetDef is one VM preset of a GPU platform.
type PresetDef struct {
	Name            string `yaml:"name" json:"name"`
	VCPUCount       int32  `yaml:"vcpu_count" json:"vcpu_count"`
	MemoryGibibytes int32  `yaml:"memory_gibibytes" json:"memory_gibibytes"`
	GPUCount        int32  `yaml:"gpu_count" json:"gpu_count"`
}

// DefaultCatalog returns the built-in catalog (catalog.yaml).
func DefaultCatalog() (Catalog, error) { return ParseCatalog(defaultCatalogYAML) }

// ParseCatalog decodes and validates a YAML (or JSON) catalog.
func ParseCatalog(data []byte) (Catalog, error) {
	var c Catalog
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("decode capacity catalog: %w", err)
	}
	if err := c.validate(); err != nil {
		return Catalog{}, fmt.Errorf("capacity catalog: %w", err)
	}
	return c, nil
}

func (c Catalog) validate() error {
	var problems []error
	seen := map[string]bool{}
	for i, g := range c.BlockGroups {
		at := fmt.Sprintf("block_groups[%d] (%s)", i, g.Key)
		if g.Key == "" {
			problems = append(problems, fmt.Errorf("%s: key is required", at))
		}
		if k := g.Tenant + "/" + g.Key; seen[k] {
			problems = append(problems, fmt.Errorf("%s: duplicate key in the tenant", at))
		} else {
			seen[k] = true
		}
		if g.Region == "" || g.Platform == "" || g.Fabric == "" {
			problems = append(problems, fmt.Errorf("%s: region, platform and fabric are required", at))
		}
		if len(g.Intervals) == 0 {
			problems = append(problems, fmt.Errorf("%s: at least one interval is required", at))
		}
		for j, iv := range g.Intervals {
			if err := iv.validate(); err != nil {
				problems = append(problems, fmt.Errorf("%s: intervals[%d]: %w", at, j, err))
			}
		}
	}
	return errors.Join(append(problems, c.validatePresets())...)
}

func (c Catalog) validatePresets() error {
	var problems []error
	platforms := map[string]bool{}
	for i, p := range c.Presets {
		at := fmt.Sprintf("presets[%d] (%s)", i, p.Platform)
		switch {
		case p.Platform == "":
			problems = append(problems, fmt.Errorf("%s: platform is required", at))
		case platforms[p.Platform]:
			problems = append(problems, fmt.Errorf("%s: duplicate platform", at))
		}
		platforms[p.Platform] = true
		for _, pr := range p.Presets {
			if pr.Name == "" || pr.VCPUCount <= 0 || pr.MemoryGibibytes <= 0 || pr.GPUCount <= 0 {
				problems = append(problems, fmt.Errorf("%s: preset %q needs a name, vcpu_count, memory_gibibytes and gpu_count > 0", at, pr.Name))
			}
		}
	}
	return errors.Join(problems...)
}

func (iv IntervalDef) validate() error {
	days := iv.StartDay != nil || iv.EndDay != nil
	stamps := iv.Start != "" || iv.End != ""
	switch {
	case days && stamps:
		return errors.New("use either start_day/end_day or start/end")
	case days && (iv.StartDay == nil || iv.EndDay == nil):
		return errors.New("start_day and end_day are both required")
	case days && *iv.EndDay < *iv.StartDay:
		return errors.New("end_day is before start_day")
	case stamps:
		start, end, err := iv.stamps()
		if err != nil {
			return err
		}
		if !end.After(start) {
			return errors.New("end must be after start")
		}
	case !days:
		return errors.New("start_day/end_day or start/end is required")
	}
	return nil
}

func (iv IntervalDef) stamps() (start, end time.Time, err error) {
	start, err = time.Parse(time.RFC3339, iv.Start)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}
	end, err = time.Parse(time.RFC3339, iv.End)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
	}
	return start.UTC(), end.UTC(), nil
}

// dayEnd is when an interval ends on its last day (Nebius: 23:59 UTC).
const dayEnd = 23*time.Hour + 59*time.Minute

// span returns the interval's time range for a catalog anchored at the UTC
// midnight anchor.
func (iv IntervalDef) span(anchor time.Time) (start, end time.Time) {
	if iv.StartDay == nil {
		start, end, _ = iv.stamps() // validated by ParseCatalog
		return start, end
	}
	day := 24 * time.Hour
	return anchor.Add(time.Duration(*iv.StartDay) * day), anchor.Add(time.Duration(*iv.EndDay)*day + dayEnd)
}

// BlockGroup is a seeded capacity block group.
type BlockGroup struct {
	ID        string
	TenantID  string
	Region    string
	Platform  string
	Fabric    string
	CreatedAt time.Time
	Intervals []Interval
}

// Interval is a seeded capacity interval: Quantity units in [Start, End).
type Interval struct {
	ID           string
	BlockGroupID string
	Quantity     uint64
	Start, End   time.Time
}

// Inventory is the catalog resolved for one mocker run: block groups with
// NIDs and absolute interval times, and the presets by platform.
type Inventory struct {
	groups    []*BlockGroup
	byID      map[string]*BlockGroup
	intervals map[string]*Interval
	presets   map[string]PlatformDef
}

// Build resolves the catalog. Relative interval days count from the UTC day
// of now; groups without a tenant belong to defaultTenant; routing maps each
// emulated region to its NID routing code, and groups in other regions are
// left out.
func (c Catalog) Build(now time.Time, defaultTenant string, routing map[string]string) (*Inventory, error) {
	anchor := now.UTC().Truncate(24 * time.Hour)
	inv := &Inventory{byID: map[string]*BlockGroup{}, intervals: map[string]*Interval{}, presets: map[string]PlatformDef{}}
	for _, d := range c.BlockGroups {
		code, ok := routing[d.Region]
		if !ok {
			continue
		}
		tenant := d.Tenant
		if tenant == "" {
			tenant = defaultTenant
		}
		id, err := nid.Deterministic(KindBlockGroup, code, tenant+"/"+d.Key)
		if err != nil {
			return nil, fmt.Errorf("block group %s: %w", d.Key, err)
		}
		g := &BlockGroup{ID: id, TenantID: tenant, Region: d.Region, Platform: d.Platform, Fabric: d.Fabric, CreatedAt: anchor}
		for j, iv := range d.Intervals {
			ivID, err := nid.Deterministic(KindInterval, code, fmt.Sprintf("%s/%s/%d", tenant, d.Key, j))
			if err != nil {
				return nil, fmt.Errorf("block group %s interval %d: %w", d.Key, j, err)
			}
			start, end := iv.span(anchor)
			g.Intervals = append(g.Intervals, Interval{ID: ivID, BlockGroupID: id, Quantity: iv.Quantity, Start: start, End: end})
			g.CreatedAt = minTime(g.CreatedAt, start)
		}
		slices.SortStableFunc(g.Intervals, func(a, b Interval) int { return a.Start.Compare(b.Start) })
		inv.groups = append(inv.groups, g)
		inv.byID[id] = g
		for i := range g.Intervals {
			inv.intervals[g.Intervals[i].ID] = &g.Intervals[i]
		}
	}
	for _, p := range c.Presets {
		inv.presets[p.Platform] = p
	}
	return inv, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// Group returns the block group with the ID.
func (inv *Inventory) Group(id string) (*BlockGroup, bool) {
	g, ok := inv.byID[id]
	return g, ok
}

// Interval returns the interval with the ID.
func (inv *Inventory) Interval(id string) (*Interval, bool) {
	iv, ok := inv.intervals[id]
	return iv, ok
}

// InTenant returns the tenant's block groups in catalog order.
func (inv *Inventory) InTenant(tenantID string) []*BlockGroup {
	var out []*BlockGroup
	for _, g := range inv.groups {
		if g.TenantID == tenantID {
			out = append(out, g)
		}
	}
	return out
}
