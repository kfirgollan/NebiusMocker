package domain

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Dynamic public addresses (public_ip_address without allocation_id,
// static=false) follow docs.nebius.com/compute/virtual-machines/network: "If
// a VM with a dynamic address has the Stopped status for more than one hour,
// the address is automatically returned to the IPv4 public range." Until
// then the stopped instance keeps the address (its allocation stays
// ASSIGNED), and a Start reuses it. After that, a Start assigns a new one.
//
// The stop time is recorded when an instance holding such an address settles
// in STOPPED (InstanceTx.SetStoppedAt) and cleared when it runs again or is
// deleted. SweepPublicIPs (run periodically by the module on the injected
// clock) releases the addresses that are due, and Start / Update release
// them first if the sweep has not run yet, so the outcome never depends on
// the sweep's timing.

// withNICs runs fn holding the lock that serializes NIC changes of stopped
// instances with the release sweep (the operations engine serializes the
// operations of one instance, but the sweep is not an operation).
func (s *InstanceService) withNICs(fn func() error) error {
	s.nics.Lock()
	defer s.nics.Unlock()
	return fn()
}

// SweepPublicIPs releases the dynamic public addresses of instances that
// have been STOPPED for longer than InstanceDeps.PublicIPReleaseAfter. It
// returns how many instances lost their addresses.
func (s *InstanceService) SweepPublicIPs(ctx context.Context) (int, error) {
	ids, err := s.d.Instances.StoppedBefore(ctx, s.d.Clock.Now().Add(-s.d.PublicIPReleaseAfter))
	if err != nil {
		return 0, err
	}
	n := 0
	var errList []error
	for _, id := range ids {
		var released bool
		err := s.withNICs(func() error {
			var err error
			released, err = s.releaseExpiredPublic(ctx, id)
			return err
		})
		switch {
		case err != nil && !errs.IsKind(err, errs.KindNotFound):
			errList = append(errList, fmt.Errorf("release public addresses of %s: %w", id, err))
		case released:
			n++
		}
	}
	return n, errors.Join(errList...)
}

// releaseExpiredPublic releases the dynamic public addresses of instance id
// when it has been STOPPED for longer than the grace period, and reports
// whether it did. The caller holds the NIC lock (withNICs).
func (s *InstanceService) releaseExpiredPublic(ctx context.Context, id string) (bool, error) {
	var cur *computepb.Instance
	due := false
	err := s.d.Instances.Txn(ctx, func(tx InstanceTx) error {
		var err error
		if cur, err = tx.Instance(id); err != nil {
			return err
		}
		at, ok, err := tx.StoppedAt(id)
		if err != nil || !ok {
			return err
		}
		due = cur.GetStatus().GetState() == computepb.InstanceStatus_STOPPED &&
			s.d.Clock.Now().Sub(at) > s.d.PublicIPReleaseAfter
		return nil
	})
	if err != nil || !due {
		return false, err
	}
	// Bound as STOPPED without the dynamic addresses in status, every NIC
	// gives up its dynamic public address (see binding).
	bare := proto.CloneOf(cur)
	for _, st := range bare.GetStatus().GetNetworkInterfaces() {
		if isDynamicPublic(nicByName(cur.GetSpec(), st.GetName())) {
			st.PublicIpAddress = nil
		}
	}
	nics, err := s.bindAll(ctx, bare, bare.GetSpec(), computepb.InstanceStatus_STOPPED, nil)
	if err != nil {
		return false, err
	}
	err = s.mutate(ctx, id, func(tx InstanceTx, upd *computepb.Instance) error {
		upd.Status.NetworkInterfaces = nics
		return tx.ClearStoppedAt(id)
	})
	return err == nil, err
}

// recordStop keeps the stop time of an instance that settles in state to:
// it starts when an instance holding a dynamic public address becomes
// STOPPED (an update of a stopped instance keeps the original time), and is
// cleared otherwise.
func (s *InstanceService) recordStop(tx InstanceTx, inst *computepb.Instance, to computepb.InstanceStatus_InstanceState) error {
	id := inst.GetMetadata().GetId()
	if to != computepb.InstanceStatus_STOPPED || !holdsDynamicPublic(inst) {
		return tx.ClearStoppedAt(id)
	}
	if _, ok, err := tx.StoppedAt(id); err != nil || ok {
		return err
	}
	return tx.SetStoppedAt(id, s.d.Clock.Now())
}

// isDynamicPublic reports whether nic asks for a dynamic public address.
func isDynamicPublic(nic *computepb.NetworkInterfaceSpec) bool {
	pub := nic.GetPublicIpAddress()
	return pub != nil && pub.GetAllocationId() == "" && !pub.GetStatic()
}

// holdsDynamicPublic reports whether any NIC of inst has a dynamic public
// address in its status.
func holdsDynamicPublic(inst *computepb.Instance) bool {
	for _, nic := range inst.GetSpec().GetNetworkInterfaces() {
		if isDynamicPublic(nic) && hasPublicAddress(inst, nic.GetName()) {
			return true
		}
	}
	return false
}

// hasPublicAddress reports whether NIC name of inst has a public address in
// the instance's status.
func hasPublicAddress(inst *computepb.Instance, name string) bool {
	for _, st := range inst.GetStatus().GetNetworkInterfaces() {
		if st.GetName() == name {
			return st.GetPublicIpAddress().GetAddress() != ""
		}
	}
	return false
}
