package quota

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestComputeInstanceGPU(t *testing.T) {
	t.Parallel()
	for platform, want := range map[string]string{
		"gpu-h200-sxm": "compute.instance.gpu.h200",
		"gpu-b200":     "compute.instance.gpu.b200",
		"cpu-d3":       "",
		"gpu-":         "",
		"":             "",
	} {
		if got := ComputeInstanceGPU(platform); got != want {
			t.Errorf("ComputeInstanceGPU(%q) = %q, want %q", platform, got, want)
		}
	}
}

func TestSizeQuotaNames(t *testing.T) {
	t.Parallel()
	if got, want := ComputeDiskSize("NETWORK_SSD_IO_M3"), "compute.disk.size.network-ssd-io-m3"; got != want {
		t.Errorf("ComputeDiskSize = %q, want %q", got, want)
	}
	if got, want := ComputeFilesystemSize("NETWORK_HDD"), "compute.filesystem.size.network-hdd"; got != want {
		t.Errorf("ComputeFilesystemSize = %q, want %q", got, want)
	}
}

func TestDefaultCatalogOverrides(t *testing.T) {
	t.Parallel()
	c, err := DefaultCatalog(map[string]uint64{ComputeInstanceCount: 7, "compute.instance.gpu.zz9": 3})
	if err != nil {
		t.Fatalf("DefaultCatalog: %v", err)
	}
	if e, ok := c.Lookup(ComputeInstanceCount); !ok || e.Default != 7 {
		t.Errorf("override = %+v, %v; want default 7", e, ok)
	}
	if e, ok := c.Lookup("compute.instance.gpu.zz9"); !ok || e.Default != 3 {
		t.Errorf("synthesized override = %+v, %v; want default 3", e, ok)
	}
	if e, _ := c.Lookup(ComputeDiskCount); e.Default != defaultCount {
		t.Errorf("untouched default = %d, want %d", e.Default, defaultCount)
	}

	_, err = DefaultCatalog(map[string]uint64{"no.such.quota": 1})
	uq, ok := errors.AsType[*UnknownQuotaError](err)
	if !ok || uq.Name != "no.such.quota" {
		t.Fatalf("DefaultCatalog unknown = %v, want *UnknownQuotaError for no.such.quota", err)
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	c, err := DefaultCatalog(nil)
	if err != nil {
		t.Fatalf("DefaultCatalog: %v", err)
	}
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"compute.instance.gpu.h200", true},
		{"compute.instance.gpu.mi300", true}, // unlisted family is synthesized
		{"compute.instance.gpu.", false},
		{"compute.instance.gpu.a.b", false},
		{"nope", false},
	} {
		e, ok := c.Lookup(tc.name)
		if ok != tc.ok {
			t.Errorf("Lookup(%q) ok = %v, want %v", tc.name, ok, tc.ok)
		}
		if ok && (e.Default != defaultGPU || e.Service != "compute" || e.ServiceDescription == "") {
			t.Errorf("Lookup(%q) = %+v, want a GPU entry with the GPU default", tc.name, e)
		}
	}
	for _, e := range c.Entries() {
		if e.Name == "compute.instance.gpu.mi300" {
			t.Error("synthesized family appears in Entries")
		}
	}
}

func TestEntries(t *testing.T) {
	t.Parallel()
	c, err := DefaultCatalog(nil)
	if err != nil {
		t.Fatalf("DefaultCatalog: %v", err)
	}
	es := c.Entries()
	if len(es) == 0 {
		t.Fatal("no entries")
	}
	if !slices.IsSortedFunc(es, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) }) {
		t.Error("entries are not sorted by name")
	}
	for _, e := range es {
		if e.Name == "" || e.Service == "" || e.ServiceDescription == "" || e.Unit == "" || e.Description == "" {
			t.Errorf("incomplete entry %+v", e)
		}
		if e.Unit != UnitCount && e.Unit != UnitByte {
			t.Errorf("%s: unit %q", e.Name, e.Unit)
		}
	}
}
