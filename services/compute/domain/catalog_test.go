package domain

import (
	"testing"
	"time"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

func TestDefaultCatalog(t *testing.T) {
	t.Parallel()
	cat, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog: %v", err)
	}
	families := map[string]*Family{}
	for i := range cat.Families {
		families[cat.Families[i].Name] = &cat.Families[i]
	}
	for _, name := range []string{"ubuntu22.04-driverless", "ubuntu22.04-cuda12", "ubuntu24.04-driverless", "ubuntu24.04-cuda13.0", "mk8s-worker-node-v-1-32-ubuntu24.04"} {
		if families[name] == nil {
			t.Errorf("default catalog lacks family %s", name)
		}
	}
	if d := families["ubuntu22.04-driverless"].Deprecation; d == nil || !d.At.Equal(time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ubuntu22.04-driverless deprecation = %+v", d)
	}
}

func TestParseCatalogErrors(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		"families: [{name: '', versions: [{version: '1'}]}]",
		"families: [{name: a}]",
		"families: [{name: a, versions: [{version: '1'}, {version: '1'}]}]",
		"families: [{name: a, versions: [{version: '1'}]}, {name: a, versions: [{version: '1'}]}]",
		"families: [{name: a, cpu_architecture: riscv, versions: [{version: '1'}]}]",
		"families: [{name: a, colour: blue, versions: [{version: '1'}]}]", // unknown field
		"{",
	} {
		if _, err := ParseCatalog([]byte(doc)); err == nil {
			t.Errorf("ParseCatalog(%q) succeeded, want error", doc)
		}
	}
}

func TestParseCatalogJSONAndRegions(t *testing.T) {
	t.Parallel()
	cat, err := ParseCatalog([]byte(`{"families": [{"name": "a", "cpu_architecture": "arm64", "regions": ["eu-north1"],
		"versions": [{"version": "v1", "released_at": "2025-05-05T00:00:00Z", "deprecation": {"at": "2026-01-01T00:00:00Z", "message": "m"}}]}]}`))
	if err != nil {
		t.Fatalf("ParseCatalog(JSON): %v", err)
	}
	f := cat.Families[0]
	if arch, _ := parseArch(f.CPUArchitecture); arch != computepb.ImageSpec_ARM64 {
		t.Fatalf("arch = %v", arch)
	}
	if got := f.Versions[0].releasedAt(time.Time{}); !got.Equal(time.Date(2025, 5, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("released_at = %v", got)
	}
	if err := cat.CheckRegions(testRegions); err != nil {
		t.Fatalf("CheckRegions: %v", err)
	}
	if err := cat.CheckRegions([]Region{{Name: "us-central1", RoutingCode: "u00"}}); err == nil {
		t.Fatal("CheckRegions accepted an unknown region")
	}
}

func TestSeedIsIdempotentAndStable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cat, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	deps := SeedDeps{Images: f.images, Blobs: f.blobs, Clock: f.clk}
	for range 2 {
		if err := SeedPublicCatalog(t.Context(), deps, cat, testRegions); err != nil {
			t.Fatalf("SeedPublicCatalog: %v", err)
		}
	}
	versions := 0
	for _, fam := range cat.Families {
		versions += len(fam.Versions)
	}
	if got := len(f.images.order); got != versions*len(testRegions) {
		t.Fatalf("seeded %d images, want %d", got, versions*len(testRegions))
	}
	img, err := f.svc.GetLatestByFamily(t.Context(), "ubuntu24.04-driverless", "")
	if err != nil {
		t.Fatal(err)
	}
	if img.GetMetadata().GetName() != "ubuntu24.04-driverless-20251015" || img.GetStatus().GetMinDiskSizeBytes() != 20<<30 ||
		img.GetStatus().GetStorageSizeBytes() != syntheticPayloadSize ||
		!img.GetMetadata().GetCreatedAt().AsTime().Equal(time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("seeded image = %v", img)
	}
	blob := f.blobs.images[img.GetMetadata().GetId()]
	if blob.meta[MetaFormat] != "qcow2" || int64(len(blob.data)) != syntheticPayloadSize {
		t.Fatalf("seeded blob meta = %v (%d bytes)", blob.meta, len(blob.data))
	}
}
