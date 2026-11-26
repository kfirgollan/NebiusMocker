package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Capacity reservations (docs: compute/virtual-machines/reservations).
//
// spec.reservation_policy.reservation_ids are capacity block group IDs, tried
// in order. AUTO (the default) draws from a reservation when one fits and
// otherwise runs in the common pool; FORBID uses only the pool (reservation
// IDs are a validation error); STRICT runs only in a reservation and fails
// the request when none fits. Only regular (non-preemptible) GPU instances
// use reservations.
//
// An instance draws when it is created and when it starts without holding a
// reservation; the reserved GPUs stay held while it is stopped and are
// released when it is deleted. Changing reservation_policy or the preset
// (both only while STOPPED) drops the reservation; the next start draws
// again. status.reservation_id is the block group the instance draws from
// (empty: the common pool).

// Reservations finds the capacity reservations capability at call time (the
// capacity module may be disabled).
type Reservations interface {
	CapacityReservations() (peer.CapacityReservations, bool)
}

const (
	reservationPolicyField = "spec.reservation_policy.policy"
	reservationIDsField    = "spec.reservation_policy.reservation_ids"
)

// checkReservationPolicy validates reservation_policy against the preset:
// reservations apply only to regular GPU instances.
func checkReservationPolicy(spec *computepb.InstanceSpec, pp PlatformPreset) error {
	rp := spec.GetReservationPolicy()
	if rp.GetPolicy() == computepb.ReservationPolicy_FORBID || reservationEligible(spec, pp) {
		return nil
	}
	why := "only GPU instances can use capacity reservations"
	if spec.GetPreemptible() != nil {
		why = "preemptible instances cannot use capacity reservations"
	}
	switch {
	case rp.GetPolicy() == computepb.ReservationPolicy_STRICT:
		return errs.BadRequest(errs.Violation{Field: reservationPolicyField, Message: why})
	case len(rp.GetReservationIds()) > 0:
		return errs.BadRequest(errs.Violation{Field: reservationIDsField, Message: why})
	}
	return nil
}

// presetGPUs is the number of GPUs of the preset (the reservation quantity).
func presetGPUs(pp PlatformPreset) uint64 {
	n := pp.Preset.GetResources().GetGpuCount()
	if n <= 0 {
		return 0
	}
	return uint64(n)
}

func reservationEligible(spec *computepb.InstanceSpec, pp PlatformPreset) bool {
	return spec.GetPreemptible() == nil && pp.Preset.GetResources().GetGpuCount() > 0
}

// drawReservation decides where inst runs: it reserves GPUs of a capacity
// block group for it and returns the group's ID, or "" for the common pool
// (FORBID, not eligible, or AUTO when no reservation fits). STRICT fails with
// the capacity error. A reservation the instance already holds is replaced
// when another one is drawn; when "" is returned it is left as it was, for
// the caller to release once its change is committed.
func (s *InstanceService) drawReservation(ctx context.Context, inst *computepb.Instance) (string, error) {
	spec := inst.GetSpec()
	rp := spec.GetReservationPolicy()
	if rp.GetPolicy() == computepb.ReservationPolicy_FORBID {
		return "", nil
	}
	projectID := inst.GetMetadata().GetParentId()
	pp, err := s.d.Platforms.Resolve(ctx, projectID, spec.GetResources().GetPlatform(), spec.GetResources().GetPreset())
	if err != nil {
		return "", err
	}
	if !reservationEligible(spec, pp) {
		return "", nil
	}
	strict := rp.GetPolicy() == computepb.ReservationPolicy_STRICT
	caps, ok := s.d.Reservations.CapacityReservations()
	if !ok {
		if strict {
			return "", errs.BadState(inst.GetMetadata().GetId(),
				"reservation_policy STRICT: no capacity reservation can be found (the capacity service is not enabled)")
		}
		return "", nil
	}
	var fabric string
	if clusterID := spec.GetGpuCluster().GetId(); clusterID != "" {
		if fabric, err = s.d.GpuClusters.Fabric(ctx, clusterID); err != nil {
			return "", fmt.Errorf("spec.gpu_cluster.id: %w", err)
		}
	}
	groupID, err := caps.Reserve(ctx, peer.CapacityRequest{
		ConsumerID: inst.GetMetadata().GetId(), ProjectID: projectID,
		Region: pp.Region, Platform: spec.GetResources().GetPlatform(), Fabric: fabric,
		Quantity:       presetGPUs(pp),
		ReservationIDs: rp.GetReservationIds(),
	})
	switch {
	case err == nil:
		return groupID, nil
	case !strict && errs.IsKind(err, errs.KindQuotaExceeded):
		return "", nil // AUTO: no reservation fits, run in the common pool
	default:
		return "", specField(err)
	}
}

