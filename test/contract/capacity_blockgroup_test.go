package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func affinity(platform, fabric string) *capacitypb.ResourceAffinity {
	return &capacitypb.ResourceAffinity{Versions: &capacitypb.ResourceAffinity_ComputeV1{
		ComputeV1: &capacitypb.ResourceAffinityComputeV1{Platform: platform, Fabric: fabric},
	}}
}

func TestCapacityBlockGroupListAndGet(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	ctx := t.Context()

	list, err := e.groups.List(ctx, &capacitypb.ListCapacityBlockGroupsRequest{ParentId: e.inst.TenantID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.GetItems()) != 3 {
		t.Fatalf("List returned %d block groups, want 3", len(list.GetItems()))
	}
	type want struct {
		region, fabric string
		limit          uint64
		state          capacitypb.CapacityBlockGroupStatus_State
	}
	wants := map[string]want{
		capH100Platform: {"eu-north1", capH100Fabric, 16, capacitypb.CapacityBlockGroupStatus_STATE_ACTIVE},
		capH200Platform: {"eu-north1", capH200Fabric, 32, capacitypb.CapacityBlockGroupStatus_STATE_ACTIVE},
		capB200Platform: {"us-central1", capB200Fabric, 0, capacitypb.CapacityBlockGroupStatus_STATE_INACTIVE},
	}
	for _, g := range list.GetItems() {
		platform := g.GetStatus().GetResourceAffinity().GetComputeV1().GetPlatform()
		w, ok := wants[platform]
		if !ok {
			t.Errorf("unexpected block group for platform %q", platform)
			continue
		}
		if g.GetMetadata().GetId() == "" || g.GetMetadata().GetParentId() != e.inst.TenantID {
			t.Errorf("%s: metadata = %v", platform, g.GetMetadata())
		}
		if g.GetStatus().GetRegion() != w.region || g.GetStatus().GetResourceAffinity().GetComputeV1().GetFabric() != w.fabric {
			t.Errorf("%s: spec = %v, want region %s fabric %s", platform, g.GetSpec(), w.region, w.fabric)
		}
		if g.GetStatus().GetState() != w.state || g.GetStatus().GetCurrentLimit() != w.limit {
			t.Errorf("%s: state %v limit %d, want %v / %d", platform, g.GetStatus().GetState(), g.GetStatus().GetCurrentLimit(), w.state, w.limit)
		}
		if g.GetStatus().GetService() != "compute" {
			t.Errorf("%s: service = %q, want compute", platform, g.GetStatus().GetService())
		}
		got, err := e.groups.Get(ctx, &capacitypb.GetCapacityBlockGroupRequest{Id: g.GetMetadata().GetId()})
		if err != nil {
			t.Errorf("%s: Get: %v", platform, err)
			continue
		}
		if got.GetMetadata().GetId() != g.GetMetadata().GetId() || got.GetStatus().GetCurrentLimit() != w.limit {
			t.Errorf("%s: Get = %v, differs from List item", platform, got)
		}
	}
}

func TestCapacityBlockGroupPaging(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	ctx := t.Context()
	seen := map[string]bool{}
	token := ""
	for range 5 {
		page, err := e.groups.List(ctx, &capacitypb.ListCapacityBlockGroupsRequest{ParentId: e.inst.TenantID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.GetItems()) > 2 {
			t.Fatalf("page has %d items, want <= 2", len(page.GetItems()))
		}
		for _, g := range page.GetItems() {
			seen[g.GetMetadata().GetId()] = true
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Errorf("paging saw %d distinct groups, want 3", len(seen))
	}
}

func TestCapacityBlockGroupGetByResourceAffinity(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	ctx := t.Context()
	h200 := e.groupByPlatform(t, capH200Platform)
	get := func(region string, aff *capacitypb.ResourceAffinity) (*capacitypb.CapacityBlockGroup, error) {
		return e.groups.GetByResourceAffinity(ctx, &capacitypb.GetCapacityBlockGroupByResourceAffinityRequest{
			ParentId: e.inst.TenantID, Region: region, ResourceAffinity: aff,
		})
	}

	// With and without the fabric.
	for _, fabric := range []string{capH200Fabric, ""} {
		g, err := get("eu-north1", affinity(capH200Platform, fabric))
		if err != nil {
			t.Fatalf("fabric %q: %v", fabric, err)
		}
		if g.GetMetadata().GetId() != h200.GetMetadata().GetId() {
			t.Errorf("fabric %q: got %s, want %s", fabric, g.GetMetadata().GetId(), h200.GetMetadata().GetId())
		}
	}

	_, err := get("eu-north1", affinity("gpu-unknown", ""))
	capWantCode(t, err, codes.NotFound)
	_, err = get("eu-north1", affinity(capH200Platform, "fabric-999"))
	capWantCode(t, err, codes.NotFound)
	// Region mismatch: the h200 group is in eu-north1.
	_, err = get("us-central1", affinity(capH200Platform, capH200Fabric))
	capWantCode(t, err, codes.NotFound)
	// No affinity at all.
	_, err = get("eu-north1", nil)
	capWantCode(t, err, codes.InvalidArgument)
}

func TestCapacityBlockGroupListResourcesEmpty(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	g := e.groupByPlatform(t, capH100Platform)
	res, err := e.groups.ListResources(t.Context(), &capacitypb.ListCapacityBlockGroupResourcesRequest{Id: g.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(res.GetResourceIds()) != 0 {
		t.Errorf("resources = %v, want none", res.GetResourceIds())
	}
	_, err = e.groups.ListResources(t.Context(), &capacitypb.ListCapacityBlockGroupResourcesRequest{Id: "capacityblockgroup-e00doesnotexist"})
	capWantCode(t, err, codes.NotFound)
}

func TestCapacityBlockGroupErrors(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	ctx := t.Context()
	_, err := e.groups.Get(ctx, &capacitypb.GetCapacityBlockGroupRequest{Id: "capacityblockgroup-e00doesnotexist"})
	capWantCode(t, err, codes.NotFound)
	_, err = e.groups.List(ctx, &capacitypb.ListCapacityBlockGroupsRequest{ParentId: e.inst.TenantID, PageSize: -1})
	capWantCode(t, err, codes.InvalidArgument)
	_, err = e.groups.List(ctx, &capacitypb.ListCapacityBlockGroupsRequest{ParentId: e.inst.TenantID, PageSize: 1_000_000})
	capWantCode(t, err, codes.InvalidArgument)
	_, err = e.groups.List(ctx, &capacitypb.ListCapacityBlockGroupsRequest{ParentId: e.inst.TenantID, PageToken: "garbage"})
	capWantCode(t, err, codes.InvalidArgument)
}

func TestCapacityIntervalsOverTime(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(capacityTestStart)
	// Operations complete inline: on a manual clock a delay would never pass.
	e := startCapacity(t, testkit.WithClock(clk), testkit.WithOperationDelay(0))
	ctx := t.Context()
	h100 := e.groupByPlatform(t, capH100Platform)
	gid := h100.GetMetadata().GetId()

	// Before the second interval starts.
	if st := h100.GetStatus(); st.GetCurrentLimit() != 16 || st.GetState() != capacitypb.CapacityBlockGroupStatus_STATE_ACTIVE {
		t.Fatalf("status = %v, want ACTIVE with limit 16", st)
	}
	st := h100.GetStatus()
	wantNext := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	if !st.GetNextChangeAt().AsTime().Equal(wantNext) || st.GetNextChangeTo() != 24 {
		t.Errorf("next change = %v -> %d, want %v -> 24", st.GetNextChangeAt().AsTime(), st.GetNextChangeTo(), wantNext)
	}

	intervals := func() map[uint64]*capacitypb.CapacityInterval {
		list, err := e.intervals.List(ctx, &capacitypb.ListCapacityIntervalsRequest{ParentId: gid})
		if err != nil {
			t.Fatalf("intervals List: %v", err)
		}
		out := map[uint64]*capacitypb.CapacityInterval{}
		for _, iv := range list.GetItems() {
			if iv.GetMetadata().GetParentId() != gid || iv.GetStatus().GetContainerId() != e.inst.TenantID {
				t.Errorf("interval parent %q container %q, want %s and the tenant", iv.GetMetadata().GetParentId(), iv.GetStatus().GetContainerId(), gid)
			}
			out[iv.GetStatus().GetQuantity()] = iv
		}
		return out
	}
	ivs := intervals()
	if len(ivs) != 2 || ivs[16] == nil || ivs[8] == nil {
		t.Fatalf("intervals = %v, want quantities 16 and 8", ivs)
	}
	if got := ivs[16].GetStatus().GetState(); got != capacitypb.CapacityIntervalStatus_STATE_ACTIVE {
		t.Errorf("16-GPU interval state = %v, want ACTIVE", got)
	}
	if got := ivs[8].GetStatus().GetState(); got != capacitypb.CapacityIntervalStatus_STATE_SCHEDULED {
		t.Errorf("8-GPU interval state = %v, want SCHEDULED", got)
	}
	if !ivs[8].GetStatus().GetStartTime().AsTime().Equal(wantNext) {
		t.Errorf("8-GPU interval start = %v, want %v", ivs[8].GetStatus().GetStartTime().AsTime(), wantNext)
	}
	if ivs[8].GetStatus().GetRegion() != "eu-north1" || ivs[8].GetStatus().GetService() != "compute" {
		t.Errorf("8-GPU interval spec = %v", ivs[8].GetSpec())
	}
	got, err := e.intervals.Get(ctx, &capacitypb.GetCapacityIntervalRequest{Id: ivs[8].GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("interval Get: %v", err)
	}
	if got.GetStatus().GetQuantity() != 8 {
		t.Errorf("interval Get quantity = %d, want 8", got.GetStatus().GetQuantity())
	}

	// Past the start of the scheduled interval.
	clk.Advance(14 * 24 * time.Hour)
	g, err := e.groups.Get(ctx, &capacitypb.GetCapacityBlockGroupRequest{Id: gid})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if g.GetStatus().GetCurrentLimit() != 24 {
		t.Errorf("current_limit = %d, want 24", g.GetStatus().GetCurrentLimit())
	}
	ivs = intervals()
	if got := ivs[8].GetStatus().GetState(); got != capacitypb.CapacityIntervalStatus_STATE_ACTIVE {
		t.Errorf("8-GPU interval state after 14 days = %v, want ACTIVE", got)
	}

	// The b200 group starts in 30 days: INACTIVE now, ACTIVE then.
	b200 := e.groupByPlatform(t, capB200Platform)
	if b200.GetStatus().GetState() != capacitypb.CapacityBlockGroupStatus_STATE_INACTIVE {
		t.Errorf("b200 state at +14d = %v, want INACTIVE", b200.GetStatus().GetState())
	}
	clk.Advance(17 * 24 * time.Hour)
	b200 = e.groupByPlatform(t, capB200Platform)
	if b200.GetStatus().GetState() != capacitypb.CapacityBlockGroupStatus_STATE_ACTIVE || b200.GetStatus().GetCurrentLimit() != 64 {
		t.Errorf("b200 at +31d = %v / %d, want ACTIVE / 64", b200.GetStatus().GetState(), b200.GetStatus().GetCurrentLimit())
	}
}

func TestCapacityIntervalErrors(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	ctx := t.Context()
	_, err := e.intervals.Get(ctx, &capacitypb.GetCapacityIntervalRequest{Id: "capacityinterval-e00doesnotexist"})
	capWantCode(t, err, codes.NotFound)
	_, err = e.intervals.List(ctx, &capacitypb.ListCapacityIntervalsRequest{ParentId: "capacityblockgroup-e00doesnotexist"})
	capWantCode(t, err, codes.NotFound)
	g := e.groupByPlatform(t, capH100Platform)
	_, err = e.intervals.List(ctx, &capacitypb.ListCapacityIntervalsRequest{ParentId: g.GetMetadata().GetId(), PageSize: -5})
	capWantCode(t, err, codes.InvalidArgument)
}

// wantCode fails the test unless err carries the gRPC code.
func capWantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v, want %v (err: %v)", got, want, err)
	}
}
