package domain

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

//go:embed prices.yaml
var defaultPricesYAML []byte

// KindSKU is the NID type of SKUs.
const KindSKU = "sku"

// PriceFile is the YAML/JSON form of the price list (prices.yaml).
type PriceFile struct {
	Currency    string                     `yaml:"currency" json:"currency"`
	Compute     []ComputePriceDef          `yaml:"compute" json:"compute"`
	Disks       map[string]StoragePriceDef `yaml:"disks" json:"disks"`
	Filesystems map[string]StoragePriceDef `yaml:"filesystems" json:"filesystems"`
}

// ComputePriceDef prices one compute platform.
type ComputePriceDef struct {
	Platform      string         `yaml:"platform" json:"platform"`
	ProductTitle  string         `yaml:"product_title" json:"product_title"`
	GPUHour       string         `yaml:"gpu_hour" json:"gpu_hour"`
	VCPUHour      string         `yaml:"vcpu_hour" json:"vcpu_hour"`
	MemoryGiBHour string         `yaml:"memory_gib_hour" json:"memory_gib_hour"`
	Preemptible   *SpotMarketDef `yaml:"preemptible" json:"preemptible"`
}

// SpotMarketDef is the spot market of a preemptible platform.
type SpotMarketDef struct {
	ProductTitle string            `yaml:"product_title" json:"product_title"`
	MinPrice     string            `yaml:"min_price" json:"min_price"`
	MaxPrice     string            `yaml:"max_price" json:"max_price"`
	Spot         map[string]string `yaml:"spot" json:"spot"`
}

// StoragePriceDef prices one disk or filesystem type per GiB-hour.
type StoragePriceDef struct {
	ProductTitle string `yaml:"product_title" json:"product_title"`
	GiBHour      string `yaml:"gib_hour" json:"gib_hour"`
}

// UnitPrice is one SKU and its price per unit-hour.
type UnitPrice struct {
	SKU   string
	Price Decimal
}

// ComputePrice is the resolved price of one compute platform. A GPU platform
// has GPU set; a non-GPU platform has VCPU and Memory.
type ComputePrice struct {
	Platform     string
	ProductTitle string
	GPU          *UnitPrice
	VCPU         *UnitPrice
	Memory       *UnitPrice
	// Spot is the platform's spot market (nil when it has none).
	Spot *SpotMarket
}

// SpotMarket is the spot market of one preemptible platform: one SKU, the
// current per-GPU-hour price in each region, and the range a pricing
// policy's max_price must fall in.
type SpotMarket struct {
	SKU          string
	ProductTitle string
	MinPrice     Decimal
	MaxPrice     Decimal
	// Prices maps a region to its current spot price per GPU-hour.
	Prices map[string]Decimal
}

// StoragePrice is the price of a disk or filesystem type per GiB-hour.
type StoragePrice struct {
	ProductTitle string
	UnitPrice
}

// PriceList is the immutable, validated price list.
type PriceList struct {
	Currency    string
	compute     map[string]*ComputePrice
	platforms   []string // sorted
	disks       map[computepb.DiskSpec_DiskType]StoragePrice
	filesystems map[computepb.FilesystemSpec_FilesystemType]StoragePrice
}

// DefaultPriceList returns the built-in price list for the given regions.
func DefaultPriceList(regions []string, routingCode string) (*PriceList, error) {
	return ParsePriceList(defaultPricesYAML, regions, routingCode)
}

// ParsePriceList decodes and validates a YAML (or JSON) price list. Spot
// prices of regions outside regions are dropped. routingCode is the NID
// routing code of the derived SKU IDs.
func ParsePriceList(data []byte, regions []string, routingCode string) (*PriceList, error) {
	var f PriceFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode price list: %w", err)
	}
	pl, err := buildPriceList(f, regions, routingCode)
	if err != nil {
		return nil, fmt.Errorf("price list: %w", err)
	}
	return pl, nil
}

func buildPriceList(f PriceFile, regions []string, rc string) (*PriceList, error) {
	if f.Currency == "" {
		return nil, errors.New("currency is required")
	}
	pl := &PriceList{
		Currency:    strings.ToLower(f.Currency),
		compute:     map[string]*ComputePrice{},
		disks:       map[computepb.DiskSpec_DiskType]StoragePrice{},
		filesystems: map[computepb.FilesystemSpec_FilesystemType]StoragePrice{},
	}
	for i, d := range f.Compute {
		cp, err := buildCompute(d, regions, rc)
		if err != nil {
			return nil, fmt.Errorf("compute[%d] %s: %w", i, d.Platform, err)
		}
		if _, dup := pl.compute[d.Platform]; dup {
			return nil, fmt.Errorf("compute[%d]: duplicate platform %q", i, d.Platform)
		}
		pl.compute[d.Platform] = cp
		pl.platforms = append(pl.platforms, d.Platform)
	}
	slices.Sort(pl.platforms)
	for name, d := range f.Disks {
		t, ok := computepb.DiskSpec_DiskType_value[name]
		if !ok || t == 0 {
			return nil, fmt.Errorf("disks: unknown disk type %q", name)
		}
		sp, err := buildStorage("disk/"+name, d, rc)
		if err != nil {
			return nil, fmt.Errorf("disks.%s: %w", name, err)
		}
		pl.disks[computepb.DiskSpec_DiskType(t)] = sp
	}
	for name, d := range f.Filesystems {
		t, ok := computepb.FilesystemSpec_FilesystemType_value[name]
		if !ok || t == 0 {
			return nil, fmt.Errorf("filesystems: unknown filesystem type %q", name)
		}
		sp, err := buildStorage("filesystem/"+name, d, rc)
		if err != nil {
			return nil, fmt.Errorf("filesystems.%s: %w", name, err)
		}
		pl.filesystems[computepb.FilesystemSpec_FilesystemType(t)] = sp
	}
	return pl, nil
}

