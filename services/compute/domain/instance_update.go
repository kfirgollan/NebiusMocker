package domain

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// applyUpdate runs in the update operation's Prepare. It returns the state
// the instance settles in when the operation completes
// (InstanceStatus_UNSPECIFIED: no transition).
//
// Rules (docs: compute/virtual-machines/update, lifecycle):
//   - only RUNNING or STOPPED instances can be updated;
//   - immutable: service_account_id, resources.platform, gpu_cluster,
//     nvl_instance_group_id, recovery_policy, preemptible, and per interface (matched by name)
//     subnet_id and ip_address;
//   - the preset may change only while the instance is STOPPED and the
//     platform has allow_preset_change;
//   - toggling spec.stopped stops (STOPPING → STOPPED) or starts
//     (STARTING → RUNNING) the instance; other spec changes run as
//     UPDATING; a metadata-only change has no transition;
//   - disks and interfaces are reconciled (see reconcileDisks, bindAll).
func (s *InstanceService) applyUpdate(ctx context.Context, cmd UpdateInstanceCmd) (state computepb.InstanceStatus_InstanceState, err error) {
	err = s.withNICs(func() error {
		var err error
		state, err = s.applyUpdateLocked(ctx, cmd)
		return err
	})
	return state, err
}

// applyUpdateLocked is applyUpdate with the NIC lock held. Dynamic public
// addresses that are due are released first (see SweepPublicIPs).
func (s *InstanceService) applyUpdateLocked(ctx context.Context, cmd UpdateInstanceCmd) (computepb.InstanceStatus_InstanceState, error) {
	none := computepb.InstanceStatus_UNSPECIFIED
	if _, err := s.releaseExpiredPublic(ctx, cmd.Metadata.GetId()); err != nil {
		return none, err
	}
	stored, updated, err := s.patchInstance(ctx, cmd)
	if err != nil || updated == nil {
		return none, err
	}
	id, state := stored.GetMetadata().GetId(), stored.GetStatus().GetState()
	prev, next := stored.GetSpec(), updated.GetSpec()
	if err := s.checkUpdate(ctx, stored, next); err != nil {
		return none, err
	}
	via, target := transition(state, prev, next)
	after := state
	if target != none {
		after = target
	}
	if err := s.admitUpdateRun(ctx, prev, next, state, after); err != nil {
		return none, err
	}
	reservation, commitReservation, undoReservation, err := s.reservationOnUpdate(ctx, stored, updated, target)
	if err != nil {
		return none, err
	}
	updated.Status.ReservationId = reservation
	nics, err := s.bindAll(ctx, updated, next, after, prev)
	if err != nil {
		return none, errors.Join(err, undoReservation())
	}
	restore := func(err error, fs []string) error {
		_, restoreErr := s.bindAll(ctx, stored, prev, state, next)
		return errors.Join(err, restoreErr, s.detachFilesystems(ctx, id, fs), undoReservation())
	}
	addedFS, err := s.attachFilesystems(ctx, updated, prev, next)
	if err != nil {
		return none, restore(err, nil)
	}
	updated.Status.NetworkInterfaces = nics
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.d.Clock.Now())
	if via != none {
		setInstanceState(updated, via)
	}
	if err := s.commitUpdate(ctx, stored, updated); err != nil {
		return none, restore(err, addedFS)
	}
	if err := s.detachFilesystems(ctx, id, removedFilesystems(prev, next)); err != nil {
		return none, err
	}
	if err := commitReservation(); err != nil {
		return none, err
	}
	return target, nil
}

