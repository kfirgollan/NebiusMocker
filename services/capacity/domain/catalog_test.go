package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/services/capacity/domain"
)

func TestDefaultCatalog(t *testing.T) {
	t.Parallel()
	cat, err := domain.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error: %v", err)
	}
	if len(cat.BlockGroups) == 0 || len(cat.Presets) == 0 {
		t.Fatalf("DefaultCatalog() = %d block groups, %d presets; want both non-empty", len(cat.BlockGroups), len(cat.Presets))
	}
	routing := map[string]string{}
	for _, g := range cat.BlockGroups {
		routing[g.Region] = routingCode
	}
	inv, err := cat.Build(t0, tenantID, routing)
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	groups := inv.InTenant(tenantID)
	if len(groups) != len(cat.BlockGroups) {
		t.Fatalf("InTenant(default) = %d groups, want %d", len(groups), len(cat.BlockGroups))
	}
	for _, g := range groups {
		if len(g.Intervals) == 0 {
			t.Errorf("group %s has no intervals", g.ID)
		}
	}
}

// groupYAML returns a one-group catalog; fields override the valid ones.
func groupYAML(key, region, platform, fabric, intervals string) string {
	return "block_groups:\n  - key: " + key + "\n    region: " + region + "\n    platform: " + platform +
		"\n    fabric: " + fabric + "\n    intervals: " + intervals + "\n"
}

const goodInterval = "[{quantity: 1, start_day: 0, end_day: 1}]"

func TestParseCatalogValid(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, yaml string }{
		{"days", groupYAML("k", "r", "p", "f", goodInterval)},
		{"timestamps", groupYAML("k", "r", "p", "f", `[{quantity: 1, start: "2026-01-01T00:00:00Z", end: "2026-02-01T00:00:00Z"}]`)},
		{"same key in two tenants", groupYAML("k", "r", "p", "f", goodInterval) +
			"  - {key: k, tenant: tenant-x, region: r, platform: p, fabric: f, intervals: " + goodInterval + "}\n"},
		{"single day", groupYAML("k", "r", "p", "f", "[{quantity: 1, start_day: 3, end_day: 3}]")},
		{"empty catalog", "block_groups: []\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := domain.ParseCatalog([]byte(tt.yaml)); err != nil {
				t.Fatalf("ParseCatalog() error: %v", err)
			}
		})
	}
}

func TestParseCatalogErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"missing key", groupYAML(`""`, "r", "p", "f", goodInterval), "key is required"},
		{"missing region", groupYAML("k", `""`, "p", "f", goodInterval), "region, platform and fabric are required"},
		{"missing platform", groupYAML("k", "r", `""`, "f", goodInterval), "region, platform and fabric are required"},
		{"missing fabric", groupYAML("k", "r", "p", `""`, goodInterval), "region, platform and fabric are required"},
		{"duplicate key", groupYAML("k", "r", "p", "f", goodInterval) +
			"  - {key: k, region: r, platform: p, fabric: f, intervals: " + goodInterval + "}\n", "duplicate key"},
		{"no intervals", groupYAML("k", "r", "p", "f", "[]"), "at least one interval is required"},
		{
			"both day and stamp forms", groupYAML("k", "r", "p", "f",
				`[{quantity: 1, start_day: 0, end_day: 1, start: "2026-01-01T00:00:00Z", end: "2026-01-02T00:00:00Z"}]`),
			"either start_day/end_day or start/end",
		},
		{"only start_day", groupYAML("k", "r", "p", "f", "[{quantity: 1, start_day: 0}]"), "start_day and end_day are both required"},
		{"end_day before start_day", groupYAML("k", "r", "p", "f", "[{quantity: 1, start_day: 5, end_day: 4}]"), "end_day is before start_day"},
		{"end before start", groupYAML("k", "r", "p", "f",
			`[{quantity: 1, start: "2026-02-01T00:00:00Z", end: "2026-01-01T00:00:00Z"}]`), "end must be after start"},
		{"end equals start", groupYAML("k", "r", "p", "f",
			`[{quantity: 1, start: "2026-01-01T00:00:00Z", end: "2026-01-01T00:00:00Z"}]`), "end must be after start"},
		{"bad start timestamp", groupYAML("k", "r", "p", "f", `[{quantity: 1, start: yesterday, end: "2026-01-01T00:00:00Z"}]`), "start:"},
		{"bad end timestamp", groupYAML("k", "r", "p", "f", `[{quantity: 1, start: "2026-01-01T00:00:00Z", end: tomorrow}]`), "end:"},
		{"no times at all", groupYAML("k", "r", "p", "f", "[{quantity: 1}]"), "is required"},
		{"unknown field", "block_groups:\n  - {key: k, bogus: 1}\n", "decode capacity catalog"},
		{"bad preset", "presets:\n  - platform: p\n    presets:\n      - {name: x, vcpu_count: 1, memory_gibibytes: 1, gpu_count: 0}\n", "needs a name"},
		{"preset without name", "presets:\n  - platform: p\n    presets:\n      - {vcpu_count: 1, memory_gibibytes: 1, gpu_count: 1}\n", "needs a name"},
		{"preset without platform", "presets:\n  - presets: []\n", "platform is required"},
		{"duplicate platform", "presets:\n  - {platform: p}\n  - {platform: p}\n", "duplicate platform"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := domain.ParseCatalog([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseCatalog() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseCatalogReportsEveryProblem(t *testing.T) {
	t.Parallel()
	_, err := domain.ParseCatalog([]byte("block_groups:\n  - {key: a}\n  - {key: b}\n"))
	if err == nil {
		t.Fatal("ParseCatalog() error = nil, want one error per bad group")
	}
	for _, want := range []string{"block_groups[0]", "block_groups[1]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func buildInventory(t *testing.T, now time.Time, defaultTenant string, routing map[string]string) *domain.Inventory {
	t.Helper()
	cat, err := domain.ParseCatalog([]byte(testCatalogYAML))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	inv, err := cat.Build(now, defaultTenant, routing)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return inv
}

func TestBuildIDsAreDeterministic(t *testing.T) {
	t.Parallel()
	routing := map[string]string{"eu-north1": routingCode}
	first := buildInventory(t, t0, tenantID, routing)
	second := buildInventory(t, t0.Add(48*time.Hour), tenantID, routing)
	a, b := first.InTenant(tenantID), second.InTenant(tenantID)
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("InTenant() = %d and %d groups, want equal and non-empty", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Errorf("group %d ID = %s then %s, want the same", i, a[i].ID, b[i].ID)
		}
		for j := range a[i].Intervals {
			if a[i].Intervals[j].ID != b[i].Intervals[j].ID {
				t.Errorf("group %d interval %d ID changed between builds", i, j)
			}
		}
	}
	if want := gid(t, "a"); a[0].ID != want {
		t.Errorf("group a ID = %s, want %s", a[0].ID, want)
	}
	if !strings.HasPrefix(a[0].ID, "capacityblockgroup-"+routingCode) {
		t.Errorf("group ID %q lacks the capacityblockgroup-%s prefix", a[0].ID, routingCode)
	}
	if !strings.HasPrefix(a[0].Intervals[0].ID, "capacityinterval-"+routingCode) {
		t.Errorf("interval ID %q lacks the capacityinterval-%s prefix", a[0].Intervals[0].ID, routingCode)
	}
}

func TestBuildTenantsAndRegions(t *testing.T) {
	t.Parallel()
	inv := buildInventory(t, t0, tenantID, map[string]string{"eu-north1": routingCode})
	if got := len(inv.InTenant(tenantID)); got != 5 {
		t.Errorf("InTenant(default) = %d groups, want 5 (a, b, c, future, gap)", got)
	}
	if got := len(inv.InTenant(otherTenantID)); got != 1 {
		t.Errorf("InTenant(other) = %d groups, want 1", got)
	}
	for _, g := range inv.InTenant(tenantID) {
		if g.Region != "eu-north1" {
			t.Errorf("group %s region = %q: a group of a region without routing was kept", g.ID, g.Region)
		}
	}
	if _, ok := inv.Group(gidIn(t, tenantID, "elsewhere")); ok {
		t.Error("group of a region not in routing was built, want it skipped")
	}
	if g, ok := inv.Group(gidIn(t, otherTenantID, "foreign")); !ok || g.TenantID != otherTenantID {
		t.Errorf("Group(foreign) = %v, %v; want a group of %s", g, ok, otherTenantID)
	}

	// The default tenant is only a default: another default moves the groups.
	moved := buildInventory(t, t0, "tenant-e00moved", map[string]string{"eu-north1": routingCode})
	if got := len(moved.InTenant("tenant-e00moved")); got != 5 {
		t.Errorf("InTenant(moved default) = %d groups, want 5", got)
	}
	if got := len(moved.InTenant(tenantID)); got != 0 {
		t.Errorf("InTenant(original default) = %d groups, want 0", got)
	}

	if got := len(buildInventory(t, t0, tenantID, nil).InTenant(tenantID)); got != 0 {
		t.Errorf("Build without routing built %d groups, want 0", got)
	}
}

func TestBuildIntervalTimes(t *testing.T) {
	t.Parallel()
	// Mid-day: relative days still count from 00:00 UTC of that day.
	inv := buildInventory(t, t0.Add(10*time.Hour+30*time.Minute), tenantID, map[string]string{"eu-north1": routingCode})
	g, ok := inv.Group(gid(t, "a"))
	if !ok {
		t.Fatal("group a not built")
	}
	want := []struct{ start, end time.Time }{
		{day(-30), dayEnd(334)},
		{day(14), dayEnd(44)},
	}
	if len(g.Intervals) != len(want) {
		t.Fatalf("group a has %d intervals, want %d", len(g.Intervals), len(want))
	}
	for i, w := range want {
		iv := g.Intervals[i]
		if !iv.Start.Equal(w.start) || !iv.End.Equal(w.end) {
			t.Errorf("interval %d = [%s, %s), want [%s, %s)", i, iv.Start, iv.End, w.start, w.end)
		}
		if iv.BlockGroupID != g.ID {
			t.Errorf("interval %d block group = %s, want %s", i, iv.BlockGroupID, g.ID)
		}
		if got, ok := inv.Interval(iv.ID); !ok || got.ID != iv.ID {
			t.Errorf("Interval(%s) = %v, %v; want it found", iv.ID, got, ok)
		}
	}
	if !g.CreatedAt.Equal(day(-30)) {
		t.Errorf("group a CreatedAt = %s, want the earliest interval start %s", g.CreatedAt, day(-30))
	}
	if _, ok := inv.Interval("capacityinterval-nope"); ok {
		t.Error("Interval(unknown) found something")
	}
}

func TestBuildTimestampIntervalsAreSortedByStart(t *testing.T) {
	t.Parallel()
	cat, err := domain.ParseCatalog([]byte(groupYAML("k", "r", "p", "f",
		`[{quantity: 2, start: "2026-03-01T00:00:00+01:00", end: "2026-04-01T00:00:00Z"}, {quantity: 1, start: "2026-01-01T00:00:00Z", end: "2026-02-01T00:00:00Z"}]`)))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	inv, err := cat.Build(t0, tenantID, map[string]string{"r": routingCode})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	g := inv.InTenant(tenantID)[0]
	if g.Intervals[0].Quantity != 1 || g.Intervals[1].Quantity != 2 {
		t.Fatalf("interval quantities = %d, %d; want 1, 2 (sorted by start)", g.Intervals[0].Quantity, g.Intervals[1].Quantity)
	}
	if want := time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC); !g.Intervals[1].Start.Equal(want) || g.Intervals[1].Start.Location() != time.UTC {
		t.Errorf("offset timestamp start = %s, want %s in UTC", g.Intervals[1].Start, want)
	}
}