func buildCompute(d ComputePriceDef, regions []string, rc string) (*ComputePrice, error) {
	if d.Platform == "" {
		return nil, errors.New("platform is required")
	}
	cp := &ComputePrice{Platform: d.Platform, ProductTitle: d.ProductTitle}
	var err error
	switch {
	case d.GPUHour != "" && d.VCPUHour == "" && d.MemoryGiBHour == "":
		if cp.GPU, err = unitPrice("compute/"+d.Platform+"/gpu", d.GPUHour, rc); err != nil {
			return nil, fmt.Errorf("gpu_hour: %w", err)
		}
	case d.GPUHour == "" && d.VCPUHour != "" && d.MemoryGiBHour != "":
		if cp.VCPU, err = unitPrice("compute/"+d.Platform+"/vcpu", d.VCPUHour, rc); err != nil {
			return nil, fmt.Errorf("vcpu_hour: %w", err)
		}
		if cp.Memory, err = unitPrice("compute/"+d.Platform+"/memory", d.MemoryGiBHour, rc); err != nil {
			return nil, fmt.Errorf("memory_gib_hour: %w", err)
		}
	default:
		return nil, errors.New("set either gpu_hour, or both vcpu_hour and memory_gib_hour")
	}
	if d.Preemptible == nil {
		return cp, nil
	}
	if cp.GPU == nil {
		return nil, errors.New("preemptible: spot markets are priced per GPU; set gpu_hour")
	}
	if cp.Spot, err = buildSpot(d.Platform, *d.Preemptible, regions, rc); err != nil {
		return nil, fmt.Errorf("preemptible: %w", err)
	}
	return cp, nil
}

func buildSpot(platform string, d SpotMarketDef, regions []string, rc string) (*SpotMarket, error) {
	sku, err := nid.Deterministic(KindSKU, rc, "compute/"+platform+"/preemptible")
	if err != nil {
		return nil, err
	}
	m := &SpotMarket{SKU: sku, ProductTitle: d.ProductTitle, Prices: map[string]Decimal{}}
	if m.MinPrice, err = ParseDecimal(d.MinPrice); err != nil {
		return nil, fmt.Errorf("min_price: %w", err)
	}
	if m.MaxPrice, err = ParseDecimal(d.MaxPrice); err != nil {
		return nil, fmt.Errorf("max_price: %w", err)
	}
	if m.MinPrice.Cmp(m.MaxPrice) > 0 {
		return nil, errors.New("min_price is above max_price")
	}
	for region, s := range d.Spot {
		p, err := ParseDecimal(s)
		if err != nil {
			return nil, fmt.Errorf("spot.%s: %w", region, err)
		}
		if slices.Contains(regions, region) {
			m.Prices[region] = p
		}
	}
	return m, nil
}

func buildStorage(key string, d StoragePriceDef, rc string) (StoragePrice, error) {
	up, err := unitPrice(key, d.GiBHour, rc)
	if err != nil {
		return StoragePrice{}, fmt.Errorf("gib_hour: %w", err)
	}
	return StoragePrice{ProductTitle: d.ProductTitle, UnitPrice: *up}, nil
}

func unitPrice(key, price, rc string) (*UnitPrice, error) {
	p, err := ParseDecimal(price)
	if err != nil {
		return nil, err
	}
	sku, err := nid.Deterministic(KindSKU, rc, key)
	if err != nil {
		return nil, err
	}
	return &UnitPrice{SKU: sku, Price: p}, nil
}

// Compute returns the price of a platform.
func (p *PriceList) Compute(platform string) (*ComputePrice, bool) {
	c, ok := p.compute[platform]
	return c, ok
}

// Disk returns the price of a disk type.
func (p *PriceList) Disk(t computepb.DiskSpec_DiskType) (StoragePrice, bool) {
	d, ok := p.disks[t]
	return d, ok
}

// Filesystem returns the price of a filesystem type.
func (p *PriceList) Filesystem(t computepb.FilesystemSpec_FilesystemType) (StoragePrice, bool) {
	d, ok := p.filesystems[t]
	return d, ok
}

// SpotMarkets returns the platforms with a spot market, sorted by platform.
func (p *PriceList) SpotMarkets() []*ComputePrice {
	var out []*ComputePrice
	for _, name := range p.platforms {
		if c := p.compute[name]; c.Spot != nil {
			out = append(out, c)
		}
	}
	return out
}

// SpotPrice returns the platform's price entry and its current spot price in
// region; ok is false when the platform has no spot market there.
func (p *PriceList) SpotPrice(platform, region string) (cp *ComputePrice, price Decimal, ok bool) {
	c, found := p.compute[platform]
	if !found || c.Spot == nil {
		return nil, Decimal{}, false
	}
	price, ok = c.Spot.Prices[region]
	return c, price, ok
}
