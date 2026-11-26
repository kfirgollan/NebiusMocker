package domain

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

const (
	testPolicy      = "pricingpolicy-e00allowed"
	testPolicyOther = "pricingpolicy-e00second"
)

// spotSpec is a preemptible 1-GPU H100 instance under the pricing policy id.
func spotSpec(id string) *computepb.InstanceSpec {
	s := baseSpec()
	s.Resources = &computepb.ResourcesSpec{Platform: "gpu-h100-sxm", Size: presetSize("1gpu-16vcpu-200gb")}
	s.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
	s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
	s.PricingModel = &computepb.InstanceSpec_SpotPricingPolicy{SpotPricingPolicy: &computepb.SpotPricingPolicySpec{Id: id}}
	return s
}

// spotPolicy is an ACTIVE, schedulable H100 policy of testProject.
func spotPolicy(id string) peer.PricingPolicy {
	return peer.PricingPolicy{ID: id, ProjectID: testProject, Platform: "gpu-h100-sxm", MaxPrice: "2.000", State: peer.PricingPolicyActive}
}

func newSpotFixture(t *testing.T) *instFixture {
	t.Helper()
	f := newInstFixture(t)
	f.pricing.put(spotPolicy(testPolicy))
	f.pricing.put(spotPolicy(testPolicyOther))
	return f
}

func (f *instFixture) running(t *testing.T, policy string) int64 {
	t.Helper()
	n, err := f.svc.RunningVMCount(t.Context(), testProject, policy)
	if err != nil {
		t.Fatalf("RunningVMCount: %v", err)
	}
	return n
}

func TestInstancePricingModelRules(t *testing.T) {
	t.Parallel()
	preemptible := func(s *computepb.InstanceSpec) { *s = *spotSpec(testPolicy) }
	cases := []struct {
		name  string
		edit  func(*computepb.InstanceSpec)
		kind  errs.Kind
		field string
	}{
		{"spot policy on a regular instance", func(s *computepb.InstanceSpec) {
			s.PricingModel = &computepb.InstanceSpec_SpotPricingPolicy{SpotPricingPolicy: &computepb.SpotPricingPolicySpec{Id: testPolicy}}
		}, errs.KindBadRequest, "spec.spot_pricing_policy"},
		{"follows_spot_price on a regular instance", func(s *computepb.InstanceSpec) {
			s.PricingModel = &computepb.InstanceSpec_FollowsSpotPrice{FollowsSpotPrice: &computepb.FollowsSpotPriceSpec{}}
		}, errs.KindBadRequest, "spec.follows_spot_price"},
		{"on_demand on a preemptible instance", func(s *computepb.InstanceSpec) {
			preemptible(s)
			s.PricingModel = &computepb.InstanceSpec_OnDemand{OnDemand: &computepb.OnDemandSpec{}}
		}, errs.KindBadRequest, "spec.on_demand"},
		{"no policy id", func(s *computepb.InstanceSpec) { *s = *spotSpec("") }, errs.KindBadRequest, spotPolicyField},
		{"not a policy id", func(s *computepb.InstanceSpec) { *s = *spotSpec("project-e00x") }, errs.KindBadRequest, spotPolicyField},
		{"unknown policy", func(s *computepb.InstanceSpec) { *s = *spotSpec("pricingpolicy-e00nope") }, errs.KindNotFound, ""},
		{"policy of another project", func(s *computepb.InstanceSpec) { *s = *spotSpec("pricingpolicy-e00otherproj") }, errs.KindBadRequest, spotPolicyField},
		{"policy of another platform", func(s *computepb.InstanceSpec) { *s = *spotSpec("pricingpolicy-e00h200") }, errs.KindBadRequest, spotPolicyField},
		{"policy being deleted", func(s *computepb.InstanceSpec) { *s = *spotSpec("pricingpolicy-e00deleting") }, errs.KindBadState, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSpotFixture(t)
			other := spotPolicy("pricingpolicy-e00otherproj")
			other.ProjectID = otherProject
			f.pricing.put(other)
			h200 := spotPolicy("pricingpolicy-e00h200")
			h200.Platform = "gpu-h200-sxm"
			f.pricing.put(h200)
			deleting := spotPolicy("pricingpolicy-e00deleting")
			deleting.State = peer.PricingPolicyDeleting
			f.pricing.put(deleting)
			_, err := f.create("vm", edited(baseSpec(), tc.edit))
			wantKind(t, err, tc.kind, tc.field)
			if f.repo.count() != 0 {
				t.Errorf("a rejected create stored an instance")
			}
		})
	}
}

func TestInstanceSpotPolicyBillingDisabled(t *testing.T) {
	t.Parallel()
	f := newSpotFixture(t)
	f.pricing.disabled = true
	_, err := f.create("vm", spotSpec(testPolicy))
	wantKind(t, err, errs.KindBadRequest, spotPolicyField)
}

func TestInstanceSpotPolicyRunningCount(t *testing.T) {
	t.Parallel()
	f := newSpotFixture(t)
	a := f.mustCreate(t, "a", spotSpec(testPolicy))
	f.mustCreate(t, "b", edited(spotSpec(testPolicy), func(s *computepb.InstanceSpec) { s.BootDisk = managedDisk("b-boot", 20) }))
	f.mustCreate(t, "c", edited(spotSpec(testPolicy), func(s *computepb.InstanceSpec) {
		s.BootDisk = managedDisk("c-boot", 20)
		s.Stopped = true
	}))
	if n := f.running(t, testPolicy); n != 2 {
		t.Fatalf("running = %d, want 2 (a stopped instance does not count)", n)
	}
	id := a.GetMetadata().GetId()
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	if n := f.running(t, testPolicy); n != 1 {
		t.Fatalf("after Stop: running = %d, want 1", n)
	}
	if err := f.start(id); err != nil {
		t.Fatal(err)
	}
	if n := f.running(t, testPolicy); n != 2 {
		t.Fatalf("after Start: running = %d, want 2", n)
	}
	if n, _ := f.svc.RunningVMCount(t.Context(), otherProject, testPolicy); n != 0 {
		t.Errorf("other project: running = %d, want 0", n)
	}
}

