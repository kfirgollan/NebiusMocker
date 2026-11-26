package domain_test

import (
	"errors"
	"strings"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

func calcInstance(project, platform, preset string) domain.Item {
	spec := &computepb.InstanceSpec{Resources: &computepb.ResourcesSpec{
		Platform: platform, Size: &computepb.ResourcesSpec_Preset{Preset: preset},
	}}
	return domain.Item{
		Field: "resource_specs[0]", Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: "vm"}, Instance: spec,
	}
}

func calcDisk(typ computepb.DiskSpec_DiskType, size any) domain.Item {
	spec := &computepb.DiskSpec{Type: typ}
	switch s := size.(type) {
	case *computepb.DiskSpec_SizeGibibytes:
		spec.Size = s
	case *computepb.DiskSpec_SizeMebibytes:
		spec.Size = s
	case *computepb.DiskSpec_SizeKibibytes:
		spec.Size = s
	case *computepb.DiskSpec_SizeBytes:
		spec.Size = s
	}
	return domain.Item{Field: "resource_specs[0]", Metadata: &commonpb.ResourceMetadata{ParentId: calcProject, Name: "d"}, Disk: spec}
}

func calcFS(typ computepb.FilesystemSpec_FilesystemType, gib int64) domain.Item {
	return domain.Item{
		Field: "resource_specs[0]", Metadata: &commonpb.ResourceMetadata{ParentId: calcProject, Name: "fs"},
		Filesystem: &computepb.FilesystemSpec{Type: typ, Size: &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: gib}},
	}
}

func calcWantBad(t *testing.T, err error, field, contains string) {
	t.Helper()
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || e.Kind != errs.KindBadRequest {
		t.Fatalf("err = %v, want BadRequest on %s", err, field)
	}
	for _, v := range e.Violations {
		if v.Field == field && strings.Contains(v.Message, contains) {
			return
		}
	}
	t.Fatalf("violations %+v lack field %s containing %q", e.Violations, field, contains)
}

func TestCalculatorPrices(t *testing.T) {
	const h100, cpu = "gpu-h100-sxm", "cpu-e2"
	tests := []struct {
		name        string
		item        domain.Item
		hourly      string
		wantSKUQtys []string
	}{
		{"h100 8 gpu on demand", calcInstance(calcProject, h100, "8gpu-128vcpu-1600gb"), "23.6", []string{"8"}},
		{"h100 1 gpu", calcInstance(calcProject, h100, "1gpu-16vcpu-200gb"), "2.95", []string{"1"}},
		{"h100 8 gpu preemptible in eu-north1", calcSpotInstance(calcProject, h100, "8gpu-128vcpu-1600gb"), "10", []string{"8"}},
		{"cpu-e2 4vcpu-16gb", calcInstance(calcProject, cpu, "4vcpu-16gb"), "0.0832", []string{"4", "16"}},
		{"ssd disk 100 GiB", calcDisk(computepb.DiskSpec_NETWORK_SSD, &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 100}), "0.00986", []string{"100"}},
		{"disk 1024 MiB is 1 GiB", calcDisk(computepb.DiskSpec_NETWORK_HDD, &computepb.DiskSpec_SizeMebibytes{SizeMebibytes: 1024}), "0.0000417", []string{"1"}},
		{"disk half GiB in KiB", calcDisk(computepb.DiskSpec_NETWORK_HDD, &computepb.DiskSpec_SizeKibibytes{SizeKibibytes: 1 << 19}), "0.00002085", []string{"0.5"}},
		{"disk 2 GiB in bytes", calcDisk(computepb.DiskSpec_NETWORK_SSD_IO_M3, &computepb.DiskSpec_SizeBytes{SizeBytes: 2 << 30}), "0.0002638", []string{"2"}},
		{"filesystem ssd 1000 GiB", calcFS(computepb.FilesystemSpec_NETWORK_SSD, 1000), "0.1111", []string{"1000"}},
	}
	c := newCalc(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Price(t.Context(), []domain.Item{tc.item}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if h := domain.Total(got[0].Lines).String(); h != tc.hourly {
				t.Fatalf("hourly = %s, want %s", h, tc.hourly)
			}
			if len(got[0].Lines) != len(tc.wantSKUQtys) {
				t.Fatalf("lines = %d, want %d", len(got[0].Lines), len(tc.wantSKUQtys))
			}
			for i, q := range tc.wantSKUQtys {
				if got[0].Lines[i].Quantity.String() != q || !strings.HasPrefix(got[0].Lines[i].SKU, "sku-") {
					t.Errorf("line %d = %+v, want quantity %s and a sku- ID", i, got[0].Lines[i], q)
				}
			}
		})
	}
}

