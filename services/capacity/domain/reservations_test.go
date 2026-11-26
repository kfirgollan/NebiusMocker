package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// h100 is a reservation request for the h100 platform in the default
// project; fields are filled by the callers.
func h100(consumer string, qty uint64, ids ...string) peer.CapacityRequest {
	return peer.CapacityRequest{
		ConsumerID: consumer, ProjectID: projectID, Region: "eu-north1", Platform: "gpu-h100-sxm",
		Quantity: qty, ReservationIDs: ids,
	}
}

func TestReserveWithinTheGroupLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0) // group a: 16 GPUs
	a := gid(t, "a")
	got, err := e.svc.Reserve(t.Context(), h100("c1", 10, a))
	if err != nil || got != a {
		t.Fatalf("Reserve(10) = %q, %v; want %s", got, err, a)
	}
	got, err = e.svc.Reserve(t.Context(), h100("c2", 6, a))
	if err != nil || got != a {
		t.Fatalf("Reserve(6) = %q, %v; want %s (exactly fills the group)", got, err, a)
	}
	_, err = e.svc.Reserve(t.Context(), h100("c3", 1, a))
	wantKind(t, "Reserve(1) in a full group", err, errs.KindQuotaExceeded)
	if err := e.svc.Release(t.Context(), "c1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := e.svc.Reserve(t.Context(), h100("c3", 10, a)); err != nil {
		t.Fatalf("Reserve(10) after a release: %v", err)
	}
}

func TestReserveFollowsTheGroupLimitOverTime(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	a := gid(t, "a")
	_, err := e.svc.Reserve(t.Context(), h100("c1", 20, a))
	wantKind(t, "Reserve(20) before the second interval", err, errs.KindQuotaExceeded)
	e.clk.Advance(14 * time.Hour * 24)
	if _, err := e.svc.Reserve(t.Context(), h100("c1", 20, a)); err != nil {
		t.Fatalf("Reserve(20) while the limit is 24: %v", err)
	}
}

func TestReserveOutsideAnyInterval(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	req := h100("c1", 1, gid(t, "future"))
	req.Platform, req.Fabric = "gpu-b200-sxm", "fabric-9"
	_, err := e.svc.Reserve(t.Context(), req)
	wantKind(t, "Reserve in a group that is not active yet", err, errs.KindQuotaExceeded)
}

func TestReserveHonorsTheAllowance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		allowance *uint64
		qty       uint64
		wantKind  errs.Kind
	}{
		{"within the limit", u64(4), 4, 0},
		{"over the limit", u64(4), 5, errs.KindQuotaExceeded},
		{"zero limit", u64(0), 1, errs.KindQuotaExceeded},
		{"limit above the group size, within the group", u64(1000), 16, 0},
		{"limit above the group size, over the group", u64(1000), 17, errs.KindQuotaExceeded},
		{"no limit", nil, 16, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			a := gid(t, "a")
			e.newAllowance(t, projectID, a, tt.allowance)
			_, err := e.svc.Reserve(t.Context(), h100("c1", tt.qty, a))
			wantKind(t, "Reserve", err, tt.wantKind)
		})
	}
}

func TestReserveAllowanceCountsTheProjectsOtherConsumers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	a := gid(t, "a")
	e.newAllowance(t, projectID, a, u64(6))
	if _, err := e.svc.Reserve(t.Context(), h100("c1", 4, a)); err != nil {
		t.Fatalf("Reserve(4): %v", err)
	}
	_, err := e.svc.Reserve(t.Context(), h100("c2", 3, a))
	wantKind(t, "Reserve(3) with 4 of 6 used by the project", err, errs.KindQuotaExceeded)
	if _, err := e.svc.Reserve(t.Context(), h100("c2", 2, a)); err != nil {
		t.Fatalf("Reserve(2): %v", err)
	}

	// Another project without an allowance may use the rest of the group...
	other := h100("c3", 10, a)
	other.ProjectID = otherProjectID
	if _, err := e.svc.Reserve(t.Context(), other); err != nil {
		t.Fatalf("Reserve(10) for the other project: %v", err)
	}
	// ...but not more than the group's current limit (16) in total.
	other.ConsumerID, other.Quantity = "c4", 1
	_, err = e.svc.Reserve(t.Context(), other)
	wantKind(t, "Reserve(1) in a full group", err, errs.KindQuotaExceeded)
}