// patchInstance applies the update to a copy of the stored instance. It
// returns a nil updated instance when nothing changes.
func (s *InstanceService) patchInstance(ctx context.Context, cmd UpdateInstanceCmd) (stored, updated *computepb.Instance, err error) {
	stored, err = s.d.Instances.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return nil, nil, err
	}
	if err := checkMetadataUpdatable(stored.GetMetadata(), cmd.Metadata); err != nil {
		return nil, nil, err
	}
	updated, err = patchResource(stored, &computepb.Instance{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return nil, nil, err
	}
	if updated.GetSpec() == nil {
		updated.Spec = &computepb.InstanceSpec{}
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return stored, nil, nil // nothing changed: keep resource_version and updated_at
	}
	if updated.GetMetadata().GetName() == "" {
		return nil, nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	return stored, updated, nil
}

// checkUpdate validates the new spec against the stored instance.
func (s *InstanceService) checkUpdate(ctx context.Context, stored *computepb.Instance, next *computepb.InstanceSpec) error {
	id, state, prev := stored.GetMetadata().GetId(), stored.GetStatus().GetState(), stored.GetSpec()
	if !isResting(state) {
		return errs.BadState(id, fmt.Sprintf("instance is %s: only RUNNING or STOPPED instances can be updated", state))
	}
	if err := keepInstanceImmutable(prev, next); err != nil {
		return err
	}
	pp, err := s.checkSpec(ctx, stored.GetMetadata().GetParentId(), next)
	if err != nil {
		return err
	}
	if err := s.checkPricingPolicy(ctx, stored.GetMetadata().GetParentId(), prev, next); err != nil {
		return err
	}
	if next.GetResources().GetPreset() != prev.GetResources().GetPreset() {
		switch {
		case !pp.Platform.GetSpec().GetAllowPresetChange():
			return errs.BadRequest(errs.Violation{Field: "spec.resources.preset", Message: fmt.Sprintf(
				"platform %s does not allow changing the preset; recreate the instance", next.GetResources().GetPlatform())})
		case state != computepb.InstanceStatus_STOPPED:
			return errs.BadState(id, "the preset can be changed only while the instance is STOPPED: stop it first")
		}
	}
	if err := checkReservationUpdate(id, state, prev, next); err != nil {
		return err
	}
	if len(removedFilesystems(next, prev)) > 0 && state != computepb.InstanceStatus_STOPPED {
		return errs.BadState(id, "filesystems can be attached only while the instance is STOPPED: stop it first")
	}
	return nil
}

// admitUpdateRun checks the pricing policy of an instance that runs after
// the update, when the update starts it or changes its pricing policy.
func (s *InstanceService) admitUpdateRun(ctx context.Context, prev, next *computepb.InstanceSpec, state, after computepb.InstanceStatus_InstanceState) error {
	if after != computepb.InstanceStatus_RUNNING {
		return nil
	}
	if state == computepb.InstanceStatus_RUNNING && next.GetSpotPricingPolicy().GetId() == prev.GetSpotPricingPolicy().GetId() {
		return nil
	}
	return s.admitRun(ctx, next)
}

// transition returns the state an update passes through and the one it
// settles in (UNSPECIFIED, UNSPECIFIED when the spec does not change).
func transition(state computepb.InstanceStatus_InstanceState, prev, next *computepb.InstanceSpec) (via, target computepb.InstanceStatus_InstanceState) {
	switch {
	case proto.Equal(prev, next):
		return computepb.InstanceStatus_UNSPECIFIED, computepb.InstanceStatus_UNSPECIFIED
	case next.GetStopped() && !prev.GetStopped() && state == computepb.InstanceStatus_RUNNING:
		return computepb.InstanceStatus_STOPPING, computepb.InstanceStatus_STOPPED
	case !next.GetStopped() && prev.GetStopped() && state == computepb.InstanceStatus_STOPPED:
		return computepb.InstanceStatus_STARTING, computepb.InstanceStatus_RUNNING
	default:
		return computepb.InstanceStatus_UPDATING, state
	}
}

// commitUpdate reconciles the disks and stores the updated instance in one
// transaction.
func (s *InstanceService) commitUpdate(ctx context.Context, stored, updated *computepb.Instance) error {
	plan, err := s.planDisks(ctx, stored, stored.GetSpec(), updated.GetSpec())
	if err != nil {
		return err
	}
	return s.admitUpdate(ctx, stored, updated, plan, func(ctx context.Context) error {
		return s.d.Instances.Txn(ctx, func(tx InstanceTx) error {
			atts, err := s.applyDisks(tx, stored, plan)
			if err != nil {
				return err
			}
			updated.Status.DiskAttachments = atts
			return tx.PutInstance(updated)
		})
	})
}

// keepInstanceImmutable fails when an IMMUTABLE field of prev changed in next.
func keepInstanceImmutable(prev, next *computepb.InstanceSpec) error {
	var v []errs.Violation
	immutable := func(field string) {
		v = append(v, errs.Violation{Field: "spec." + field, Message: "field is immutable"})
	}
	if prev.GetServiceAccountId() != next.GetServiceAccountId() {
		immutable("service_account_id")
	}
	if prev.GetResources().GetPlatform() != next.GetResources().GetPlatform() {
		immutable("resources.platform")
	}
	if !proto.Equal(prev.GetGpuCluster(), next.GetGpuCluster()) {
		immutable("gpu_cluster")
	}
	if prev.GetNvlInstanceGroupId() != next.GetNvlInstanceGroupId() {
		immutable("nvl_instance_group_id")
	}
	if prev.GetRecoveryPolicy() != next.GetRecoveryPolicy() {
		immutable("recovery_policy")
	}
	if !proto.Equal(prev.GetPreemptible(), next.GetPreemptible()) {
		immutable("preemptible") // docs: cannot be added to or removed from an existing instance
	}
	for i, nic := range next.GetNetworkInterfaces() {
		old := nicByName(prev, nic.GetName())
		if old == nil {
			continue
		}
		field := fmt.Sprintf("network_interfaces[%d].", i)
		if old.GetSubnetId() != nic.GetSubnetId() {
			immutable(field + "subnet_id")
		}
		if !proto.Equal(old.GetIpAddress(), nic.GetIpAddress()) {
			immutable(field + "ip_address")
		}
	}
	if len(v) > 0 {
		return errs.BadRequest(v...)
	}
	return nil
}
