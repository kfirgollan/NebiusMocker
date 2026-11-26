package domain

import (
	"context"
	"fmt"
	"sync"
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// fakeBlockGroup is one capacity block group of fakeReservations.
type fakeBlockGroup struct {
	region, platform, fabric string
	limit                    uint64
}

type fakeHold struct {
	group    string
	quantity uint64
}

// fakeReservations is the capacity reservations capability: block groups
// with a GPU limit, holds per consumer, the documented error kinds.
type fakeReservations struct {
	mu       sync.Mutex
	disabled bool
	groups   map[string]fakeBlockGroup
	order    []string
	holds    map[string]fakeHold
	requests []peer.CapacityRequest
}

const (
	testCBG      = "capacityblockgroup-e00h100"
	testCBGSmall = "capacityblockgroup-e00small"
)

func newFakeReservations() *fakeReservations {
	f := &fakeReservations{groups: map[string]fakeBlockGroup{}, holds: map[string]fakeHold{}}
	f.add(testCBG, fakeBlockGroup{region: "eu-north1", platform: "gpu-h100-sxm", fabric: "fabric-2", limit: 16})
	f.add(testCBGSmall, fakeBlockGroup{region: "eu-north1", platform: "gpu-h100-sxm", fabric: "fabric-3", limit: 8})
	return f
}

func (f *fakeReservations) add(id string, g fakeBlockGroup) {
	f.groups[id] = g
	f.order = append(f.order, id)
}

func (f *fakeReservations) CapacityReservations() (peer.CapacityReservations, bool) {
	return f, !f.disabled
}

func (f *fakeReservations) Reserve(_ context.Context, r peer.CapacityRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	matches := func(g fakeBlockGroup) bool {
		return g.region == r.Region && g.platform == r.Platform && (r.Fabric == "" || g.fabric == r.Fabric)
	}
	candidates := r.ReservationIDs
	if len(candidates) == 0 {
		for _, id := range f.order {
			if matches(f.groups[id]) {
				candidates = append(candidates, id)
			}
		}
	}
	for i, id := range candidates {
		g, ok := f.groups[id]
		if !ok {
			return "", errs.NotFound(id)
		}
		if !matches(g) {
			return "", errs.BadRequest(errs.Violation{Field: fmt.Sprintf("reservation_policy.reservation_ids[%d]", i), Message: "mismatch"})
		}
	}
	for _, id := range candidates {
		var used uint64
		for c, h := range f.holds {
			if h.group == id && c != r.ConsumerID {
				used += h.quantity
			}
		}
		if used+r.Quantity <= f.groups[id].limit {
			f.holds[r.ConsumerID] = fakeHold{group: id, quantity: r.Quantity}
			return id, nil
		}
	}
	return "", &errs.Error{Kind: errs.KindQuotaExceeded, Message: "not enough reserved capacity"}
}

func (f *fakeReservations) Release(_ context.Context, consumerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.holds, consumerID)
	return nil
}

func (f *fakeReservations) holdOf(consumerID string) (fakeHold, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.holds[consumerID]
	return h, ok
}

func reservationSpec(policy computepb.ReservationPolicy_Policy, ids ...string) *computepb.InstanceSpec {
	return edited(gpuSpec(), func(s *computepb.InstanceSpec) {
		s.ReservationPolicy = &computepb.ReservationPolicy{Policy: policy, ReservationIds: ids}
	})
}

// mk creates an instance whose managed boot disk is named after it.
func (f *instFixture) mk(t *testing.T, name string, spec *computepb.InstanceSpec) *computepb.Instance {
	t.Helper()
	spec.BootDisk = managedDisk(name+"-boot", 20)
	return f.mustCreate(t, name, spec)
}