func TestReserveAgainReplacesTheReservation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	a := gid(t, "a")
	e.newAllowance(t, projectID, a, u64(8))
	if _, err := e.svc.Reserve(t.Context(), h100("c1", 8, a)); err != nil {
		t.Fatalf("Reserve(8): %v", err)
	}
	// The consumer's own reservation does not count against itself, neither
	// for the group nor for the allowance.
	if _, err := e.svc.Reserve(t.Context(), h100("c1", 8, a)); err != nil {
		t.Fatalf("Reserve(8) again: %v", err)
	}
	if _, err := e.svc.Reserve(t.Context(), h100("c1", 2, a)); err != nil {
		t.Fatalf("Reserve(2) again: %v", err)
	}
	if got := e.blockGroup(t, a).GetStatus().GetUsage(); got != 2 {
		t.Errorf("usage = %d, want 2 (replaced, not added)", got)
	}
	ids, err := e.svc.ListBlockGroupResources(t.Context(), a)
	if err != nil || len(ids) != 1 || ids[0] != "c1" {
		t.Errorf("ListBlockGroupResources() = %v, %v; want [c1]", ids, err)
	}
	_, err = e.svc.Reserve(t.Context(), h100("c1", 9, a))
	wantKind(t, "Reserve(9) over the allowance", err, errs.KindQuotaExceeded)
	if got := e.blockGroup(t, a).GetStatus().GetUsage(); got != 2 {
		t.Errorf("usage after a failed re-reserve = %d, want the old 2", got)
	}
}

func TestReserveWithoutNamedGroupsTriesEveryMatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0) // h100 groups: a (16, fabric-2), b (8, fabric-3)
	got, err := e.svc.Reserve(t.Context(), h100("c1", 16))
	if err != nil || got != gid(t, "a") {
		t.Fatalf("Reserve(16) = %q, %v; want group a", got, err)
	}
	got, err = e.svc.Reserve(t.Context(), h100("c2", 8))
	if err != nil || got != gid(t, "b") {
		t.Fatalf("Reserve(8) = %q, %v; want group b (a is full)", got, err)
	}
	_, err = e.svc.Reserve(t.Context(), h100("c3", 1))
	wantKind(t, "Reserve(1) when every match is full", err, errs.KindQuotaExceeded)

	req := h100("c4", 1)
	req.Fabric = "fabric-9"
	_, err = e.svc.Reserve(t.Context(), req)
	wantKind(t, "Reserve on a fabric without a group", err, errs.KindQuotaExceeded)

	// The other tenant's h100 group is never drawn from.
	if err := e.svc.Release(t.Context(), "c1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, id := range []string{"c2", "c3"} {
		if err := e.svc.Release(t.Context(), id); err != nil {
			t.Fatalf("Release(%s): %v", id, err)
		}
	}
	foreign := h100("c5", 4)
	foreign.ProjectID = foreignProject
	got, err = e.svc.Reserve(t.Context(), foreign)
	if err != nil || got != gidIn(t, otherTenantID, "foreign") {
		t.Errorf("Reserve for a project of the other tenant = %q, %v; want its own group", got, err)
	}
}

func TestReserveNamedGroups(t *testing.T) {
	t.Parallel()
	a, b, c := gid(t, "a"), gid(t, "b"), gid(t, "c")
	tests := []struct {
		name      string
		ids       []string
		qty       uint64
		wantID    string
		wantKind  errs.Kind
		wantField string
	}{
		{"first group fits", []string{a, b}, 4, a, 0, ""},
		{"too big for every named group", []string{a, b}, 17, "", errs.KindQuotaExceeded, ""},
		{"only the second fits", []string{b, a}, 8, b, 0, ""},
		{"unknown ID", []string{"capacityblockgroup-e00nope"}, 1, "", errs.KindNotFound, ""},
		{"group of another tenant", []string{gidIn(t, otherTenantID, "foreign")}, 1, "", errs.KindNotFound, ""},
		{"platform mismatch", []string{c}, 1, "", errs.KindBadRequest, "reservation_policy.reservation_ids[0]"},
		{"mismatch is reported by position", []string{a, c}, 1, "", errs.KindBadRequest, "reservation_policy.reservation_ids[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			got, err := e.svc.Reserve(t.Context(), h100("c1", tt.qty, tt.ids...))
			wantKind(t, "Reserve", err, tt.wantKind)
			if err == nil && got != tt.wantID {
				t.Errorf("Reserve() = %s, want %s", got, tt.wantID)
			}
			if tt.wantField != "" {
				ee, ok := errors.AsType[*errs.Error](err)
				if !ok || len(ee.Violations) != 1 || ee.Violations[0].Field != tt.wantField {
					t.Errorf("violations = %v, want one on %s", ee, tt.wantField)
				}
			}
			if err != nil && len(e.usage.items) != 0 {
				t.Errorf("a failed Reserve stored %d reservations", len(e.usage.items))
			}
		})
	}
}

func TestReserveNamedFabricMismatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	req := h100("c1", 1, gid(t, "a"))
	req.Fabric = "fabric-3"
	_, err := e.svc.Reserve(t.Context(), req)
	wantKind(t, "Reserve naming group a for fabric-3", err, errs.KindBadRequest)
}

func TestReserveInvalidRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*peer.CapacityRequest)
		want errs.Kind
	}{
		{"no consumer", func(r *peer.CapacityRequest) { r.ConsumerID = "" }, errs.KindBadRequest},
		{"zero quantity", func(r *peer.CapacityRequest) { r.Quantity = 0 }, errs.KindBadRequest},
		{"project is not a project ID", func(r *peer.CapacityRequest) { r.ProjectID = tenantID }, errs.KindBadRequest},
		{"unknown project", func(r *peer.CapacityRequest) { r.ProjectID = "project-e00unknown" }, errs.KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			req := h100("c1", 1)
			tt.edit(&req)
			_, err := e.svc.Reserve(t.Context(), req)
			wantKind(t, "Reserve", err, tt.want)
		})
	}
}

func TestReleaseUnknownConsumerIsNotAnError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	if err := e.svc.Release(t.Context(), "never-reserved"); err != nil {
		t.Fatalf("Release(unknown) error: %v", err)
	}
}

