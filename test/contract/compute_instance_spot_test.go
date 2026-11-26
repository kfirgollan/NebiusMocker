package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	billingsvc "github.com/nebius/gosdk/services/nebius/billing/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/billing"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// spotEnv is a mocker with IAM, Compute, VPC and billing, enforcing authz.
type spotEnv struct {
	instClients
	policies billingsvc.PricingPolicyService
}

func startSpot(t *testing.T) spotEnv {
	t.Helper()
	env := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{vpc.New(), billing.New()},
		testkit.WithAuthz(authz.ModeEnforce))
	return spotEnv{instClients: instClientsFor(t, env, ""), policies: billingsvc.NewPricingPolicyService(env.Inst.SDK(t))}
}

// createPolicyIn creates a policy of platform in parent and returns its ID.
func (e spotEnv) createPolicyIn(t *testing.T, parent, name, platform, maxPrice string) string {
	t.Helper()
	op, err := e.policies.Create(t.Context(), &billingpb.CreatePricingPolicyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: policySpec(platform, maxPrice),
	})
	return mustOp(t, op, err)
}

// policy creates a gpu-h100-sxm policy in the seeded project.
func (e spotEnv) policy(t *testing.T, name, maxPrice string) string {
	t.Helper()
	return e.createPolicyIn(t, e.project, name, "gpu-h100-sxm", maxPrice)
}

func (e spotEnv) getPolicy(t *testing.T, id string) *billingpb.PricingPolicy {
	t.Helper()
	p, err := e.policies.Get(t.Context(), &billingpb.GetPricingPolicyRequest{Id: id})
	if err != nil {
		t.Fatalf("get pricing policy %s: %v", id, err)
	}
	return p
}

// wantRunning checks running_vm_count on both Get and List.
func (e spotEnv) wantRunning(t *testing.T, id string, want int64) {
	t.Helper()
	if got := e.getPolicy(t, id).GetStatus().GetRunningVmCount(); got != want {
		t.Errorf("policy %s running_vm_count = %d, want %d", id, got, want)
	}
	list, err := e.policies.List(t.Context(), &billingpb.ListPricingPoliciesRequest{ParentId: e.project})
	if err != nil {
		t.Fatalf("list pricing policies: %v", err)
	}
	for _, p := range list.GetItems() {
		if p.GetMetadata().GetId() == id && p.GetStatus().GetRunningVmCount() != want {
			t.Errorf("list: policy %s running_vm_count = %d, want %d", id, p.GetStatus().GetRunningVmCount(), want)
		}
	}
}

// spotSpec is a preemptible H100 instance priced by policyID.
func (e spotEnv) spotSpec(name, policyID string) *computepb.InstanceSpec {
	s := e.gpuSpec(name)
	s.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
	s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
	s.PricingModel = &computepb.InstanceSpec_SpotPricingPolicy{SpotPricingPolicy: &computepb.SpotPricingPolicySpec{Id: policyID}}
	return s
}

func (e spotEnv) updatePolicy(t *testing.T, id, name, maxPrice string) error {
	t.Helper()
	p := e.getPolicy(t, id)
	md, spec := p.GetMetadata(), p.GetSpec()
	md.Name = name
	spec.Pricing = &billingpb.PricingMethod{Versions: &billingpb.PricingMethod_MaxPriceV1{MaxPriceV1: &billingpb.MaxPriceV1{MaxPrice: maxPrice}}}
	op, err := e.policies.Update(t.Context(), &billingpb.UpdatePricingPolicyRequest{Metadata: md, Spec: spec})
	if err != nil {
		return err
	}
	_, err = op.Wait(t.Context())
	return err
}

