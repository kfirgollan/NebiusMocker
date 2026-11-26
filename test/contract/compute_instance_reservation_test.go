package contract

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/capacity"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

const reservationMissingGroup = "capacityblockgroup-e00doesnotexist"

// reservationEnv is a mocker with IAM, Compute, VPC and capacity, enforcing
// authz, with the admin's SDK clients.
type reservationEnv struct {
	instClients
	*capacityEnv
}

func startReservations(t *testing.T) reservationEnv {
	t.Helper()
	env := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{vpc.New(), capacity.New()},
		testkit.WithAuthz(authz.ModeEnforce))
	return reservationEnv{
		instClients: instClientsFor(t, env, ""),
		capacityEnv: &capacityEnv{inst: env.Inst, capacityClients: newCapacityClients(env.Inst.SDK(t))},
	}
}

// reservedSpec is a GPU instance spec with the reservation policy.
func (e reservationEnv) reservedSpec(name string, policy computepb.ReservationPolicy_Policy, ids ...string) *computepb.InstanceSpec {
	spec := e.gpuSpec(name)
	spec.ReservationPolicy = &computepb.ReservationPolicy{Policy: policy, ReservationIds: ids}
	return spec
}

// updateWithMask resends the instance with change applied and the reset mask.
func (e reservationEnv) updateWithMask(ctx context.Context, t *testing.T, i *computepb.Instance, mask string, change func(*computepb.InstanceSpec)) error {
	t.Helper()
	next := proto.Clone(i).(*computepb.Instance)
	change(next.GetSpec())
	op, err := e.instances.Update(resetCtx(ctx, t, mask), &computepb.UpdateInstanceRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// usage returns the GPUs held in the group and the instances holding them.
func (e reservationEnv) usage(t *testing.T, groupID string) (gpus uint64, holders []string) {
	t.Helper()
	g, err := e.capacityEnv.groups.Get(t.Context(), &capacitypb.GetCapacityBlockGroupRequest{Id: groupID})
	if err != nil {
		t.Fatalf("get block group %s: %v", groupID, err)
	}
	res, err := e.capacityEnv.groups.ListResources(t.Context(), &capacitypb.ListCapacityBlockGroupResourcesRequest{Id: groupID})
	if err != nil {
		t.Fatalf("list resources of %s: %v", groupID, err)
	}
	return g.GetStatus().GetUsage(), res.GetResourceIds()
}

func (e reservationEnv) wantUsage(t *testing.T, groupID string, gpus uint64, holders ...*computepb.Instance) {
	t.Helper()
	got, ids := e.usage(t, groupID)
	var want []string
	for _, h := range holders {
		want = append(want, h.GetMetadata().GetId())
	}
	slices.Sort(ids)
	slices.Sort(want)
	if got != gpus || !slices.Equal(ids, want) {
		t.Errorf("block group %s: usage = %d held by %v, want %d held by %v", groupID, got, ids, gpus, want)
	}
}

func (e reservationEnv) h100(t *testing.T) string {
	t.Helper()
	return e.groupByPlatform(t, capH100Platform).GetMetadata().GetId()
}

func TestInstanceReservationAutoDraws(t *testing.T) {
	t.Parallel()
	e := startReservations(t)
	h100 := e.h100(t)

	a := e.mustCreate(t, "a", e.gpuSpec("a")) // default policy is AUTO
	if got := a.GetStatus().GetReservationId(); got != h100 {
		t.Errorf("reservation_id = %q, want %q", got, h100)
	}
	e.wantUsage(t, h100, 8, a)

	// A second VM fills the 16-GPU group; the third falls back to the pool.
	b := e.mustCreate(t, "b", e.reservedSpec("b", computepb.ReservationPolicy_AUTO))
	e.wantUsage(t, h100, 16, a, b)
	c := e.mustCreate(t, "c", e.gpuSpec("c"))
	if got := c.GetStatus().GetReservationId(); got != "" {
		t.Errorf("fallback reservation_id = %q, want empty", got)
	}
	e.wantUsage(t, h100, 16, a, b)
}

func TestInstanceReservationStrictExhausted(t *testing.T) {
	t.Parallel()
	e := startReservations(t)
	h100 := e.h100(t)
	a := e.mustCreate(t, "a", e.gpuSpec("a"))
	b := e.mustCreate(t, "b", e.reservedSpec("b", computepb.ReservationPolicy_STRICT))
	if got := b.GetStatus().GetReservationId(); got != h100 {
		t.Errorf("STRICT reservation_id = %q, want %q", got, h100)
	}

	_, err := e.create(t.Context(), "c", e.reservedSpec("c", computepb.ReservationPolicy_STRICT))
	wantCode(t, "STRICT with a full group", err, codes.ResourceExhausted, "")
	e.wantUsage(t, h100, 16, a, b)

	list, err := e.instances.List(t.Context(), &computepb.ListInstancesRequest{ParentId: e.project})
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if n := len(list.GetItems()); n != 2 {
		t.Errorf("instances after failed STRICT create = %d, want 2", n)
	}
}

func TestInstanceReservationForbid(t *testing.T) {
	t.Parallel()
	e := startReservations(t)
	h100 := e.h100(t)

	_, err := e.create(t.Context(), "bad", e.reservedSpec("bad", computepb.ReservationPolicy_FORBID, h100))
	wantCode(t, "FORBID with reservation_ids", err, codes.InvalidArgument, "reservation_ids")

	i := e.mustCreate(t, "pool", e.reservedSpec("pool", computepb.ReservationPolicy_FORBID))
	if got := i.GetStatus().GetReservationId(); got != "" {
		t.Errorf("FORBID reservation_id = %q, want empty", got)
	}
	e.wantUsage(t, h100, 0)
}

func TestInstanceReservationExplicitIDs(t *testing.T) {
	t.Parallel()
	e := startReservations(t)
	h100 := e.h100(t)
	h200 := e.groupByPlatform(t, capH200Platform).GetMetadata().GetId()

	i := e.mustCreate(t, "ok", e.reservedSpec("ok", computepb.ReservationPolicy_AUTO, h100))
	if got := i.GetStatus().GetReservationId(); got != h100 {
		t.Errorf("reservation_id = %q, want %q", got, h100)
	}

	const field = "spec.reservation_policy.reservation_ids[0]"
	for _, policy := range []computepb.ReservationPolicy_Policy{computepb.ReservationPolicy_AUTO, computepb.ReservationPolicy_STRICT} {
		_, err := e.create(t.Context(), "missing", e.reservedSpec("missing", policy, reservationMissingGroup))
		wantCode(t, policy.String()+" unknown id", err, codes.NotFound, "")
		_, err = e.create(t.Context(), "mismatch", e.reservedSpec("mismatch", policy, h200))
		wantCode(t, policy.String()+" h200 group for an h100 VM", err, codes.InvalidArgument, field)
	}
	e.wantUsage(t, h100, 8, i)
}

func TestInstanceReservationLifecycle(t *testing.T) {
	t.Parallel()
	e := startReservations(t)
	ctx := t.Context()
	h100 := e.h100(t)

	i := e.mustCreate(t, "life", e.gpuSpec("life"))
	id := i.GetMetadata().GetId()
	e.wantUsage(t, h100, 8, i)

	if err := e.stop(ctx, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopped := e.get(t, id)
	if got := stopped.GetStatus().GetReservationId(); got != h100 {
		t.Errorf("stopped reservation_id = %q, want %q", got, h100)
	}
	e.wantUsage(t, h100, 8, stopped)

	if err := e.start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	started := e.get(t, id)
	if got := started.GetStatus().GetReservationId(); got != h100 {
		t.Errorf("restarted reservation_id = %q, want %q", got, h100)
	}
	e.wantUsage(t, h100, 8, started)

	e.mustDelete(t, id)
	e.wantUsage(t, h100, 0)
}

func TestInstanceReservationProjectAllowance(t *testing.T) {
	t.Parallel()
	e := startReservations(t)
	h100 := e.h100(t)
	e.createAllowance(t, h100, capLimit(8))

	a := e.mustCreate(t, "a", e.reservedSpec("a", computepb.ReservationPolicy_STRICT))
	if got := a.GetStatus().GetReservationId(); got != h100 {
		t.Errorf("reservation_id = %q, want %q", got, h100)
	}

	_, err := e.create(t.Context(), "strict", e.reservedSpec("strict", computepb.ReservationPolicy_STRICT))
	wantCode(t, "STRICT over the project allowance", err, codes.ResourceExhausted, "")

	b := e.mustCreate(t, "auto", e.reservedSpec("auto", computepb.ReservationPolicy_AUTO))
	if got := b.GetStatus().GetReservationId(); got != "" {
		t.Errorf("AUTO over the allowance reservation_id = %q, want empty", got)
	}
	e.wantUsage(t, h100, 8, a)
}

func TestInstanceReservationPolicyChange(t *testing.T) {
	t.Parallel()
	e := startReservations(t)
	ctx := t.Context()
	h100 := e.h100(t)

	i := e.mustCreate(t, "chg", e.gpuSpec("chg"))
	id := i.GetMetadata().GetId()
	forbid := func(s *computepb.InstanceSpec) {
		s.ReservationPolicy = &computepb.ReservationPolicy{Policy: computepb.ReservationPolicy_FORBID}
	}
	const mask = instResetMask + ",spec.reservation_policy"

	err := e.updateWithMask(ctx, t, i, mask, forbid)
	wantCode(t, "change policy while RUNNING", err, codes.FailedPrecondition, "")
	e.wantUsage(t, h100, 8, i)

	if err := e.stop(ctx, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopped := e.get(t, id)
	if err := e.updateWithMask(ctx, t, stopped, mask, forbid); err != nil {
		t.Fatalf("change policy while STOPPED: %v", err)
	}
	updated := e.get(t, id)
	if got := updated.GetSpec().GetReservationPolicy().GetPolicy(); got != computepb.ReservationPolicy_FORBID {
		t.Errorf("policy = %v, want FORBID", got)
	}
	if got := updated.GetStatus().GetReservationId(); got != "" {
		t.Errorf("reservation_id after policy change = %q, want empty", got)
	}
	e.wantUsage(t, h100, 0)

	// The next start follows the new policy: still the pool.
	if err := e.start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	e.wantUsage(t, h100, 0)
}

func TestInstanceReservationRejectsCPUStrict(t *testing.T) {
	t.Parallel()
	e := startReservations(t)
	spec := e.cpuSpec("cpu")
	spec.ReservationPolicy = &computepb.ReservationPolicy{Policy: computepb.ReservationPolicy_STRICT}
	_, err := e.create(t.Context(), "cpu", spec)
	wantCode(t, "STRICT on a CPU instance", err, codes.InvalidArgument, "reservation_policy")
}
