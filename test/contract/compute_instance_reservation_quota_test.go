package contract

import (
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	quotassvc "github.com/nebius/gosdk/services/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/capacity"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// TestInstanceReservationGPUQuota: GPUs drawn from a capacity reservation do
// not count towards the GPU quota (docs compute/virtual-machines/reservations),
// while an AUTO VM that falls back to the common pool, and a FORBID one, do.
// Instances are still counted.
func TestInstanceReservationGPUQuota(t *testing.T) {
	t.Parallel()
	env := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{vpc.New(), capacity.New(), quotas.New()},
		testkit.WithAuthz(authz.ModeEnforce))
	e := reservationEnv{
		instClients: instClientsFor(t, env, ""),
		capacityEnv: &capacityEnv{inst: env.Inst, capacityClients: newCapacityClients(env.Inst.SDK(t))},
	}
	q := quotaEnv{inst: env.Inst, quotas: quotassvc.NewQuotaAllowanceService(env.Inst.SDK(t))}
	p := e.project
	gpuQuota := quota.ComputeInstanceGPU("gpu-h100-sxm")
	group := e.groupByPlatform(t, "gpu-h100-sxm").GetMetadata().GetId()
	usage := func(name string) uint64 { return q.byName(t, p, name).GetStatus().GetUsage() }
	gpus0, count0 := usage(gpuQuota), usage(quota.ComputeInstanceCount)

	// The group reserves 16 GPUs: two 8-GPU VMs draw from it, no GPU quota.
	a := e.mustCreate(t, "res-a", e.reservedSpec("res-a", computepb.ReservationPolicy_AUTO))
	b := e.mustCreate(t, "res-b", e.reservedSpec("res-b", computepb.ReservationPolicy_STRICT, group))
	for _, i := range []*computepb.Instance{a, b} {
		if got := e.get(t, i.GetMetadata().GetId()).GetStatus().GetReservationId(); got != group {
			t.Fatalf("%s reservation_id = %q, want %s", i.GetMetadata().GetName(), got, group)
		}
	}
	if got := usage(gpuQuota); got != gpus0 {
		t.Fatalf("%s usage with two reserved VMs = %d, want %d", gpuQuota, got, gpus0)
	}
	if got := usage(quota.ComputeInstanceCount); got != count0+2 {
		t.Fatalf("instance count = %d, want %d", got, count0+2)
	}

	// The group is full: an AUTO VM falls back to the pool and is charged.
	c := e.mustCreate(t, "pool-c", e.reservedSpec("pool-c", computepb.ReservationPolicy_AUTO))
	if got := e.get(t, c.GetMetadata().GetId()).GetStatus().GetReservationId(); got != "" {
		t.Fatalf("fallback VM reservation_id = %q, want the common pool", got)
	}
	if got := usage(gpuQuota); got != gpus0+8 {
		t.Fatalf("%s usage after a pool VM = %d, want %d", gpuQuota, got, gpus0+8)
	}

	// With no GPU quota left, a pool VM is refused, a reserved one is not.
	q.setLimit(t, p, gpuQuota, gpus0+8)
	_, err := e.create(t.Context(), "pool-d", e.reservedSpec("pool-d", computepb.ReservationPolicy_FORBID))
	if v := quotaFailure(t, err).Violations(); v[0].GetQuota() != gpuQuota {
		t.Fatalf("violations = %v, want %s", v, gpuQuota)
	}
	e.mustDelete(t, a.GetMetadata().GetId()) // frees 8 reserved GPUs
	e.mustCreate(t, "res-e", e.reservedSpec("res-e", computepb.ReservationPolicy_STRICT))
	if got := usage(gpuQuota); got != gpus0+8 {
		t.Fatalf("%s usage = %d, want %d", gpuQuota, got, gpus0+8)
	}
}