func TestUsageShowsInGroupAndResources(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	a := gid(t, "a")
	for _, r := range []peer.CapacityRequest{h100("c1", 4, a), h100("c2", 2, a)} {
		if _, err := e.svc.Reserve(t.Context(), r); err != nil {
			t.Fatalf("Reserve(%s): %v", r.ConsumerID, err)
		}
	}
	st := e.blockGroup(t, a).GetStatus()
	if st.GetUsage() != 6 || st.GetUsagePercentage() != "37.5%" || st.GetUsageState() != capacitypb.CapacityBlockGroupStatus_USAGE_STATE_USED {
		t.Errorf("usage, percentage, state = %d, %q, %v; want 6, 37.5%%, USED", st.GetUsage(), st.GetUsagePercentage(), st.GetUsageState())
	}
	ids, err := e.svc.ListBlockGroupResources(t.Context(), a)
	if err != nil {
		t.Fatalf("ListBlockGroupResources: %v", err)
	}
	if diff := cmp.Diff([]string{"c1", "c2"}, ids); diff != "" {
		t.Errorf("ListBlockGroupResources() (-want +got):\n%s", diff)
	}
	if other, err := e.svc.ListBlockGroupResources(t.Context(), gid(t, "b")); err != nil || len(other) != 0 {
		t.Errorf("ListBlockGroupResources(b) = %v, %v; want none", other, err)
	}
	if _, err := e.svc.ListBlockGroupResources(t.Context(), "capacityblockgroup-e00nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("ListBlockGroupResources(unknown) error = %v, want not_found", err)
	}

	if err := e.svc.Release(t.Context(), "c1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	st = e.blockGroup(t, a).GetStatus()
	if st.GetUsage() != 2 || st.GetUsagePercentage() != "12.5%" {
		t.Errorf("usage, percentage after a release = %d, %q; want 2, 12.5%%", st.GetUsage(), st.GetUsagePercentage())
	}
	if err := e.svc.Release(t.Context(), "c2"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	st = e.blockGroup(t, a).GetStatus()
	if st.GetUsage() != 0 || st.GetUsageState() != capacitypb.CapacityBlockGroupStatus_USAGE_STATE_NOT_USED {
		t.Errorf("usage, state after releasing everything = %d, %v; want 0, NOT_USED", st.GetUsage(), st.GetUsageState())
	}
}

func TestUsageOfAnExpiredGroupIsFullyUsed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	a := gid(t, "a")
	if _, err := e.svc.Reserve(t.Context(), h100("c1", 4, a)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	e.clk.Advance(400 * time.Hour * 24)
	st := e.blockGroup(t, a).GetStatus()
	if st.GetCurrentLimit() != 0 || st.GetUsage() != 4 || st.GetUsagePercentage() != "100%" {
		t.Errorf("limit, usage, percentage = %d, %d, %q; want 0, 4, 100%%", st.GetCurrentLimit(), st.GetUsage(), st.GetUsagePercentage())
	}
}

func TestResourceAdviceReflectsReservations(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	a := gid(t, "a")

	advice := func() map[string]*capacitypb.ResourceAdviceStatus_Availability {
		page, err := e.svc.ListResourceAdvice(t.Context(), tenantID, 0, "")
		if err != nil {
			t.Fatalf("ListResourceAdvice: %v", err)
		}
		out := map[string]*capacitypb.ResourceAdviceStatus_Availability{}
		for _, it := range page.Items {
			ci := it.GetSpec().GetComputeInstance()
			out[it.GetSpec().GetFabric()+"/"+ci.GetPlatform()+"/"+ci.GetPreset().GetName()] = it.GetStatus().GetReserved()
		}
		return out
	}
	const (
		aOne   = "fabric-2/gpu-h100-sxm/1gpu"
		aEight = "fabric-2/gpu-h100-sxm/8gpu"
		bEight = "fabric-3/gpu-h100-sxm/8gpu"
		cEight = "fabric-7/gpu-h200-sxm/8gpu"
	)
	type want struct {
		available, limit uint32
		level            capacitypb.ResourceAdviceStatus_Availability_AvailabilityLevel
	}
	const (
		high    = capacitypb.ResourceAdviceStatus_Availability_AVAILABILITY_LEVEL_HIGH
		reached = capacitypb.ResourceAdviceStatus_Availability_AVAILABILITY_LEVEL_LIMIT_REACHED
	)
	check := func(label string, wants map[string]want) {
		t.Helper()
		got := advice()
		if len(got) != 5 {
			t.Fatalf("%s: %d advice items, want 5 (presets of the platforms of a, b and c): %v", label, len(got), got)
		}
		for k, w := range wants {
			g, ok := got[k]
			if !ok {
				t.Errorf("%s: no advice for %s", label, k)
				continue
			}
			if g.GetAvailable() != w.available || g.GetLimit() != w.limit || g.GetAvailabilityLevel() != w.level ||
				g.GetDataState() != capacitypb.ResourceAdviceStatus_Availability_DATA_STATE_FRESH {
				t.Errorf("%s: %s = available %d, limit %d, level %v, state %v; want %d, %d, %v, FRESH",
					label, k, g.GetAvailable(), g.GetLimit(), g.GetAvailabilityLevel(), g.GetDataState(), w.available, w.limit, w.level)
			}
		}
	}

	check("idle", map[string]want{
		aOne: {16, 16, high}, aEight: {2, 2, high}, bEight: {1, 1, high}, cEight: {4, 4, high},
	})
	if _, err := e.svc.Reserve(t.Context(), h100("c1", 10, a)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	check("10 of 16 reserved", map[string]want{
		aOne: {6, 16, high}, aEight: {0, 2, reached}, bEight: {1, 1, high}, cEight: {4, 4, high},
	})
	if err := e.svc.Release(t.Context(), "c1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	check("released", map[string]want{
		aOne: {16, 16, high}, aEight: {2, 2, high}, bEight: {1, 1, high}, cEight: {4, 4, high},
	})
}

func TestResourceAdviceShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	page, err := e.svc.ListResourceAdvice(t.Context(), tenantID, 0, "")
	if err != nil {
		t.Fatalf("ListResourceAdvice: %v", err)
	}
	it := page.Items[0]
	if it.GetMetadata().GetParentId() != tenantID || it.GetSpec().GetRegion() != "eu-north1" {
		t.Errorf("parent, region = %s, %s; want %s, eu-north1", it.GetMetadata().GetParentId(), it.GetSpec().GetRegion(), tenantID)
	}
	ci := it.GetSpec().GetComputeInstance()
	if ci.GetGpuMemoryGigabytes() != 80 || ci.GetPreset().GetResources().GetGpuCount() != 1 || ci.GetPreset().GetResources().GetVcpuCount() != 16 {
		t.Errorf("compute instance = %v, want 80 GB GPUs and the 1gpu preset", ci)
	}
	// The mocker has no on-demand or preemptible capacity to report.
	st := it.GetStatus()
	for name, av := range map[string]*capacitypb.ResourceAdviceStatus_Availability{"on_demand": st.GetOnDemand(), "preemptible": st.GetPreemptible()} {
		if av.GetDataState() != capacitypb.ResourceAdviceStatus_Availability_DATA_STATE_UNKNOWN ||
			av.GetAvailabilityLevel() != capacitypb.ResourceAdviceStatus_Availability_AVAILABILITY_LEVEL_UNKNOWN {
			t.Errorf("%s = %v, want unknown data and level", name, av)
		}
	}

	other, err := e.svc.ListResourceAdvice(t.Context(), otherTenantID, 0, "")
	if err != nil || len(other.Items) != 2 {
		t.Errorf("ListResourceAdvice(other tenant) = %d items, %v; want the 2 presets of its h100 group", len(other.Items), err)
	}
	if _, err := e.svc.ListResourceAdvice(t.Context(), "tenant-e00unknown", 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("ListResourceAdvice(unknown tenant) error = %v, want not_found", err)
	}
	if _, err := e.svc.ListResourceAdvice(t.Context(), projectID, 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("ListResourceAdvice(project) error = %v, want bad_request", err)
	}
}