func TestCalculatorMonthlyIs720Hours(t *testing.T) {
	c := newCalc(t)
	got, err := c.Price(t.Context(), []domain.Item{calcInstance(calcProject, "gpu-h100-sxm", "8gpu-128vcpu-1600gb")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	month := domain.Scale(got[0].Lines, domain.UnitMonth.Hours())
	if s := domain.Total(month).String(); s != "16992" {
		t.Fatalf("monthly = %s, want 16992", s)
	}
	minute := domain.Total(domain.Scale(got[0].Lines, domain.UnitMinute.Hours()))
	if r := minute.Rounded(); r != "0.40" {
		t.Fatalf("per minute rounded = %s, want 0.40", r)
	}
}

func TestCalculatorProjectsAndRegions(t *testing.T) {
	c := newCalc(t)
	// The other project is in us-central1, which has an H100 on-demand price
	// but no H100 spot market.
	if _, err := c.Price(t.Context(), []domain.Item{calcInstance(calcOtherProject, "gpu-h100-sxm", "1gpu-16vcpu-200gb")}, nil); err != nil {
		t.Fatalf("on-demand in us-central1: %v", err)
	}
	_, err := c.Price(t.Context(), []domain.Item{calcSpotInstance(calcOtherProject, "gpu-h100-sxm", "1gpu-16vcpu-200gb")}, nil)
	calcWantBad(t, err, "resource_specs[0].spec.preemptible", "no spot market")

	_, err = c.Price(t.Context(), []domain.Item{calcInstance(calcGhostProject, "cpu-e2", "4vcpu-16gb")}, nil)
	if !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("unknown project: %v, want NotFound", err)
	}
	_, err = c.Price(t.Context(), []domain.Item{calcInstance("tenant-e00mock0000000000default", "cpu-e2", "4vcpu-16gb")}, nil)
	calcWantBad(t, err, "resource_specs[0].metadata.parent_id", "project ID")
	_, err = c.Price(t.Context(), []domain.Item{calcInstance("", "cpu-e2", "4vcpu-16gb")}, nil)
	calcWantBad(t, err, "resource_specs[0].metadata.parent_id", "required")
}

func TestCalculatorUpdateSpecUsesParentOfID(t *testing.T) {
	c := newCalc(t)
	it := calcSpotInstance("", "gpu-h100-sxm", "1gpu-16vcpu-200gb")
	it.Update = true
	it.Metadata = &commonpb.ResourceMetadata{Id: calcInstanceID}
	// The instance lives in the other project (us-central1): no H100 spot there.
	_, err := c.Price(t.Context(), []domain.Item{it}, nil)
	calcWantBad(t, err, "resource_specs[0].spec.preemptible", "us-central1")

	it.Metadata = &commonpb.ResourceMetadata{Id: "computeinstance-e00test000000099"}
	if _, err := c.Price(t.Context(), []domain.Item{it}, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("unknown instance: %v, want NotFound", err)
	}
	// A create spec does not fall back to the id.
	it.Update = false
	it.Metadata = &commonpb.ResourceMetadata{Id: calcInstanceID}
	_, err = c.Price(t.Context(), []domain.Item{it}, nil)
	calcWantBad(t, err, "resource_specs[0].metadata.parent_id", "required")
}

func TestCalculatorInstanceValidation(t *testing.T) {
	c := newCalc(t)
	tests := []struct {
		name         string
		item         domain.Item
		field, wants string
	}{
		{"platform required", calcInstance(calcProject, "", "4vcpu-16gb"), "resource_specs[0].spec.resources.platform", "required"},
		{"preset required", calcInstance(calcProject, "cpu-e2", ""), "resource_specs[0].spec.resources.preset", "required"},
		{"unknown platform", calcInstance(calcProject, "cpu-nope", "4vcpu-16gb"), "resource_specs[0].spec.resources.platform", "cpu-nope"},
		{"unknown preset", calcInstance(calcProject, "cpu-e2", "99vcpu"), "resource_specs[0].spec.resources.preset", "99vcpu"},
		{"platform without a price", calcInstance(calcProject, "cpu-unpriced", "2vcpu-8gb"), "resource_specs[0].spec.resources.platform", "no price"},
		{"preemptible cpu platform", calcSpotInstance(calcProject, "cpu-e2", "4vcpu-16gb"), "resource_specs[0].spec.preemptible", "no spot market"},
		{"no resources", domain.Item{Field: "resource_specs[0]", Metadata: &commonpb.ResourceMetadata{ParentId: calcProject}, Instance: &computepb.InstanceSpec{}}, "resource_specs[0].spec.resources.platform", "required"},
		{"no metadata", domain.Item{Field: "resource_specs[0]", Instance: &computepb.InstanceSpec{}}, "resource_specs[0].metadata", "required"},
		{"empty item", domain.Item{Field: "resource_specs[0]", Metadata: &commonpb.ResourceMetadata{ParentId: calcProject}}, "resource_specs[0].spec", "required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Price(t.Context(), []domain.Item{tc.item}, nil)
			calcWantBad(t, err, tc.field, tc.wants)
		})
	}
}

