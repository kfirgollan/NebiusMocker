package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
)

func TestCapacityResourceAdviceList(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	list, err := e.advice.List(t.Context(), &capacitypb.ListResourceAdviceRequest{ParentId: e.inst.TenantID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Three block groups, two presets per platform.
	if len(list.GetItems()) != 6 {
		t.Fatalf("List returned %d items, want 6", len(list.GetItems()))
	}
	type key struct{ platform, preset string }
	seen := map[key]*capacitypb.ResourceAdvice{}
	for _, item := range list.GetItems() {
		ci := item.GetSpec().GetComputeInstance()
		seen[key{ci.GetPlatform(), ci.GetPreset().GetName()}] = item
		if item.GetMetadata().GetParentId() != e.inst.TenantID {
			t.Errorf("parent = %q, want the tenant", item.GetMetadata().GetParentId())
		}
		for name, av := range map[string]*capacitypb.ResourceAdviceStatus_Availability{
			"on_demand": item.GetStatus().GetOnDemand(), "preemptible": item.GetStatus().GetPreemptible(),
		} {
			if av.GetDataState() != capacitypb.ResourceAdviceStatus_Availability_DATA_STATE_UNKNOWN ||
				av.GetAvailabilityLevel() != capacitypb.ResourceAdviceStatus_Availability_AVAILABILITY_LEVEL_UNKNOWN {
				t.Errorf("%s/%s %s = %v, want UNKNOWN", ci.GetPlatform(), ci.GetPreset().GetName(), name, av)
			}
		}
	}
	if len(seen) != 6 {
		t.Errorf("distinct (platform, preset) = %d, want 6", len(seen))
	}

	// h200 fabric-7 holds 32 GPUs: the 8-GPU preset fits 4 times.
	item := seen[key{capH200Platform, "8gpu-128vcpu-1600gb"}]
	if item == nil {
		t.Fatalf("no advice for %s 8gpu preset", capH200Platform)
	}
	if item.GetSpec().GetRegion() != "eu-north1" || item.GetSpec().GetFabric() != capH200Fabric {
		t.Errorf("spec = %v", item.GetSpec())
	}
	if r := item.GetSpec().GetComputeInstance().GetPreset().GetResources(); r.GetGpuCount() != 8 || r.GetVcpuCount() != 128 {
		t.Errorf("preset resources = %v", r)
	}
	r := item.GetStatus().GetReserved()
	if r.GetAvailable() != 4 || r.GetLimit() != 4 ||
		r.GetDataState() != capacitypb.ResourceAdviceStatus_Availability_DATA_STATE_FRESH ||
		r.GetAvailabilityLevel() != capacitypb.ResourceAdviceStatus_Availability_AVAILABILITY_LEVEL_HIGH {
		t.Errorf("reserved = %v, want available 4, limit 4, FRESH, HIGH", r)
	}
	// The 1-GPU preset fits 32 times.
	if r := seen[key{capH200Platform, "1gpu-16vcpu-200gb"}].GetStatus().GetReserved(); r.GetAvailable() != 32 || r.GetLimit() != 32 {
		t.Errorf("1gpu reserved = %v, want 32 / 32", r)
	}
}

func TestCapacityResourceAdvicePaging(t *testing.T) {
	t.Parallel()
	e := startCapacity(t)
	ctx := t.Context()
	seen := map[string]bool{}
	token := ""
	pages := 0
	for range 10 {
		page, err := e.advice.List(ctx, &capacitypb.ListResourceAdviceRequest{ParentId: e.inst.TenantID, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.GetItems()) != 1 {
			t.Fatalf("page %d has %d items, want 1", pages, len(page.GetItems()))
		}
		pages++
		ci := page.GetItems()[0].GetSpec().GetComputeInstance()
		seen[ci.GetPlatform()+"/"+ci.GetPreset().GetName()] = true
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if pages != 6 || len(seen) != 6 {
		t.Errorf("paged through %d pages, %d distinct items, want 6 and 6", pages, len(seen))
	}
	_, err := e.advice.List(ctx, &capacitypb.ListResourceAdviceRequest{ParentId: e.inst.TenantID, PageSize: -1})
	capWantCode(t, err, codes.InvalidArgument)
}