func (e spotEnv) deletePolicy(t *testing.T, id string) error {
	t.Helper()
	op, err := e.policies.Delete(t.Context(), &billingpb.DeletePricingPolicyRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(t.Context())
	return err
}

func (e spotEnv) usePolicy(id string) func(*computepb.InstanceSpec) {
	return func(s *computepb.InstanceSpec) {
		s.PricingModel = &computepb.InstanceSpec_SpotPricingPolicy{SpotPricingPolicy: &computepb.SpotPricingPolicySpec{Id: id}}
	}
}

func TestInstanceSpotRunningCountPinsPolicy(t *testing.T) {
	t.Parallel()
	e := startSpot(t)
	pid := e.policy(t, "ok", "5.000")

	inst := e.mustCreate(t, "a", e.spotSpec("a", pid))
	id := inst.GetMetadata().GetId()
	if got := inst.GetSpec().GetSpotPricingPolicy().GetId(); got != pid {
		t.Errorf("spot_pricing_policy.id = %q, want %q", got, pid)
	}
	e.wantRunning(t, pid, 1)

	wantSpotCode(t, "max_price change while running", e.updatePolicy(t, pid, "ok", "6.000"), codes.FailedPrecondition, "")
	wantSpotCode(t, "delete while running", e.deletePolicy(t, pid), codes.FailedPrecondition, "")
	if err := e.updatePolicy(t, pid, "renamed", "5.000"); err != nil {
		t.Errorf("rename while running: %v", err)
	}

	if err := e.stop(t.Context(), id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	e.wantRunning(t, pid, 0)
	if err := e.updatePolicy(t, pid, "renamed", "6.000"); err != nil {
		t.Errorf("max_price change while stopped: %v", err)
	}
	if err := e.deletePolicy(t, pid); err != nil {
		t.Errorf("delete while stopped: %v", err)
	}
}

func TestInstanceSpotPricingModelRules(t *testing.T) {
	t.Parallel()
	e := startSpot(t)
	pid := e.policy(t, "ok", "5.000")

	tests := []struct {
		name   string
		change func(*computepb.InstanceSpec)
		code   codes.Code
		field  string
	}{
		{"policy on a non-preemptible instance", func(s *computepb.InstanceSpec) {
			s.Preemptible, s.RecoveryPolicy = nil, computepb.InstanceRecoveryPolicy_RECOVER
		}, codes.InvalidArgument, "spec.spot_pricing_policy"},
		{"follows_spot_price on a non-preemptible instance", func(s *computepb.InstanceSpec) {
			s.Preemptible, s.RecoveryPolicy = nil, computepb.InstanceRecoveryPolicy_RECOVER
			s.PricingModel = &computepb.InstanceSpec_FollowsSpotPrice{FollowsSpotPrice: &computepb.FollowsSpotPriceSpec{}}
		}, codes.InvalidArgument, "spec.follows_spot_price"},
		{"on_demand on a preemptible instance", func(s *computepb.InstanceSpec) {
			s.PricingModel = &computepb.InstanceSpec_OnDemand{OnDemand: &computepb.OnDemandSpec{}}
		}, codes.InvalidArgument, "spec.on_demand"},
		{"unknown policy", e.usePolicy("pricingpolicy-e00doesnotexist"), codes.NotFound, "pricingpolicy-e00doesnotexist"},
	}
	for _, tc := range tests {
		spec := e.spotSpec("v", pid)
		tc.change(spec)
		_, err := e.create(t.Context(), "v", spec)
		wantSpotCode(t, tc.name, err, tc.code, tc.field)
	}
}

func TestInstanceSpotPolicyOfOtherProjectOrPlatform(t *testing.T) {
	t.Parallel()
	e := startSpot(t)

	projects := iamv2svc.NewProjectService(e.env.Inst.SDK(t))
	op, err := projects.Create(t.Context(), &iamv2pb.CreateProjectRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.env.Inst.TenantID, Name: "other"},
		Spec:     &iamv2pb.ProjectSpec{Region: "eu-north1"},
	})
	other := mustOp(t, op, err)

	foreign := e.createPolicyIn(t, other, "foreign", "gpu-h100-sxm", "5.000")
	_, err = e.create(t.Context(), "x", e.spotSpec("x", foreign))
	wantSpotCode(t, "policy of another project", err, codes.InvalidArgument, "spec.spot_pricing_policy.id")

	h200 := e.createPolicyIn(t, e.project, "h200", "gpu-h200-sxm", "5.000")
	_, err = e.create(t.Context(), "y", e.spotSpec("y", h200))
	wantSpotCode(t, "policy of another platform", err, codes.InvalidArgument, "spec.spot_pricing_policy.id")

	// The same checks apply when an update changes the policy.
	inst := e.mustCreate(t, "a", e.spotSpec("a", e.policy(t, "ok", "5.000")))
	err = e.update(t.Context(), t, inst, e.usePolicy(h200))
	wantSpotCode(t, "update to a policy of another platform", err, codes.InvalidArgument, "spec.spot_pricing_policy.id")
}