func TestCalculatorInstanceNeedsComputeCatalog(t *testing.T) {
	c := newCalcFor(t, calcPlatforms{disabled: true})
	_, err := c.Price(t.Context(), []domain.Item{calcInstance(calcProject, "cpu-e2", "4vcpu-16gb")}, nil)
	calcWantBad(t, err, "resource_specs[0].spec.resources.platform", "compute is not enabled")
	// Disks do not need the catalog.
	if _, err := c.Price(t.Context(), []domain.Item{calcDisk(computepb.DiskSpec_NETWORK_SSD, &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 10})}, nil); err != nil {
		t.Fatalf("disk without compute: %v", err)
	}
}

func TestCalculatorStorageValidation(t *testing.T) {
	c := newCalc(t)
	imageDisk := calcDisk(computepb.DiskSpec_NETWORK_SSD, nil)
	imageDisk.Disk.Source = &computepb.DiskSpec_SourceImageFamily{SourceImageFamily: &computepb.SourceImageFamily{ImageFamily: "ubuntu22.04-driverless"}}
	tests := []struct {
		name         string
		item         domain.Item
		field, wants string
	}{
		{"disk type required", calcDisk(computepb.DiskSpec_UNSPECIFIED, &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 1}), "resource_specs[0].spec.type", "required"},
		{"disk size derived from image", imageDisk, "resource_specs[0].spec.size", "explicit size"},
		{"disk size zero", calcDisk(computepb.DiskSpec_NETWORK_SSD, &computepb.DiskSpec_SizeGibibytes{}), "resource_specs[0].spec.size", "positive"},
		{"disk size negative", calcDisk(computepb.DiskSpec_NETWORK_SSD, &computepb.DiskSpec_SizeBytes{SizeBytes: -5}), "resource_specs[0].spec.size", "positive"},
		{"filesystem type required", calcFS(computepb.FilesystemSpec_UNSPECIFIED, 1), "resource_specs[0].spec.type", "required"},
		{"filesystem type without a price", calcFS(computepb.FilesystemSpec_WEKA, 1), "resource_specs[0].spec.type", "no price"},
		{"filesystem size missing", domain.Item{Field: "resource_specs[0]", Metadata: &commonpb.ResourceMetadata{ParentId: calcProject}, Filesystem: &computepb.FilesystemSpec{Type: computepb.FilesystemSpec_NETWORK_SSD}}, "resource_specs[0].spec.size", "explicit size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Price(t.Context(), []domain.Item{tc.item}, nil)
			calcWantBad(t, err, tc.field, tc.wants)
		})
	}
}