// releaseReservation frees the instance's reservation, if any.
func (s *InstanceService) releaseReservation(ctx context.Context, instanceID string) error {
	caps, ok := s.d.Reservations.CapacityReservations()
	if !ok {
		return nil
	}
	return caps.Release(ctx, instanceID)
}

// restoreReservation undoes a drawReservation whose change was not
// committed: back to the reservation the instance held before (heldID), or
// released when it held none.
func (s *InstanceService) restoreReservation(ctx context.Context, inst *computepb.Instance, heldID, drawnID string) error {
	if drawnID == "" || drawnID == heldID {
		return nil
	}
	caps, ok := s.d.Reservations.CapacityReservations()
	if !ok {
		return nil
	}
	if heldID == "" {
		return caps.Release(ctx, inst.GetMetadata().GetId())
	}
	prev := &computepb.Instance{Metadata: inst.GetMetadata(), Spec: inst.GetSpec()}
	pp, err := s.d.Platforms.Resolve(ctx, prev.GetMetadata().GetParentId(), prev.GetSpec().GetResources().GetPlatform(), prev.GetSpec().GetResources().GetPreset())
	if err != nil {
		return err
	}
	_, err = caps.Reserve(ctx, peer.CapacityRequest{
		ConsumerID: inst.GetMetadata().GetId(), ProjectID: inst.GetMetadata().GetParentId(),
		Region: pp.Region, Platform: prev.GetSpec().GetResources().GetPlatform(),
		Quantity: presetGPUs(pp), ReservationIDs: []string{heldID},
	})
	return err
}

// reservationOnStart draws a reservation for a stopped instance that is
// starting, unless it still holds one (reservations stay held while an
// instance is stopped). It returns the ID to record in status.
func (s *InstanceService) reservationOnStart(ctx context.Context, inst *computepb.Instance) (string, error) {
	if held := inst.GetStatus().GetReservationId(); held != "" {
		return held, nil
	}
	return s.drawReservation(ctx, inst)
}

// reservationOnUpdate returns the reservation the updated instance holds and
// a cleanup to run once the update is committed (release a reservation it no
// longer uses), or an undo to run when it is not. Changing
// reservation_policy or the preset drops the held reservation; an update
// that starts a STOPPED instance (target RUNNING) draws one.
func (s *InstanceService) reservationOnUpdate(ctx context.Context, stored, updated *computepb.Instance, target computepb.InstanceStatus_InstanceState) (id string, commit, undo func() error, err error) {
	held := stored.GetStatus().GetReservationId()
	prev, next := stored.GetSpec(), updated.GetSpec()
	id = held
	if !reservationPolicyEqual(prev.GetReservationPolicy(), next.GetReservationPolicy()) ||
		prev.GetResources().GetPreset() != next.GetResources().GetPreset() {
		id = ""
	}
	starting := stored.GetStatus().GetState() == computepb.InstanceStatus_STOPPED && target == computepb.InstanceStatus_RUNNING
	if starting && id == "" {
		if id, err = s.drawReservation(ctx, updated); err != nil {
			return "", nil, nil, err
		}
	}
	instanceID := stored.GetMetadata().GetId()
	commit = func() error {
		if held != "" && id == "" {
			return s.releaseReservation(ctx, instanceID)
		}
		return nil
	}
	undo = func() error { return s.restoreReservation(ctx, stored, held, id) }
	return id, commit, undo, nil
}

// checkReservationUpdate allows changing reservation_policy only while the
// instance is STOPPED (docs: "To update a VM, stop it first").
func checkReservationUpdate(id string, state computepb.InstanceStatus_InstanceState, prev, next *computepb.InstanceSpec) error {
	if state != computepb.InstanceStatus_STOPPED && !reservationPolicyEqual(prev.GetReservationPolicy(), next.GetReservationPolicy()) {
		return errs.BadState(id, "reservation_policy can be changed only while the instance is STOPPED: stop it first")
	}
	return nil
}

func reservationPolicyEqual(a, b *computepb.ReservationPolicy) bool {
	return a.GetPolicy() == b.GetPolicy() && strings.Join(a.GetReservationIds(), ",") == strings.Join(b.GetReservationIds(), ",")
}

// specField prefixes the capacity service's field names (relative to the
// instance spec, e.g. "reservation_policy.reservation_ids[0]") with "spec.".
func specField(err error) error {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || len(e.Violations) == 0 {
		return err
	}
	c := *e
	c.Violations = make([]errs.Violation, len(e.Violations))
	for i, v := range e.Violations {
		if strings.HasPrefix(v.Field, "reservation_policy.") {
			v.Field = "spec." + v.Field
		}
		c.Violations[i] = v
	}
	return &c
}
