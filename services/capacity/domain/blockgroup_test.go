package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/capacity/domain"
)

func TestPercentage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		usage, limit uint64
		want         string
	}{
		{0, 0, "0%"},
		{0, 16, "0%"},
		{3, 8, "37.5%"},
		{1, 2, "50%"},
		{1, 3, "33.33%"},
		{16, 16, "100%"},
		{5, 0, "100%"},
	}
	for _, tt := range tests {
		if got := domain.Percentage(tt.usage, tt.limit); got != tt.want {
			t.Errorf("percentage(%d, %d) = %q, want %q", tt.usage, tt.limit, got, tt.want)
		}
	}
}

func TestBlockGroupStatusOverTime(t *testing.T) {
	t.Parallel()
	type cont struct {
		start, end time.Time
		quantity   uint64
		state      capacitypb.CurrentContinuousInterval_State
	}
	const (
		scheduled = capacitypb.CurrentContinuousInterval_STATE_SCHEDULED
		active    = capacitypb.CurrentContinuousInterval_STATE_ACTIVE
		expired   = capacitypb.CurrentContinuousInterval_STATE_EXPIRED
	)
	tests := []struct {
		name       string
		key        string
		now        time.Time
		limit      uint64
		wantActive bool
		nextAt     time.Time // zero: no next change
		nextTo     uint64
		cont       cont
	}{
		{"a: before its first interval", "a", day(-40), 0, false, day(-30), 16, cont{day(-30), dayEnd(334), 16, scheduled}},
		{"a: first interval only", "a", t0, 16, true, day(14), 24, cont{day(-30), dayEnd(334), 16, active}},
		{"a: both intervals active", "a", day(14), 24, true, dayEnd(44), 16, cont{day(-30), dayEnd(334), 24, active}},
		{"a: last minute of the second interval", "a", dayEnd(44).Add(-time.Second), 24, true, dayEnd(44), 16, cont{day(-30), dayEnd(334), 24, active}},
		{"a: second interval ended (end is exclusive)", "a", dayEnd(44), 16, true, dayEnd(334), 0, cont{day(-30), dayEnd(334), 16, active}},
		{"a: all expired", "a", day(400), 0, false, time.Time{}, 0, cont{day(-30), dayEnd(334), 16, expired}},
		{"future: scheduled", "future", t0, 0, false, day(5), 10, cont{day(5), dayEnd(9), 10, scheduled}},
		{"future: active", "future", day(5), 10, true, dayEnd(9), 0, cont{day(5), dayEnd(9), 10, active}},
		{"gap: continuous run of two intervals", "gap", t0, 4, true, dayEnd(1), 0, cont{day(0), dayEnd(3), 4, active}},
		// During the one minute break the run stays continuous (ACTIVE); its
		// quantity is the current limit, 0.
		{"gap: one minute break between intervals", "gap", dayEnd(1), 0, false, day(2), 4, cont{day(0), dayEnd(3), 0, active}},
		{"gap: wide break to the next run", "gap", day(4), 0, false, day(5), 4, cont{day(5), dayEnd(6), 4, scheduled}},
		{"gap: all expired", "gap", day(7), 0, false, time.Time{}, 0, cont{day(5), dayEnd(6), 4, expired}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, tt.now)
			g := e.blockGroup(t, gid(t, tt.key))
			st := g.GetStatus()
			if st.GetCurrentLimit() != tt.limit {
				t.Errorf("current_limit = %d, want %d", st.GetCurrentLimit(), tt.limit)
			}
			wantState := capacitypb.CapacityBlockGroupStatus_STATE_INACTIVE
			if tt.wantActive {
				wantState = capacitypb.CapacityBlockGroupStatus_STATE_ACTIVE
			}
			if st.GetState() != wantState {
				t.Errorf("state = %v, want %v", st.GetState(), wantState)
			}
			if tt.nextAt.IsZero() {
				if st.GetNextChangeAt() != nil || st.GetNextChangeTo() != 0 {
					t.Errorf("next change = %v to %d, want none", st.GetNextChangeAt(), st.GetNextChangeTo())
				}
			} else {
				if got := st.GetNextChangeAt().AsTime(); !got.Equal(tt.nextAt) {
					t.Errorf("next_change_at = %s, want %s", got, tt.nextAt)
				}
				if st.GetNextChangeTo() != tt.nextTo {
					t.Errorf("next_change_to = %d, want %d", st.GetNextChangeTo(), tt.nextTo)
				}
			}
			c := st.GetCurrentContinuousInterval()
			if c == nil {
				t.Fatal("current_continuous_interval = nil")
			}
			if !c.GetStartTime().AsTime().Equal(tt.cont.start) || !c.GetEndTime().AsTime().Equal(tt.cont.end) {
				t.Errorf("continuous interval = [%s, %s], want [%s, %s]", c.GetStartTime().AsTime(), c.GetEndTime().AsTime(), tt.cont.start, tt.cont.end)
			}
			if c.GetQuantity() != tt.cont.quantity || c.GetState() != tt.cont.state {
				t.Errorf("continuous interval quantity, state = %d, %v; want %d, %v", c.GetQuantity(), c.GetState(), tt.cont.quantity, tt.cont.state)
			}
		})
	}
}

func TestBlockGroupShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	id := gid(t, "a")
	g := e.blockGroup(t, id)
	md := g.GetMetadata()
	if md.GetId() != id || md.GetParentId() != tenantID || md.GetResourceVersion() != 1 {
		t.Errorf("metadata id, parent, version = %s, %s, %d; want %s, %s, 1", md.GetId(), md.GetParentId(), md.GetResourceVersion(), id, tenantID)
	}
	if !md.GetCreatedAt().AsTime().Equal(day(-30)) {
		t.Errorf("created_at = %s, want %s", md.GetCreatedAt().AsTime(), day(-30))
	}
	st := g.GetStatus()
	if st.GetRegion() != "eu-north1" || st.GetService() != "compute" {
		t.Errorf("region, service = %q, %q; want eu-north1, compute", st.GetRegion(), st.GetService())
	}
	aff := st.GetResourceAffinity().GetComputeV1()
	if aff.GetPlatform() != "gpu-h100-sxm" || aff.GetFabric() != "fabric-2" {
		t.Errorf("affinity platform, fabric = %q, %q; want gpu-h100-sxm, fabric-2", aff.GetPlatform(), aff.GetFabric())
	}
	if st.GetUsage() != 0 || st.GetUsagePercentage() != "0%" || st.GetUsageState() != capacitypb.CapacityBlockGroupStatus_USAGE_STATE_NOT_USED {
		t.Errorf("usage, percentage, state = %d, %q, %v; want 0, 0%%, NOT_USED", st.GetUsage(), st.GetUsagePercentage(), st.GetUsageState())
	}
	if _, err := e.svc.GetBlockGroup(t.Context(), "capacityblockgroup-e00nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetBlockGroup(unknown) error = %v, want not_found", err)
	}
}

