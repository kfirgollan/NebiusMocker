package domain_test

import (
	"strings"
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

func TestDefaultPriceListParses(t *testing.T) {
	pl := calcDefaultPrices(t)
	if pl.Currency != "usd" {
		t.Errorf("currency = %q, want usd", pl.Currency)
	}
	h100, ok := pl.Compute("gpu-h100-sxm")
	if !ok || h100.GPU == nil || h100.GPU.Price.String() != "2.95" || h100.Spot == nil {
		t.Fatalf("h100 = %+v", h100)
	}
	if _, price, ok := pl.SpotPrice("gpu-h100-sxm", "eu-north1"); !ok || price.String() != "1.25" {
		t.Errorf("h100 spot in eu-north1 = %s, %v", price, ok)
	}
	if _, _, ok := pl.SpotPrice("gpu-h100-sxm", "us-central1"); ok {
		t.Error("h100 has no spot price in us-central1")
	}
	cpu, ok := pl.Compute("cpu-e2")
	if !ok || cpu.VCPU == nil || cpu.Memory == nil || cpu.GPU != nil || cpu.Spot != nil {
		t.Fatalf("cpu-e2 = %+v", cpu)
	}
	if _, ok := pl.Disk(computepb.DiskSpec_NETWORK_SSD); !ok {
		t.Error("no NETWORK_SSD disk price")
	}
	if _, ok := pl.Filesystem(computepb.FilesystemSpec_NETWORK_HDD); !ok {
		t.Error("no NETWORK_HDD filesystem price")
	}
	if len(pl.SpotMarkets()) == 0 {
		t.Error("no spot markets")
	}
}

func TestPriceListSKUsAreStable(t *testing.T) {
	a, b := calcDefaultPrices(t), calcDefaultPrices(t)
	pa, _ := a.Compute("gpu-h100-sxm")
	pb, _ := b.Compute("gpu-h100-sxm")
	if pa.GPU.SKU != pb.GPU.SKU || pa.GPU.SKU == pa.Spot.SKU || !strings.HasPrefix(pa.GPU.SKU, "sku-") {
		t.Fatalf("on-demand SKU %s, spot SKU %s", pa.GPU.SKU, pa.Spot.SKU)
	}
}

func TestPriceListOverrides(t *testing.T) {
	const file = `
currency: EUR
compute:
  - platform: gpu-x
    product_title: "X"
    gpu_hour: "1.5"
    preemptible:
      min_price: "0.1"
      max_price: "9"
      spot: { eu-north1: "0.75", mars-1: "0.5" }
disks:
  NETWORK_HDD: { product_title: "HDD", gib_hour: "0.01" }
`
	pl, err := domain.ParsePriceList([]byte(file), []string{"eu-north1"}, "e00")
	if err != nil {
		t.Fatal(err)
	}
	if pl.Currency != "eur" {
		t.Errorf("currency = %q, want eur (lowercased)", pl.Currency)
	}
	x, ok := pl.Compute("gpu-x")
	if !ok || x.Spot == nil || len(x.Spot.Prices) != 1 {
		t.Fatalf("gpu-x = %+v; spot prices of unconfigured regions must be dropped", x)
	}
	if _, ok := pl.Compute("gpu-h100-sxm"); ok {
		t.Error("an override replaces the built-in list entirely")
	}
	if _, ok := pl.Disk(computepb.DiskSpec_NETWORK_SSD); ok {
		t.Error("NETWORK_SSD is not in the override")
	}
}

func TestInvalidPriceListsAreRejected(t *testing.T) {
	tests := []struct{ name, file, wants string }{
		{"not yaml", "currency: [", "decode"},
		{"unknown field", "currency: usd\nbogus: 1\n", "bogus"},
		{"missing currency", "compute: []\n", "currency"},
		{"price is not a decimal", "currency: usd\ncompute:\n  - {platform: p, gpu_hour: \"abc\"}\n", "gpu_hour"},
		{"negative price", "currency: usd\ncompute:\n  - {platform: p, gpu_hour: \"-1\"}\n", "gpu_hour"},
		{"neither gpu nor cpu prices", "currency: usd\ncompute:\n  - {platform: p}\n", "gpu_hour"},
		{"gpu and cpu prices", "currency: usd\ncompute:\n  - {platform: p, gpu_hour: \"1\", vcpu_hour: \"1\", memory_gib_hour: \"1\"}\n", "gpu_hour"},
		{"vcpu without memory", "currency: usd\ncompute:\n  - {platform: p, vcpu_hour: \"1\"}\n", "gpu_hour"},
		{"missing platform", "currency: usd\ncompute:\n  - {gpu_hour: \"1\"}\n", "platform is required"},
		{"duplicate platform", "currency: usd\ncompute:\n  - {platform: p, gpu_hour: \"1\"}\n  - {platform: p, gpu_hour: \"2\"}\n", "duplicate"},
		{"min above max", "currency: usd\ncompute:\n  - {platform: p, gpu_hour: \"1\", preemptible: {min_price: \"5\", max_price: \"2\"}}\n", "min_price"},
		{"preemptible cpu platform", "currency: usd\ncompute:\n  - {platform: p, vcpu_hour: \"1\", memory_gib_hour: \"1\", preemptible: {min_price: \"1\", max_price: \"2\"}}\n", "preemptible"},
		{"bad spot price", "currency: usd\ncompute:\n  - {platform: p, gpu_hour: \"1\", preemptible: {min_price: \"1\", max_price: \"2\", spot: {eu-north1: x}}}\n", "spot.eu-north1"},
		{"unknown disk type", "currency: usd\ndisks:\n  NETWORK_TAPE: {gib_hour: \"1\"}\n", "NETWORK_TAPE"},
		{"unspecified disk type", "currency: usd\ndisks:\n  UNSPECIFIED: {gib_hour: \"1\"}\n", "UNSPECIFIED"},
		{"unknown filesystem type", "currency: usd\nfilesystems:\n  FLOPPY: {gib_hour: \"1\"}\n", "FLOPPY"},
		{"bad disk price", "currency: usd\ndisks:\n  NETWORK_SSD: {gib_hour: \"\"}\n", "gib_hour"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.ParsePriceList([]byte(tc.file), []string{"eu-north1"}, "e00")
			if err == nil {
				t.Fatal("accepted an invalid price list")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error %q does not mention %q", err, tc.wants)
			}
		})
	}
}