func TestInstanceSpotPolicyBlockedCreate(t *testing.T) {
	t.Parallel()
	f := newSpotFixture(t)
	blocked := spotPolicy(testPolicy)
	blocked.Blocked = true
	f.pricing.put(blocked)

	// docs: the request returns an error, but the VM is created STOPPED.
	op, err := f.svc.Create(f.ctx, CreateInstanceCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "vm"}, Spec: spotSpec(testPolicy),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantKind(t, op.Err, errs.KindBadState, "")
	if st := f.get(t, op.ResourceID).GetStatus().GetState(); st != computepb.InstanceStatus_STOPPED {
		t.Fatalf("state = %v, want STOPPED", st)
	}
	if n := f.running(t, testPolicy); n != 0 {
		t.Errorf("running = %d, want 0", n)
	}
	// Created stopped on purpose: no error.
	f.mustCreate(t, "stopped", edited(spotSpec(testPolicy), func(s *computepb.InstanceSpec) {
		s.BootDisk = managedDisk("stopped-boot", 20)
		s.Stopped = true
	}))
}

func TestInstanceSpotPolicyStartGate(t *testing.T) {
	t.Parallel()
	f := newSpotFixture(t)
	inst := f.mustCreate(t, "vm", edited(spotSpec(testPolicy), func(s *computepb.InstanceSpec) { s.Stopped = true }))
	id := inst.GetMetadata().GetId()
	set := func(fn func(*peer.PricingPolicy)) {
		p := spotPolicy(testPolicy)
		fn(&p)
		f.pricing.put(p)
	}
	stays := func(what string, kind errs.Kind) {
		t.Helper()
		wantKind(t, f.start(id), kind, "")
		if st := f.get(t, id).GetStatus().GetState(); st != computepb.InstanceStatus_STOPPED {
			t.Fatalf("%s: state = %v, want STOPPED", what, st)
		}
	}
	set(func(p *peer.PricingPolicy) { p.Blocked = true })
	stays("blocked", errs.KindBadState)
	set(func(p *peer.PricingPolicy) { p.State = peer.PricingPolicyUpdating })
	stays("updating", errs.KindConflict)
	set(func(p *peer.PricingPolicy) { p.State = peer.PricingPolicyCreating })
	stays("creating", errs.KindConflict)
	f.pricing.drop(testPolicy)
	stays("deleted", errs.KindNotFound)

	// docs: select another pricing policy before starting it. An update that
	// leaves the deleted reference alone is fine too.
	if err := f.update(t, id, nil, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("label update with a deleted policy: %v", err)
	}
	if err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.PricingModel = &computepb.InstanceSpec_SpotPricingPolicy{SpotPricingPolicy: &computepb.SpotPricingPolicySpec{Id: testPolicyOther}}
	}, nil); err != nil {
		t.Fatalf("switch policy: %v", err)
	}
	if err := f.start(id); err != nil {
		t.Fatalf("Start under the new policy: %v", err)
	}
	if n := f.running(t, testPolicyOther); n != 1 {
		t.Errorf("running = %d, want 1", n)
	}
}

func TestInstanceSpotPolicyUpdate(t *testing.T) {
	t.Parallel()
	f := newSpotFixture(t)
	id := f.mustCreate(t, "vm", spotSpec(testPolicy)).GetMetadata().GetId()
	usePolicy := func(policy string) func(*computepb.InstanceSpec) {
		return func(s *computepb.InstanceSpec) {
			s.PricingModel = &computepb.InstanceSpec_SpotPricingPolicy{SpotPricingPolicy: &computepb.SpotPricingPolicySpec{Id: policy}}
		}
	}

	// A running instance cannot move to a blocked policy.
	blocked := spotPolicy(testPolicyOther)
	blocked.Blocked = true
	f.pricing.put(blocked)
	wantKind(t, f.update(t, id, usePolicy(testPolicyOther), nil), errs.KindBadState, "")
	if got := f.get(t, id).GetSpec().GetSpotPricingPolicy().GetId(); got != testPolicy {
		t.Fatalf("policy = %s after a refused update, want %s", got, testPolicy)
	}

	// ... but to an allowed one; the count moves with it.
	f.pricing.put(spotPolicy(testPolicyOther))
	if err := f.update(t, id, usePolicy(testPolicyOther), nil); err != nil {
		t.Fatalf("switch policy: %v", err)
	}
	if a, b := f.running(t, testPolicy), f.running(t, testPolicyOther); a != 0 || b != 1 {
		t.Fatalf("running = %d / %d, want 0 / 1", a, b)
	}

	// Starting through spec.stopped checks the policy too.
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Stopped = true }, nil); err != nil {
		t.Fatal(err)
	}
	f.pricing.put(blocked)
	wantKind(t, f.update(t, id, func(s *computepb.InstanceSpec) { s.Stopped = false }, nil), errs.KindBadState, "")

	// Following the spot price needs no policy.
	if err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.Stopped = false
		s.PricingModel = &computepb.InstanceSpec_FollowsSpotPrice{FollowsSpotPrice: &computepb.FollowsSpotPriceSpec{}}
	}, nil); err != nil {
		t.Fatalf("switch to follows_spot_price: %v", err)
	}
	if st := f.get(t, id).GetStatus().GetState(); st != computepb.InstanceStatus_RUNNING {
		t.Errorf("state = %v, want RUNNING", st)
	}
}