func TestCalculatorOfferTypesAndEmptyBatch(t *testing.T) {
	c := newCalc(t)
	items := []domain.Item{calcDisk(computepb.DiskSpec_NETWORK_SSD, &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 100})}

	plain, err := c.Price(t.Context(), items, nil)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := c.Price(t.Context(), items, []domain.OfferType{domain.OfferTypeContractPrice})
	if err != nil {
		t.Fatalf("contract price offer: %v", err)
	}
	if domain.Total(contract[0].Lines).Cmp(domain.Total(plain[0].Lines)) != 0 {
		t.Error("the contract price offer changed the estimate; the mock has no contract prices")
	}

	_, err = c.Price(t.Context(), items, []domain.OfferType{domain.OfferTypeContractPrice, domain.OfferTypeUnspecified})
	calcWantBad(t, err, "offer_types[1]", "specified")
	_, err = c.Price(t.Context(), nil, nil)
	calcWantBad(t, err, "resource_specs", "at least one")
}

func TestCalculatorCurrencyAndUnits(t *testing.T) {
	c := newCalc(t)
	for _, in := range []string{"", "usd", "USD", "Usd"} {
		if got, err := c.Currency(in); err != nil || got != "usd" {
			t.Errorf("Currency(%q) = %q, %v; want usd", in, got, err)
		}
	}
	_, err := c.Currency("eur")
	calcWantBad(t, err, "currency", "eur")

	def, err := c.Units(nil)
	if err != nil || len(def) != 2 || def[0] != domain.UnitHour || def[1] != domain.UnitMonth {
		t.Fatalf("default units = %v, %v; want hour and month", def, err)
	}
	_, err = c.Units([]domain.TimeUnit{domain.UnitDay, ""})
	calcWantBad(t, err, "filter_aggregation_unit.filter_aggregation_unit_values[1]", "MINUTE")
}

func TestMergeSumsPerSKUInFirstSeenOrder(t *testing.T) {
	a := []domain.SkuLine{{SKU: "a", Quantity: domain.DecimalInt(1), Cost: mustDec(t, "1.5")}, {SKU: "b", Quantity: domain.DecimalInt(2), Cost: mustDec(t, "2")}}
	b := []domain.SkuLine{{SKU: "b", Quantity: domain.DecimalInt(3), Cost: mustDec(t, "3")}, {SKU: "c", Quantity: domain.DecimalInt(1), Cost: mustDec(t, "0.5")}}
	got := domain.Merge(a, b)
	if len(got) != 3 || got[0].SKU != "a" || got[1].SKU != "b" || got[2].SKU != "c" {
		t.Fatalf("merged = %+v", got)
	}
	if got[1].Quantity.String() != "5" || got[1].Cost.String() != "5" {
		t.Errorf("b = %+v, want quantity 5 and cost 5", got[1])
	}
	if domain.Total(got).String() != "7" {
		t.Errorf("total = %s, want 7", domain.Total(got))
	}
}

// calcSpotInstance is calcInstance asking for a preemptible VM.
func calcSpotInstance(project, platform, preset string) domain.Item {
	it := calcInstance(project, platform, preset)
	it.Instance.Preemptible = &computepb.PreemptibleSpec{}
	return it
}
