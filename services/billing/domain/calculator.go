package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// HoursPerMonth is the length of a month in the calculator's monthly costs
// (30 days).
const HoursPerMonth = 720

// OfferType is a price offer a request may ask for. The v1 and v1alpha1
// enums have the same values; the api layer converts both.
type OfferType int

// Offer types.
const (
	OfferTypeUnspecified OfferType = iota
	// OfferTypeContractPrice asks for the prices of the account's contract.
	// The mock has no contracts, so it is accepted and changes nothing:
	// every estimate uses the price list.
	OfferTypeContractPrice
)

// TimeUnit is a billing period of the v1 calculator's aggregation. The zero
// value means "unspecified" and is rejected.
type TimeUnit string

// Time units.
const (
	UnitMinute TimeUnit = "minute"
	UnitHour   TimeUnit = "hour"
	UnitDay    TimeUnit = "day"
	UnitWeek   TimeUnit = "week"
	UnitMonth  TimeUnit = "month"
)

// Hours is the length of the unit in hours (a month is 720 hours).
func (u TimeUnit) Hours() Decimal {
	switch u {
	case UnitHour:
		return DecimalInt(1)
	case UnitMinute:
		return DecimalInt(1).DivInt(60)
	case UnitDay:
		return DecimalInt(24)
	case UnitWeek:
		return DecimalInt(24 * 7)
	case UnitMonth:
		return DecimalInt(HoursPerMonth)
	default:
		return DecimalInt(1) // unspecified: validated away by Units
	}
}

// Item is one resource spec to price, already unpacked by the api layer.
// Exactly one of Instance, Disk and Filesystem is set; Update marks the
// Update*Request variants, whose project may be implied by metadata.id.
type Item struct {
	// Field is the request path of the spec message, used in violations,
	// e.g. "resource_specs[1].spec".
	Field    string
	Metadata *commonpb.ResourceMetadata
	Update   bool

	Instance   *computepb.InstanceSpec
	Disk       *computepb.DiskSpec
	Filesystem *computepb.FilesystemSpec
}

// SkuLine is the usage and cost of one SKU over some period.
type SkuLine struct {
	SKU      string
	Quantity Decimal
	Cost     Decimal
}

// Priced is the hourly cost of one Item.
type Priced struct {
	Metadata *commonpb.ResourceMetadata
	Lines    []SkuLine
}

// Scale returns the lines over hours (quantity and cost multiplied).
func Scale(lines []SkuLine, hours Decimal) []SkuLine {
	out := make([]SkuLine, len(lines))
	for i := range lines {
		l := &lines[i]
		out[i] = SkuLine{SKU: l.SKU, Quantity: l.Quantity.Mul(hours), Cost: l.Cost.Mul(hours)}
	}
	return out
}

// Merge sums lines per SKU, in order of first appearance.
func Merge(groups ...[]SkuLine) []SkuLine {
	var out []SkuLine
	idx := map[string]int{}
	for _, g := range groups {
		for j := range g {
			l := &g[j]
			i, ok := idx[l.SKU]
			if !ok {
				idx[l.SKU] = len(out)
				out = append(out, *l)
				continue
			}
			out[i].Quantity = out[i].Quantity.Add(l.Quantity)
			out[i].Cost = out[i].Cost.Add(l.Cost)
		}
	}
	return out
}

// Total is the sum of the costs of lines.
func Total(lines []SkuLine) Decimal {
	var sum Decimal
	for i := range lines {
		sum = sum.Add(lines[i].Cost)
	}
	return sum
}

// CalculatorDeps are the dependencies of a Calculator. All are required.
type CalculatorDeps struct {
	Prices    *PriceList
	Regions   Regions
	Platforms Platforms
	Hierarchy Hierarchy
	Config    Config
}

// Calculator prices resource specs (both CalculatorService versions) from the
// price list. It is stateless.
type Calculator struct{ d CalculatorDeps }

// NewCalculator wires the calculator.
func NewCalculator(d CalculatorDeps) (*Calculator, error) {
	switch {
	case d.Prices == nil:
		return nil, errors.New("billing calculator: prices are required")
	case d.Regions == nil, d.Platforms == nil, d.Hierarchy == nil:
		return nil, errors.New("billing calculator: regions, platforms and hierarchy are required")
	}
	return &Calculator{d: d}, nil
}

// Price returns the hourly cost of every item. offers are the requested offer
// types: the contract price is accepted but has no effect (the mock has no
// contract prices); an unspecified one is invalid.
func (c *Calculator) Price(ctx context.Context, items []Item, offers []OfferType) ([]Priced, error) {
	for i, o := range offers {
		if o != OfferTypeContractPrice {
			return nil, errs.BadRequest(errs.Violation{Field: fmt.Sprintf("offer_types[%d]", i), Message: "must be a specified offer type"})
		}
	}
	if len(items) == 0 {
		return nil, errs.BadRequest(errs.Violation{Field: "resource_specs", Message: "at least one resource spec is required"})
	}
	out := make([]Priced, 0, len(items))
	for _, it := range items {
		lines, err := c.priceItem(ctx, it)
		if err != nil {
			return nil, err
		}
		out = append(out, Priced{Metadata: it.Metadata, Lines: lines})
	}
	return out, nil
}