func TestIntervals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		now  time.Time
		want []capacitypb.CapacityIntervalStatus_State
	}{
		{"only the first has started", t0, []capacitypb.CapacityIntervalStatus_State{
			capacitypb.CapacityIntervalStatus_STATE_ACTIVE, capacitypb.CapacityIntervalStatus_STATE_SCHEDULED,
		}},
		{"both active", day(20), []capacitypb.CapacityIntervalStatus_State{
			capacitypb.CapacityIntervalStatus_STATE_ACTIVE, capacitypb.CapacityIntervalStatus_STATE_ACTIVE,
		}},
		{"the second expired", day(100), []capacitypb.CapacityIntervalStatus_State{
			capacitypb.CapacityIntervalStatus_STATE_ACTIVE, capacitypb.CapacityIntervalStatus_STATE_EXPIRED,
		}},
		{"both expired", day(400), []capacitypb.CapacityIntervalStatus_State{
			capacitypb.CapacityIntervalStatus_STATE_EXPIRED, capacitypb.CapacityIntervalStatus_STATE_EXPIRED,
		}},
		{"none started", day(-40), []capacitypb.CapacityIntervalStatus_State{
			capacitypb.CapacityIntervalStatus_STATE_SCHEDULED, capacitypb.CapacityIntervalStatus_STATE_SCHEDULED,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, tt.now)
			groupID := gid(t, "a")
			page, err := e.svc.ListIntervals(t.Context(), groupID, 0, "")
			if err != nil {
				t.Fatalf("ListIntervals() error: %v", err)
			}
			if len(page.Items) != 2 || page.NextPageToken != "" {
				t.Fatalf("ListIntervals() = %d items, token %q; want 2, none", len(page.Items), page.NextPageToken)
			}
			for i, iv := range page.Items {
				if iv.GetStatus().GetState() != tt.want[i] {
					t.Errorf("interval %d state = %v, want %v", i, iv.GetStatus().GetState(), tt.want[i])
				}
				got, err := e.svc.GetInterval(t.Context(), iv.GetMetadata().GetId())
				if err != nil {
					t.Fatalf("GetInterval(%s) error: %v", iv.GetMetadata().GetId(), err)
				}
				if diff := cmp.Diff(iv, got, protocmp.Transform()); diff != "" {
					t.Errorf("GetInterval differs from ListIntervals (-list +get):\n%s", diff)
				}
			}
		})
	}
}

func TestIntervalShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	page, err := e.svc.ListIntervals(t.Context(), gid(t, "a"), 0, "")
	if err != nil {
		t.Fatalf("ListIntervals() error: %v", err)
	}
	first, second := page.Items[0], page.Items[1]
	if first.GetMetadata().GetParentId() != gid(t, "a") {
		t.Errorf("parent_id = %s, want the block group", first.GetMetadata().GetParentId())
	}
	st := first.GetStatus()
	if st.GetContainerId() != tenantID || st.GetRegion() != "eu-north1" || st.GetService() != "compute" || st.GetQuantity() != 16 {
		t.Errorf("status container, region, service, quantity = %q, %q, %q, %d; want %s, eu-north1, compute, 16",
			st.GetContainerId(), st.GetRegion(), st.GetService(), st.GetQuantity(), tenantID)
	}
	if !st.GetStartTime().AsTime().Equal(day(-30)) || !st.GetEndTime().AsTime().Equal(dayEnd(334)) {
		t.Errorf("first interval = [%s, %s], want [%s, %s]", st.GetStartTime().AsTime(), st.GetEndTime().AsTime(), day(-30), dayEnd(334))
	}
	if second.GetStatus().GetQuantity() != 8 || !second.GetStatus().GetStartTime().AsTime().Equal(day(14)) {
		t.Errorf("second interval quantity, start = %d, %s; want 8, %s", second.GetStatus().GetQuantity(), second.GetStatus().GetStartTime().AsTime(), day(14))
	}
	if _, err := e.svc.GetInterval(t.Context(), "capacityinterval-e00nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetInterval(unknown) error = %v, want not_found", err)
	}
	if _, err := e.svc.ListIntervals(t.Context(), "capacityblockgroup-e00nope", 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("ListIntervals(unknown group) error = %v, want not_found", err)
	}
}