func TestInstanceSpotBlockedPolicy(t *testing.T) {
	t.Parallel()
	e := startSpot(t)
	pid := e.policy(t, "blocked", "1.000") // below the 1.250 spot price

	// The create fails, but the VM exists, STOPPED.
	op, err := e.instances.Create(t.Context(), e.createReq("a", e.spotSpec("a", pid)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = op.Wait(t.Context())
	wantSpotCode(t, "create under a blocked policy", err, codes.FailedPrecondition, "")
	a := e.get(t, op.ResourceID())
	if got := a.GetStatus().GetState(); got != computepb.InstanceStatus_STOPPED {
		t.Fatalf("state after the failed create = %v, want STOPPED", got)
	}
	e.wantRunning(t, pid, 0)

	// With spec.stopped the create succeeds.
	spec := e.spotSpec("b", pid)
	spec.Stopped = true
	b := e.mustCreate(t, "b", spec)
	if got := b.GetStatus().GetState(); got != computepb.InstanceStatus_STOPPED {
		t.Errorf("stopped create: state = %v, want STOPPED", got)
	}

	wantSpotCode(t, "start under a blocked policy", e.start(t.Context(), a.GetMetadata().GetId()), codes.FailedPrecondition, "")
	if got := e.get(t, a.GetMetadata().GetId()).GetStatus().GetState(); got != computepb.InstanceStatus_STOPPED {
		t.Errorf("state after the failed start = %v, want STOPPED", got)
	}

	// Raising max_price to the spot price lifts the block (nothing runs).
	if err := e.updatePolicy(t, pid, "blocked", "1.250"); err != nil {
		t.Fatalf("raise max_price: %v", err)
	}
	if err := e.start(t.Context(), a.GetMetadata().GetId()); err != nil {
		t.Fatalf("start after raising max_price: %v", err)
	}
	e.wantRunning(t, pid, 1)
}

func TestInstanceSpotPolicyDeletedWhileStopped(t *testing.T) {
	t.Parallel()
	e := startSpot(t)
	gone := e.policy(t, "gone", "5.000")
	next := e.policy(t, "next", "5.000")

	inst := e.mustCreate(t, "a", e.spotSpec("a", gone))
	id := inst.GetMetadata().GetId()
	if err := e.stop(t.Context(), id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := e.deletePolicy(t, gone); err != nil {
		t.Fatalf("delete policy: %v", err)
	}

	wantSpotCode(t, "start with a deleted policy", e.start(t.Context(), id), codes.NotFound, "")
	inst = e.mustUpdate(t, e.get(t, id), e.usePolicy(next))
	if got := inst.GetSpec().GetSpotPricingPolicy().GetId(); got != next {
		t.Fatalf("policy after update = %q, want %q", got, next)
	}
	if err := e.start(t.Context(), id); err != nil {
		t.Fatalf("start with the new policy: %v", err)
	}
	e.wantRunning(t, next, 1)
}

func TestInstanceSpotUpdateRunningToOtherPolicy(t *testing.T) {
	t.Parallel()
	e := startSpot(t)
	old := e.policy(t, "old", "5.000")
	blocked := e.policy(t, "blocked", "1.000")
	allowed := e.policy(t, "allowed", "5.000")

	inst := e.mustCreate(t, "a", e.spotSpec("a", old))
	e.wantRunning(t, old, 1)

	wantSpotCode(t, "update to a blocked policy", e.update(t.Context(), t, inst, e.usePolicy(blocked)), codes.FailedPrecondition, "")
	e.wantRunning(t, old, 1)
	e.wantRunning(t, blocked, 0)

	e.mustUpdate(t, inst, e.usePolicy(allowed))
	e.wantRunning(t, old, 0)
	e.wantRunning(t, allowed, 1)
}

// wantSpotCode is wantCode for a failed call or a failed operation; field is
// checked against the message.
func wantSpotCode(t *testing.T, what string, err error, code codes.Code, field string) {
	t.Helper()
	if got := kmsCode(err); got != code {
		t.Errorf("%s: code = %v, want %v (err: %v)", what, got, code, err)
		return
	}
	if field != "" && !strings.Contains(err.Error(), field) {
		t.Errorf("%s: error does not name %q: %v", what, field, err)
	}
}