func wantHold(t *testing.T, f *instFixture, inst *computepb.Instance, group string) {
	t.Helper()
	id := inst.GetMetadata().GetId()
	if got := f.get(t, id).GetStatus().GetReservationId(); got != group {
		t.Fatalf("status.reservation_id = %q, want %q", got, group)
	}
	h, ok := f.res.holdOf(id)
	switch {
	case group == "" && ok:
		t.Fatalf("instance in the common pool holds %+v", h)
	case group != "" && (!ok || h.group != group || h.quantity != 8):
		t.Fatalf("hold = %+v (%v), want 8 GPUs of %s", h, ok, group)
	}
}

func TestInstanceReservationAuto(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	a := f.mk(t, "a", gpuSpec()) // AUTO by default
	wantHold(t, f, a, testCBG)
	if r := f.res.requests[0]; r.ProjectID != testProject || r.Region != "eu-north1" || r.Platform != "gpu-h100-sxm" || r.Fabric != "" || r.Quantity != 8 {
		t.Fatalf("request = %+v", r)
	}
	b := f.mk(t, "b", gpuSpec())
	wantHold(t, f, b, testCBG)
	c := f.mk(t, "c", gpuSpec()) // testCBG is full: next matching group
	wantHold(t, f, c, testCBGSmall)
	d := f.mk(t, "d", gpuSpec()) // all full: common pool
	wantHold(t, f, d, "")

	if err := f.remove(a.GetMetadata().GetId()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.res.holdOf(a.GetMetadata().GetId()); ok {
		t.Fatal("delete kept the reservation")
	}
}

func TestInstanceReservationStrict(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	f.mk(t, "a", reservationSpec(computepb.ReservationPolicy_STRICT, testCBGSmall))
	_, err := f.create("b", withBoot("b", reservationSpec(computepb.ReservationPolicy_STRICT, testCBGSmall)))
	wantKind(t, err, errs.KindQuotaExceeded, "")
	if n := len(f.repo.instances.byID); n != 1 || len(f.res.holds) != 1 {
		t.Fatalf("a failed STRICT create left %d instances, %d holds", n, len(f.res.holds))
	}
	f.res.disabled = true
	_, err = f.create("c", withBoot("c", reservationSpec(computepb.ReservationPolicy_STRICT)))
	wantKind(t, err, errs.KindBadState, "")
}

func TestInstanceReservationExplicitIDs(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mk(t, "a", reservationSpec(computepb.ReservationPolicy_AUTO, testCBGSmall, testCBG))
	wantHold(t, f, inst, testCBGSmall)

	_, err := f.create("b", withBoot("b", reservationSpec(computepb.ReservationPolicy_AUTO, "capacityblockgroup-e00nope")))
	wantKind(t, err, errs.KindNotFound, "")
	f.res.add("capacityblockgroup-e00h200", fakeBlockGroup{region: "eu-north1", platform: "gpu-h200-sxm", limit: 8})
	_, err = f.create("c", withBoot("c", reservationSpec(computepb.ReservationPolicy_AUTO, "capacityblockgroup-e00h200")))
	wantKind(t, err, errs.KindBadRequest, "spec.reservation_policy.reservation_ids[0]")
	_, err = f.create("d", withBoot("d", reservationSpec(computepb.ReservationPolicy_FORBID, testCBG)))
	wantKind(t, err, errs.KindBadRequest, reservationIDsField)
	if len(f.res.holds) != 1 {
		t.Fatalf("holds = %v", f.res.holds)
	}
}

func TestInstanceReservationNotEligible(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	wantHold(t, f, f.mk(t, "cpu", baseSpec()), "")
	wantHold(t, f, f.mk(t, "forbid", reservationSpec(computepb.ReservationPolicy_FORBID)), "")
	if len(f.res.requests) != 0 {
		t.Fatalf("requests = %v", f.res.requests)
	}
}

func TestInstanceReservationGpuClusterFabric(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := edited(gpuSpec(), func(s *computepb.InstanceSpec) {
		s.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: "computegpucluster-e00c1"}
	})
	wantHold(t, f, f.mk(t, "a", spec), testCBG)
	if r := f.res.requests[0]; r.Fabric != "fabric-2" {
		t.Fatalf("fabric = %q", r.Fabric)
	}
}