func TestGetBlockGroupByAffinity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		aff      domain.Affinity
		wantID   string // key in the default tenant
		wantKind errs.Kind
	}{
		{"unique platform", domain.Affinity{TenantID: tenantID, Region: "eu-north1", Platform: "gpu-h200-sxm"}, "c", 0},
		{"fabric disambiguates", domain.Affinity{TenantID: tenantID, Region: "eu-north1", Platform: "gpu-h100-sxm", Fabric: "fabric-3"}, "b", 0},
		{"fabric matches the only group", domain.Affinity{TenantID: tenantID, Region: "eu-north1", Platform: "gpu-h200-sxm", Fabric: "fabric-7"}, "c", 0},
		{"two fabrics, none given", domain.Affinity{TenantID: tenantID, Region: "eu-north1", Platform: "gpu-h100-sxm"}, "", errs.KindBadRequest},
		{"unknown platform", domain.Affinity{TenantID: tenantID, Region: "eu-north1", Platform: "gpu-nope"}, "", errs.KindNotFound},
		{"wrong fabric", domain.Affinity{TenantID: tenantID, Region: "eu-north1", Platform: "gpu-h200-sxm", Fabric: "fabric-1"}, "", errs.KindNotFound},
		{"wrong region", domain.Affinity{TenantID: tenantID, Region: "us-central1", Platform: "gpu-h200-sxm"}, "", errs.KindNotFound},
		{"platform required", domain.Affinity{TenantID: tenantID, Region: "eu-north1"}, "", errs.KindBadRequest},
		{"tenant must be a tenant ID", domain.Affinity{TenantID: projectID, Region: "eu-north1", Platform: "gpu-h200-sxm"}, "", errs.KindBadRequest},
		{"unknown tenant", domain.Affinity{TenantID: "tenant-e00unknown", Region: "eu-north1", Platform: "gpu-h200-sxm"}, "", errs.KindNotFound},
		{"other tenant has its own groups", domain.Affinity{TenantID: otherTenantID, Region: "eu-north1", Platform: "gpu-h100-sxm"}, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			g, err := e.svc.GetBlockGroupByAffinity(t.Context(), tt.aff)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("GetBlockGroupByAffinity(%+v) kind = %v, want %v (err: %v)", tt.aff, got, tt.wantKind, err)
			}
			if err == nil && tt.wantID != "" && g.GetMetadata().GetId() != gid(t, tt.wantID) {
				t.Errorf("GetBlockGroupByAffinity(%+v) = %s, want %s", tt.aff, g.GetMetadata().GetId(), gid(t, tt.wantID))
			}
			if err == nil && tt.aff.TenantID == otherTenantID && g.GetMetadata().GetParentId() != otherTenantID {
				t.Errorf("parent = %s, want the other tenant", g.GetMetadata().GetParentId())
			}
		})
	}
}

func TestAffinityAmbiguityNamesTheFabrics(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	_, err := e.svc.GetBlockGroupByAffinity(t.Context(), domain.Affinity{TenantID: tenantID, Region: "eu-north1", Platform: "gpu-h100-sxm"})
	ee, ok := errors.AsType[*errs.Error](err)
	if !ok || len(ee.Violations) == 0 || ee.Violations[0].Field != "resource_affinity.compute_v1.fabric" {
		t.Fatalf("error = %v, want a violation on resource_affinity.compute_v1.fabric", err)
	}
}

func TestListBlockGroupsOfATenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	page, err := e.svc.ListBlockGroups(t.Context(), tenantID, 0, "")
	if err != nil {
		t.Fatalf("ListBlockGroups() error: %v", err)
	}
	var got []string
	for _, g := range page.Items {
		got = append(got, g.GetMetadata().GetId())
	}
	want := []string{gid(t, "a"), gid(t, "b"), gid(t, "c"), gid(t, "future"), gid(t, "gap")}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListBlockGroups() IDs (-want +got):\n%s", diff)
	}
	other, err := e.svc.ListBlockGroups(t.Context(), otherTenantID, 0, "")
	if err != nil || len(other.Items) != 1 || other.Items[0].GetMetadata().GetId() != gidIn(t, otherTenantID, "foreign") {
		t.Errorf("ListBlockGroups(other tenant) = %v, %v; want only its foreign group", other.Items, err)
	}
}