// Currency resolves the requested currency of a v1 request: empty or the
// price list's currency (case-insensitive).
func (c *Calculator) Currency(requested string) (string, error) {
	if requested == "" || strings.EqualFold(requested, c.d.Prices.Currency) {
		return c.d.Prices.Currency, nil
	}
	return "", errs.BadRequest(errs.Violation{
		Field:   "currency",
		Message: fmt.Sprintf("%q is not available: the price list is in %q and the mock does not convert currencies", requested, c.d.Prices.Currency),
	})
}

// Units validates the selected aggregation units of a v1 request, defaulting
// to hour and month. An empty unit (unspecified) is invalid.
func (*Calculator) Units(units []TimeUnit) ([]TimeUnit, error) {
	if len(units) == 0 {
		return []TimeUnit{UnitHour, UnitMonth}, nil
	}
	for i, u := range units {
		if u == "" {
			return nil, errs.BadRequest(errs.Violation{
				Field:   fmt.Sprintf("filter_aggregation_unit.filter_aggregation_unit_values[%d]", i),
				Message: "must be MINUTE, HOUR, DAY, WEEK or MONTH",
			})
		}
	}
	return units, nil
}

func (c *Calculator) priceItem(ctx context.Context, it Item) ([]SkuLine, error) {
	if it.Metadata == nil {
		return nil, errs.BadRequest(errs.Violation{Field: it.Field + ".metadata", Message: "required"})
	}
	project, err := c.project(ctx, it)
	if err != nil {
		return nil, err
	}
	switch {
	case it.Instance != nil:
		return c.priceInstance(ctx, it.Field+".spec", project, it.Instance)
	case it.Disk != nil:
		return c.priceDisk(it.Field+".spec", it.Disk)
	case it.Filesystem != nil:
		return c.priceFilesystem(it.Field+".spec", it.Filesystem)
	default:
		return nil, errs.BadRequest(errs.Violation{Field: it.Field + ".spec", Message: "required"})
	}
}

// project resolves the project an item is priced in: metadata.parent_id, or,
// for update specs without one, the parent of the resource metadata.id.
func (c *Calculator) project(ctx context.Context, it Item) (string, error) {
	const field = ".metadata.parent_id"
	parent := it.Metadata.GetParentId()
	if parent == "" && it.Update && it.Metadata.GetId() != "" {
		p, err := c.d.Hierarchy.Parent(ctx, it.Metadata.GetId())
		if err != nil {
			return "", fmt.Errorf("parent of %s: %w", it.Metadata.GetId(), err)
		}
		return p, nil
	}
	if parent == "" {
		return "", errs.BadRequest(errs.Violation{Field: it.Field + field, Message: "required"})
	}
	if !strings.HasPrefix(parent, KindProject+"-") {
		return "", errs.BadRequest(errs.Violation{Field: it.Field + field, Message: "must be a project ID"})
	}
	if parent == c.d.Config.ProjectID {
		return parent, nil
	}
	if _, err := c.d.Hierarchy.Parent(ctx, parent); err != nil {
		return "", fmt.Errorf("project %s: %w", parent, err)
	}
	return parent, nil
}

// priceInstance prices the compute of an instance. The disks and filesystems
// an instance references are separate resources and are priced as their own
// Disk and Filesystem specs, not here.
func (c *Calculator) priceInstance(ctx context.Context, field, project string, spec *computepb.InstanceSpec) ([]SkuLine, error) {
	res := spec.GetResources()
	if res.GetPlatform() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: field + ".resources.platform", Message: "required"})
	}
	if res.GetPreset() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: field + ".resources.preset", Message: "required"})
	}
	region, err := c.d.Regions.Region(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("region of %s: %w", project, err)
	}
	pl, ok, err := c.d.Platforms.Platform(ctx, project, res.GetPlatform())
	switch {
	case !ok:
		return nil, errs.BadRequest(errs.Violation{
			Field:   field + ".resources.platform",
			Message: "pricing instances needs the compute module's platform catalog, and compute is not enabled",
		})
	case errs.IsKind(err, errs.KindNotFound):
		return nil, errs.BadRequest(errs.Violation{
			Field:   field + ".resources.platform",
			Message: fmt.Sprintf("platform %q does not exist in region %s", res.GetPlatform(), region),
		})
	case err != nil:
		return nil, fmt.Errorf("platform %s: %w", res.GetPlatform(), err)
	}
	preset, found := findPreset(pl, res.GetPreset())
	if !found {
		return nil, errs.BadRequest(errs.Violation{
			Field:   field + ".resources.preset",
			Message: fmt.Sprintf("platform %q has no preset %q", pl.Name, res.GetPreset()),
		})
	}
	price, ok := c.d.Prices.Compute(pl.Name)
	if !ok {
		return nil, errs.BadRequest(errs.Violation{
			Field:   field + ".resources.platform",
			Message: fmt.Sprintf("the price list has no price for platform %q", pl.Name),
		})
	}
	if spec.GetPreemptible() != nil {
		_, spot, ok := c.d.Prices.SpotPrice(pl.Name, region)
		if !ok {
			return nil, errs.BadRequest(errs.Violation{
				Field:   field + ".preemptible",
				Message: fmt.Sprintf("platform %q has no spot market in region %s", pl.Name, region),
			})
		}
		return appendLine(nil, price.Spot.SKU, DecimalInt(int64(preset.GPUs)), spot), nil
	}
	if price.GPU != nil {
		return appendLine(nil, price.GPU.SKU, DecimalInt(int64(preset.GPUs)), price.GPU.Price), nil
	}
	var lines []SkuLine
	lines = appendLine(lines, price.VCPU.SKU, DecimalInt(int64(preset.VCPUs)), price.VCPU.Price)
	lines = appendLine(lines, price.Memory.SKU, DecimalInt(preset.MemoryGiB), price.Memory.Price)
	return lines, nil
}

