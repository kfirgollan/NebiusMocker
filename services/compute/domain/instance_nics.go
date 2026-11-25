package domain

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// networkInterfaces returns the VPC capability, or an error naming the field
// when the vpc module is not enabled.
func (s *InstanceService) networkInterfaces() (peer.NetworkInterfaces, error) {
	n, ok := s.d.Network.NetworkInterfaces()
	if !ok {
		return nil, errs.BadRequest(errs.Violation{
			Field: "spec.network_interfaces", Message: "the vpc service is not enabled in this NebiusMocker (NEBIUS_MOCKER_SERVICES)",
		})
	}
	return n, nil
}

// binding is the desired VPC state of one NIC for an instance that is (going
// to be) in state: a dynamic public address (no allocation_id,
// static=false) is assigned when it runs, and a stopped instance keeps the
// one its status shows until it is released (see SweepPublicIPs).
func binding(inst *computepb.Instance, nic *computepb.NetworkInterfaceSpec, state computepb.InstanceStatus_InstanceState) peer.InterfaceBinding {
	b := peer.InterfaceBinding{
		InstanceID: inst.GetMetadata().GetId(), ProjectID: inst.GetMetadata().GetParentId(),
		Name: nic.GetName(), SubnetID: nic.GetSubnetId(),
		PrivateAllocationID: nic.GetIpAddress().GetAllocationId(),
	}
	if pub := nic.GetPublicIpAddress(); pub != nil && (pub.GetAllocationId() != "" || pub.GetStatic() ||
		state == computepb.InstanceStatus_RUNNING || hasPublicAddress(inst, nic.GetName())) {
		b.Public = &peer.PublicBinding{AllocationID: pub.GetAllocationId()}
	}
	for _, a := range nic.GetAliases() {
		b.AliasAllocationIDs = append(b.AliasAllocationIDs, a.GetAllocationId())
	}
	for _, sg := range nic.GetSecurityGroups() {
		b.SecurityGroupIDs = append(b.SecurityGroupIDs, sg.GetId())
	}
	return b
}

// bindAll makes the VPC state of every NIC of spec match it and returns the
// NIC statuses; state is the instance state after the change. NICs of prev (the stored spec on update) that spec no longer
// has are released. On error the NICs touched so far are restored to prev
// (or released when prev does not have them).
func (s *InstanceService) bindAll(ctx context.Context, inst *computepb.Instance, spec *computepb.InstanceSpec, state computepb.InstanceStatus_InstanceState, prev *computepb.InstanceSpec) ([]*computepb.NetworkInterfaceStatus, error) {
	net, err := s.networkInterfaces()
	if err != nil {
		return nil, err
	}
	out := make([]*computepb.NetworkInterfaceStatus, 0, len(spec.GetNetworkInterfaces()))
	for i, nic := range spec.GetNetworkInterfaces() {
		addrs, err := net.BindInterface(ctx, binding(inst, nic, state))
		if err != nil {
			err = withFieldPrefix(err, fmt.Sprintf("spec.network_interfaces[%d].", i))
			return nil, errors.Join(err, s.restoreNICs(ctx, inst, spec.GetNetworkInterfaces()[:i], prev, state))
		}
		out = append(out, nicStatus(inst, spec, i, addrs))
	}
	for _, old := range prev.GetNetworkInterfaces() {
		if nicByName(spec, old.GetName()) == nil {
			if err := net.ReleaseInterface(ctx, inst.GetMetadata().GetId(), old.GetName()); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// restoreNICs undoes bindings of nics: those prev has are bound as in prev,
// the others are released. prev nil releases all of them.
func (s *InstanceService) restoreNICs(ctx context.Context, inst *computepb.Instance, nics []*computepb.NetworkInterfaceSpec, prev *computepb.InstanceSpec, state computepb.InstanceStatus_InstanceState) error {
	net, err := s.networkInterfaces()
	if err != nil {
		return err
	}
	var errList []error
	for _, nic := range nics {
		if old := nicByName(prev, nic.GetName()); old != nil {
			_, err := net.BindInterface(ctx, binding(inst, old, state))
			errList = append(errList, err)
			continue
		}
		errList = append(errList, net.ReleaseInterface(ctx, inst.GetMetadata().GetId(), nic.GetName()))
	}
	return errors.Join(errList...)
}

// releaseAll releases the addresses of every NIC of spec.
func (s *InstanceService) releaseAll(ctx context.Context, instanceID string, spec *computepb.InstanceSpec) error {
	if len(spec.GetNetworkInterfaces()) == 0 {
		return nil
	}
	net, err := s.networkInterfaces()
	if err != nil {
		return err
	}
	var errList []error
	for _, nic := range spec.GetNetworkInterfaces() {
		errList = append(errList, net.ReleaseInterface(ctx, instanceID, nic.GetName()))
	}
	return errors.Join(errList...)
}

func nicByName(spec *computepb.InstanceSpec, name string) *computepb.NetworkInterfaceSpec {
	for _, n := range spec.GetNetworkInterfaces() {
		if n.GetName() == name {
			return n
		}
	}
	return nil
}

// nicStatus renders the effective state of NIC i.
func nicStatus(inst *computepb.Instance, spec *computepb.InstanceSpec, i int, a peer.InterfaceAddresses) *computepb.NetworkInterfaceStatus {
	nic := spec.GetNetworkInterfaces()[i]
	id := inst.GetMetadata().GetId()
	host := spec.GetHostname()
	if host == "" {
		host = id
	}
	st := &computepb.NetworkInterfaceStatus{
		Index:      int32(i), //nolint:gosec // at most 8 interfaces (proto max_items)
		Name:       nic.GetName(),
		IpAddress:  &computepb.IPAddressStatus{Address: a.PrivateAddress, AllocationId: a.PrivateAllocationID},
		MacAddress: macAddress(id, nic.GetName()),
		Fqdn:       host + "." + a.NetworkID + ".compute.internal.",
	}
	if a.PublicAddress != "" {
		st.PublicIpAddress = &computepb.PublicIPAddressStatus{
			Address: a.PublicAddress, AllocationId: a.PublicAllocationID, Static: nic.GetPublicIpAddress().GetStatic(),
		}
	}
	if len(a.AliasCIDRs) > 0 {
		st.Aliases = &computepb.IPAliasesStatus{Cidrs: a.AliasCIDRs}
	}
	for _, sg := range a.SecurityGroupIDs {
		st.SecurityGroups = append(st.SecurityGroups, &computepb.SecurityGroupStatus{Id: sg})
	}
	return st
}

// macAddress is a stable, locally administered unicast MAC (02:4e:42:…,
// "NB") derived from the instance and NIC.
func macAddress(instanceID, nic string) string {
	h := sha256.Sum256([]byte(instanceID + "/" + nic))
	return fmt.Sprintf("02:4e:42:%02x:%02x:%02x", h[0], h[1], h[2])
}

// withFieldPrefix prefixes the violation fields of a domain error (VPC
// reports fields relative to the NIC spec); other errors are wrapped.
func withFieldPrefix(err error, prefix string) error {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || len(e.Violations) == 0 {
		return fmt.Errorf("%s %w", prefix[:len(prefix)-1], err)
	}
	out := *e
	out.Violations = make([]errs.Violation, len(e.Violations))
	for i, v := range e.Violations {
		out.Violations[i] = errs.Violation{Field: prefix + v.Field, Message: v.Message}
	}
	return &out
}