func TestInstanceReservationStopStart(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mk(t, "a", reservationSpec(computepb.ReservationPolicy_STRICT, testCBGSmall))
	id := inst.GetMetadata().GetId()
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	wantHold(t, f, inst, testCBGSmall) // held while STOPPED
	// The stopped instance's GPUs are not free for another STRICT instance.
	_, err := f.create("b", withBoot("b", reservationSpec(computepb.ReservationPolicy_STRICT, testCBGSmall)))
	wantKind(t, err, errs.KindQuotaExceeded, "")
	if err := f.start(id); err != nil {
		t.Fatal(err)
	}
	wantHold(t, f, inst, testCBGSmall)
}

func TestInstanceReservationStartDraws(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	full := f.mk(t, "full", reservationSpec(computepb.ReservationPolicy_STRICT, testCBGSmall))
	pool := f.mk(t, "pool", reservationSpec(computepb.ReservationPolicy_AUTO, testCBGSmall))
	wantHold(t, f, pool, "")
	if err := f.stop(pool.GetMetadata().GetId()); err != nil {
		t.Fatal(err)
	}
	if err := f.remove(full.GetMetadata().GetId()); err != nil {
		t.Fatal(err)
	}
	if err := f.start(pool.GetMetadata().GetId()); err != nil {
		t.Fatal(err)
	}
	wantHold(t, f, pool, testCBGSmall) // room again: the start draws from the reservation
}

func TestInstanceReservationUpdate(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mk(t, "a", reservationSpec(computepb.ReservationPolicy_AUTO, testCBG))
	id := inst.GetMetadata().GetId()
	toSmall := func(s *computepb.InstanceSpec) { s.ReservationPolicy.ReservationIds = []string{testCBGSmall} }
	wantKind(t, f.update(t, id, toSmall, nil), errs.KindBadState, "")

	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Stopped = true }, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.update(t, id, toSmall, nil); err != nil {
		t.Fatal(err)
	}
	wantHold(t, f, inst, "") // the policy change dropped the reservation
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Stopped = false }, nil); err != nil {
		t.Fatal(err)
	}
	wantHold(t, f, inst, testCBGSmall) // the update started it: it drew again
}

func TestInstanceReservationStrictStartFails(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mk(t, "a", reservationSpec(computepb.ReservationPolicy_FORBID))
	id := inst.GetMetadata().GetId()
	f.mk(t, "full", reservationSpec(computepb.ReservationPolicy_STRICT, testCBGSmall))
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	if err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.ReservationPolicy = &computepb.ReservationPolicy{Policy: computepb.ReservationPolicy_STRICT, ReservationIds: []string{testCBGSmall}}
	}, nil); err != nil {
		t.Fatal(err)
	}
	wantKind(t, f.start(id), errs.KindQuotaExceeded, "")
	if got := instState(f.get(t, id)); got != computepb.InstanceStatus_STOPPED {
		t.Fatalf("state after a failed STRICT start = %v", got)
	}
}

func TestInstanceReservationRunningUpdateDoesNotDraw(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	full := f.mk(t, "full", reservationSpec(computepb.ReservationPolicy_STRICT, testCBGSmall))
	pool := f.mk(t, "pool", reservationSpec(computepb.ReservationPolicy_AUTO, testCBGSmall))
	if err := f.remove(full.GetMetadata().GetId()); err != nil {
		t.Fatal(err)
	}
	if err := f.update(t, pool.GetMetadata().GetId(), func(s *computepb.InstanceSpec) { s.Hostname = "renamed" }, nil); err != nil {
		t.Fatal(err)
	}
	wantHold(t, f, pool, "") // only a start draws
}

func withBoot(name string, spec *computepb.InstanceSpec) *computepb.InstanceSpec {
	spec.BootDisk = managedDisk(name+"-boot", 20)
	return spec
}
