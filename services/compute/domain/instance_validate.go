package domain

import (
	"context"
	"fmt"
	"strings"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// maxNICs is the most network interfaces an instance has (instance.proto).
const maxNICs = 8

// notImplemented rejects a field whose feature the mocker does not emulate
// yet: never accepted and ignored (CLAUDE.md "No shortcuts").
func notImplemented(field, what string) error {
	return errs.BadRequest(errs.Violation{Field: field, Message: what + " is not implemented in NebiusMocker yet"})
}

// checkSpec validates an instance spec (create, or the result of an update)
// and resolves its platform and preset. Disk references are checked when
// they are attached (reconcileDisks), inside the operation's transaction.
func (s *InstanceService) checkSpec(ctx context.Context, projectID string, spec *computepb.InstanceSpec) (PlatformPreset, error) {
	res := spec.GetResources()
	switch {
	case res.GetPlatform() == "":
		return PlatformPreset{}, errs.BadRequest(errs.Violation{Field: "spec.resources.platform", Message: "required"})
	case res.GetPreset() == "":
		return PlatformPreset{}, errs.BadRequest(errs.Violation{Field: "spec.resources.preset", Message: "required"})
	}
	pp, err := s.d.Platforms.Resolve(ctx, projectID, res.GetPlatform(), res.GetPreset())
	if err != nil {
		return PlatformPreset{}, err
	}
	if err := checkUnsupported(spec); err != nil {
		return PlatformPreset{}, err
	}
	if err := checkRecoveryPolicy(spec); err != nil {
		return PlatformPreset{}, err
	}
	if err := checkScheduling(spec, pp); err != nil {
		return PlatformPreset{}, err
	}
	if err := checkGpuCluster(spec, pp); err != nil {
		return PlatformPreset{}, err
	}
	if err := checkFilesystems(spec); err != nil {
		return PlatformPreset{}, err
	}
	if err := s.checkServiceAccount(ctx, projectID, spec.GetServiceAccountId()); err != nil {
		return PlatformPreset{}, err
	}
	if spec.GetBootDisk() == nil {
		return PlatformPreset{}, errs.BadRequest(errs.Violation{Field: "spec.boot_disk", Message: "required"})
	}
	for _, sl := range diskSlots(spec) {
		if sl.spec.GetAttachMode() == computepb.AttachedDiskSpec_UNSPECIFIED {
			return PlatformPreset{}, errs.BadRequest(errs.Violation{Field: sl.field + ".attach_mode", Message: "required"})
		}
	}
	return pp, s.checkNICs(ctx, projectID, spec)
}

// checkUnsupported rejects, by name, the fields whose services the mocker
// does not have (see docs/services/compute.md, "Not yet").
func checkUnsupported(spec *computepb.InstanceSpec) error {
	switch {
	case spec.GetSpotPricingPolicy() != nil:
		return notImplemented("spec.spot_pricing_policy", "Spot pricing policies")
	case spec.GetLocalDisks().GetPassthroughGroup().GetRequested():
		return notImplemented("spec.local_disks.passthrough_group", "Local disk passthrough")
	}
	rp := spec.GetReservationPolicy()
	switch {
	case len(rp.GetReservationIds()) > 0 && rp.GetPolicy() == computepb.ReservationPolicy_FORBID:
		return errs.BadRequest(errs.Violation{Field: "spec.reservation_policy.reservation_ids", Message: "must be empty when policy is FORBID"})
	case len(rp.GetReservationIds()) > 0:
		return notImplemented("spec.reservation_policy.reservation_ids", "Capacity reservations (capacity blocks)")
	}
	return nil
}

// checkScheduling checks preemptible and reservation rules against the
// platform (docs: compute/virtual-machines/preemptible, reservations).
func checkScheduling(spec *computepb.InstanceSpec, pp PlatformPreset) error {
	rp := spec.GetReservationPolicy()
	if p := spec.GetPreemptible(); p != nil {
		switch {
		case p.GetOnPreemption() != computepb.PreemptibleSpec_STOP:
			return errs.BadRequest(errs.Violation{Field: "spec.preemptible.on_preemption", Message: "must be STOP"})
		case !pp.Platform.GetStatus().GetAllowedForPreemptibles():
			return errs.BadRequest(errs.Violation{
				Field: "spec.preemptible", Message: fmt.Sprintf("platform %s does not allow preemptible instances", pp.Platform.GetMetadata().GetName()),
			})
		case spec.GetRecoveryPolicy() != computepb.InstanceRecoveryPolicy_FAIL:
			return errs.BadRequest(errs.Violation{Field: "spec.recovery_policy", Message: "preemptible instances require recovery_policy FAIL"})
		case rp.GetPolicy() == computepb.ReservationPolicy_STRICT:
			return errs.BadRequest(errs.Violation{Field: "spec.reservation_policy.policy", Message: "preemptible instances cannot use reservations"})
		}
	}
	if rp.GetPolicy() == computepb.ReservationPolicy_STRICT {
		// STRICT runs the instance only in a reservation. The mocker has
		// none (no capacity service), so it can never be satisfied.
		return errs.BadState("", "reservation_policy STRICT: no capacity reservation is available to this project (NebiusMocker has no capacity blocks)")
	}
	return nil
}

// checkServiceAccount requires a service account of the instance's project.
func (s *InstanceService) checkServiceAccount(ctx context.Context, projectID, id string) error {
	if id == "" {
		return nil
	}
	const field = "spec.service_account_id"
	if !strings.HasPrefix(id, kindServiceAccount+"-") {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a service account ID"})
	}
	parent, err := s.d.Projects.Parent(ctx, id)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if parent != projectID {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("service account %s is not in project %s", id, projectID)})
	}
	return nil
}

// checkNICs validates the network interfaces: 1 to 8, unique names, an
// ip_address on each, and subnets of the instance's project.
func (s *InstanceService) checkNICs(ctx context.Context, projectID string, spec *computepb.InstanceSpec) error {
	nics := spec.GetNetworkInterfaces()
	if len(nics) == 0 || len(nics) > maxNICs {
		return errs.BadRequest(errs.Violation{Field: "spec.network_interfaces", Message: fmt.Sprintf("between 1 and %d network interfaces are required", maxNICs)})
	}
	net, err := s.networkInterfaces()
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for i, nic := range nics {
		field := fmt.Sprintf("spec.network_interfaces[%d]", i)
		switch {
		case nic.GetName() == "":
			return errs.BadRequest(errs.Violation{Field: field + ".name", Message: "required"})
		case names[nic.GetName()]:
			return errs.BadRequest(errs.Violation{Field: field + ".name", Message: fmt.Sprintf("interface name %q is used twice", nic.GetName())})
		case nic.GetIpAddress() == nil:
			return errs.BadRequest(errs.Violation{Field: field + ".ip_address", Message: "required (an empty ip_address requests a dynamic address)"})
		case nic.GetSubnetId() == "":
			return errs.BadRequest(errs.Violation{Field: field + ".subnet_id", Message: "required"})
		}
		names[nic.GetName()] = true
		sn, err := net.Subnet(ctx, nic.GetSubnetId())
		if err != nil {
			return fmt.Errorf("%s.subnet_id: %w", field, err)
		}
		if sn.ProjectID != projectID {
			return errs.BadRequest(errs.Violation{Field: field + ".subnet_id", Message: fmt.Sprintf("subnet %s is not in project %s", sn.ID, projectID)})
		}
	}
	return nil
}