func findPreset(p Platform, name string) (Preset, bool) {
	for _, pr := range p.Presets {
		if pr.Name == name {
			return pr, true
		}
	}
	return Preset{}, false
}

// appendLine adds a line for qty units at unitPrice; a zero quantity (a CPU
// preset has no GPUs) adds nothing.
func appendLine(lines []SkuLine, sku string, qty, unitPrice Decimal) []SkuLine {
	if qty.IsZero() {
		return lines
	}
	return append(lines, SkuLine{SKU: sku, Quantity: qty, Cost: qty.Mul(unitPrice)})
}

func (c *Calculator) priceDisk(field string, spec *computepb.DiskSpec) ([]SkuLine, error) {
	if spec.GetType() == computepb.DiskSpec_UNSPECIFIED {
		return nil, errs.BadRequest(errs.Violation{Field: field + ".type", Message: "required"})
	}
	price, ok := c.d.Prices.Disk(spec.GetType())
	if !ok {
		return nil, errs.BadRequest(errs.Violation{
			Field:   field + ".type",
			Message: fmt.Sprintf("the price list has no price for disk type %s", spec.GetType()),
		})
	}
	var n, per int64
	switch s := spec.GetSize().(type) {
	case *computepb.DiskSpec_SizeGibibytes:
		n, per = s.SizeGibibytes, 1
	case *computepb.DiskSpec_SizeMebibytes:
		n, per = s.SizeMebibytes, 1<<10
	case *computepb.DiskSpec_SizeKibibytes:
		n, per = s.SizeKibibytes, 1<<20
	case *computepb.DiskSpec_SizeBytes:
		n, per = s.SizeBytes, 1<<30
	}
	gib, err := sizeGiB(field+".size", n, per)
	if err != nil {
		return nil, err
	}
	return appendLine(nil, price.SKU, gib, price.Price), nil
}

func (c *Calculator) priceFilesystem(field string, spec *computepb.FilesystemSpec) ([]SkuLine, error) {
	if spec.GetType() == computepb.FilesystemSpec_UNSPECIFIED {
		return nil, errs.BadRequest(errs.Violation{Field: field + ".type", Message: "required"})
	}
	price, ok := c.d.Prices.Filesystem(spec.GetType())
	if !ok {
		return nil, errs.BadRequest(errs.Violation{
			Field:   field + ".type",
			Message: fmt.Sprintf("the price list has no price for filesystem type %s", spec.GetType()),
		})
	}
	var n, per int64
	switch s := spec.GetSize().(type) {
	case *computepb.FilesystemSpec_SizeGibibytes:
		n, per = s.SizeGibibytes, 1
	case *computepb.FilesystemSpec_SizeMebibytes:
		n, per = s.SizeMebibytes, 1<<10
	case *computepb.FilesystemSpec_SizeKibibytes:
		n, per = s.SizeKibibytes, 1<<20
	case *computepb.FilesystemSpec_SizeBytes:
		n, per = s.SizeBytes, 1<<30
	}
	gib, err := sizeGiB(field+".size", n, per)
	if err != nil {
		return nil, err
	}
	return appendLine(nil, price.SKU, gib, price.Price), nil
}

// sizeGiB is n/per GiB exactly. per is 0 when the spec sets no size, e.g. a
// disk sized by its source image.
func sizeGiB(field string, n, per int64) (Decimal, error) {
	if per == 0 {
		return Decimal{}, errs.BadRequest(errs.Violation{
			Field: field,
			Message: "the calculator needs an explicit size (size_gibibytes, size_mebibytes, size_kibibytes or size_bytes); " +
				"it cannot derive one from a source image or snapshot",
		})
	}
	if n <= 0 {
		return Decimal{}, errs.BadRequest(errs.Violation{Field: field, Message: "must be positive"})
	}
	return DecimalRatio(n, per), nil
}
