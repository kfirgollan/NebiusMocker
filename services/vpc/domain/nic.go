package domain

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// NICService gives Compute instance network interfaces (NICs) their VPC
// addresses. Every address is a vpcallocation whose status.assignment names
// the instance and the NIC. All mutations hold core.mu, so IPAM decisions are
// atomic with the rest of VPC.
type NICService struct{ c *core }

// NICBinding is the desired state of one instance NIC. Violation fields are
// relative to the NIC spec ("ip_address.allocation_id", "security_groups[1].id").
type NICBinding struct {
	InstanceID string
	ProjectID  string
	// Name is the NIC name, unique within the instance.
	Name     string
	SubnetID string
	// PrivateAllocationID is an existing private allocation, or "" for an
	// ephemeral address.
	PrivateAllocationID string
	// Public asks for a public address; nil means none.
	Public             *NICPublic
	AliasAllocationIDs []string
	SecurityGroupIDs   []string
}

// NICPublic asks for a public address: an existing allocation, or an
// ephemeral one when AllocationID is "".
type NICPublic struct{ AllocationID string }

// NICAddresses are the effective addresses of a NIC.
type NICAddresses struct {
	NetworkID           string
	PrivateAddress      string
	PrivateAllocationID string
	PublicAddress       string
	PublicAllocationID  string
	AliasCIDRs          []string
	SecurityGroupIDs    []string
}

// NICSubnet is a resolved subnet.
type NICSubnet struct {
	ID, ProjectID, NetworkID string
	// NetworkCIDRs are the private CIDRs of the network: its private pools'
	// and the subnet's own.
	NetworkCIDRs []string
}

// NICs returns the network interface service.
func (s *Service) NICs() *NICService { return &NICService{c: s.c} }

// Subnet returns a subnet's project and network, or errs.NotFound.
func (n *NICService) Subnet(ctx context.Context, subnetID string) (NICSubnet, error) {
	sn, err := n.c.r.Subnets.Get(ctx, subnetID)
	if err != nil {
		return NICSubnet{}, err
	}
	out := NICSubnet{ID: subnetID, ProjectID: sn.GetMetadata().GetParentId(), NetworkID: sn.GetSpec().GetNetworkId()}
	for _, p := range sn.GetStatus().GetIpv4PrivatePools() {
		out.NetworkCIDRs = append(out.NetworkCIDRs, p.GetCidrs()...)
	}
	net, err := n.c.r.Networks.Get(ctx, out.NetworkID)
	if err != nil {
		return NICSubnet{}, err
	}
	for _, ref := range net.GetSpec().GetIpv4PrivatePools().GetPools() {
		pool, err := n.c.r.Pools.Get(ctx, ref.GetId())
		if errs.IsKind(err, errs.KindNotFound) {
			continue
		}
		if err != nil {
			return NICSubnet{}, err
		}
		out.NetworkCIDRs = append(out.NetworkCIDRs, pool.GetStatus().GetCidrs()...)
	}
	slices.Sort(out.NetworkCIDRs)
	out.NetworkCIDRs = slices.Compact(out.NetworkCIDRs)
	return out, nil
}

// slot is one address of a NIC in the desired state.
type slot struct {
	typ   vpcpb.NetworkInterfaceAssignment_Type
	field string
	vis   vpcpb.IpVisibility
	// id is the allocation to keep or bind; "" means create an ephemeral one.
	id string
	// created is the new ephemeral allocation, set during planning.
	created *vpcpb.Allocation
}

type nicPlan struct {
	b       NICBinding
	subnet  *vpcpb.Subnet
	current []*vpcpb.Allocation
	slots   []*slot
	// bind are the explicit allocations to assign.
	bind map[string]*vpcpb.Allocation
}

// BindInterface makes the allocations assigned to the NIC match b and returns
// the effective addresses. It is idempotent; on any error nothing changes.
func (n *NICService) BindInterface(ctx context.Context, b NICBinding) (NICAddresses, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	sn, err := ref(ctx, n.c.r.Subnets, "subnet_id", b.SubnetID)
	if err != nil {
		return NICAddresses{}, err
	}
	if err := sameParent("subnet_id", b.ProjectID, sn); err != nil {
		return NICAddresses{}, err
	}
	if st := sn.GetStatus().GetState(); st != vpcpb.SubnetStatus_READY {
		return NICAddresses{}, errs.BadState(b.SubnetID, fmt.Sprintf("subnet is %s, not READY", st))
	}
	networkID := sn.GetSpec().GetNetworkId()
	sgs, err := n.securityGroups(ctx, networkID, b.SecurityGroupIDs)
	if err != nil {
		return NICAddresses{}, err
	}
	current, err := n.assigned(ctx, b.InstanceID, b.Name)
	if err != nil {
		return NICAddresses{}, err
	}
	p := &nicPlan{b: b, subnet: sn, current: current, bind: map[string]*vpcpb.Allocation{}}
	if err := n.plan(ctx, p); err != nil {
		return NICAddresses{}, err
	}
	if err := n.admitApply(ctx, p); err != nil {
		return NICAddresses{}, err
	}
	out, err := n.addresses(ctx, p)
	out.NetworkID, out.SecurityGroupIDs = networkID, sgs
	return out, err
}

// ReleaseInterface frees every allocation assigned to the NIC.
func (n *NICService) ReleaseInterface(ctx context.Context, instanceID, name string) error {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	current, err := n.assigned(ctx, instanceID, name)
	if err != nil {
		return err
	}
	for _, a := range current {
		if err := n.release(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func (n *NICService) assigned(ctx context.Context, instanceID, name string) ([]*vpcpb.Allocation, error) {
	return n.c.r.Allocations.All(ctx, func(a *vpcpb.Allocation) bool {
		ni := a.GetStatus().GetAssignment().GetNetworkInterface()
		return ni != nil && ni.GetInstanceId() == instanceID && ni.GetName() == name
	})
}

// securityGroups validates the groups and returns the effective list.
func (n *NICService) securityGroups(ctx context.Context, networkID string, ids []string) ([]string, error) {
	for i, id := range ids {
		field := fmt.Sprintf("security_groups[%d].id", i)
		sg, err := ref(ctx, n.c.r.SecurityGroups, field, id)
		if err != nil {
			return nil, err
		}
		if sg.GetSpec().GetNetworkId() != networkID {
			return nil, errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("security group %s belongs to network %s, not %s", id, sg.GetSpec().GetNetworkId(), networkID)})
		}
	}
	if len(ids) > 0 {
		return slices.Clone(ids), nil
	}
	def, err := n.c.r.SecurityGroups.All(ctx, func(sg *vpcpb.SecurityGroup) bool {
		return sg.GetStatus().GetDefault() && sg.GetSpec().GetNetworkId() == networkID
	})
	if err != nil {
		return nil, err
	}
	return idsOf(def), nil
}

// plan decides what each address of the NIC becomes and validates it. It
// writes nothing.
func (n *NICService) plan(ctx context.Context, p *nicPlan) error {
	b := p.b
	p.slots = append(p.slots, &slot{typ: vpcpb.NetworkInterfaceAssignment_PRIMARY, field: "ip_address.allocation_id", vis: vpcpb.IpVisibility_PRIVATE, id: b.PrivateAllocationID})
	if b.Public != nil {
		p.slots = append(p.slots, &slot{typ: vpcpb.NetworkInterfaceAssignment_PUBLIC, field: "public_ip_address.allocation_id", vis: vpcpb.IpVisibility_PUBLIC, id: b.Public.AllocationID})
	}
	for i, id := range b.AliasAllocationIDs {
		p.slots = append(p.slots, &slot{typ: vpcpb.NetworkInterfaceAssignment_ALIAS, field: fmt.Sprintf("aliases[%d].allocation_id", i), vis: vpcpb.IpVisibility_PRIVATE, id: id})
	}
	used := map[string]bool{}
	space := map[vpcpb.IpVisibility][]netip.Prefix{}
	for _, s := range p.slots {
		if s.id == "" {
			s.id = n.keepEphemeral(p, s)
			if s.id == "" {
				a, err := n.newEphemeral(ctx, p, s)
				if err != nil {
					return err
				}
				s.created = a
				continue
			}
		}
		if used[s.id] {
			return errs.BadRequest(errs.Violation{Field: s.field, Message: fmt.Sprintf("allocation %s is used by another address of this interface", s.id)})
		}
		used[s.id] = true
		a, err := n.checkExisting(ctx, p, s, space)
		if err != nil {
			return err
		}
		p.bind[s.id] = a
	}
	return nil
}

// keepEphemeral returns the ID of the NIC's current ephemeral allocation of
// the slot's type in the same subnet, or "".
func (n *NICService) keepEphemeral(p *nicPlan, s *slot) string {
	for _, a := range p.current {
		if a.GetStatus().GetAssignment().GetNetworkInterface().GetType() == s.typ && !a.GetStatus().GetStatic() &&
			a.GetStatus().GetDetails().GetSubnetId() == p.b.SubnetID {
			return a.GetMetadata().GetId()
		}
	}
	return ""
}

func (n *NICService) assignment(p *nicPlan, s *slot) *vpcpb.Assignment {
	return &vpcpb.Assignment{Type: &vpcpb.Assignment_NetworkInterface{NetworkInterface: &vpcpb.NetworkInterfaceAssignment{
		InstanceId: p.b.InstanceID, Name: p.b.Name, Type: s.typ,
	}}}
}

// newEphemeral picks the lowest free address for a NIC-owned allocation. Its
// name is "<instance>-<nic>-private" or "-public", since the store keeps names
// unique per project.
func (n *NICService) newEphemeral(ctx context.Context, p *nicPlan, s *slot) (*vpcpb.Allocation, error) {
	suffix := "private"
	spec := &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
		Pool: &vpcpb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: p.b.SubnetID},
	}}}
	if s.vis == vpcpb.IpVisibility_PUBLIC {
		suffix = "public"
		var err error
		if spec, err = n.publicSpec(ctx, p); err != nil {
			return nil, err
		}
	}
	id, err := n.c.ids.New(KindAllocation)
	if err != nil {
		return nil, fmt.Errorf("new allocation id: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s", p.b.InstanceID, p.b.Name, suffix)
	a := &vpcpb.Allocation{Metadata: n.c.metadata(id, p.b.ProjectID, name, nil), Spec: spec}
	req, err := requestOf(spec)
	if err != nil {
		return nil, err
	}
	details, err := n.c.allocations.place(ctx, a, req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.field, err)
	}
	details.SubnetId = p.b.SubnetID
	a.Status = &vpcpb.AllocationStatus{State: vpcpb.AllocationStatus_ASSIGNED, Details: details, Static: false, Assignment: n.assignment(p, s)}
	return a, nil
}

// publicSpec requests a public address from the subnet's public space, or from
// the project's default public pool when the subnet has none.
func (n *NICService) publicSpec(ctx context.Context, p *nicPlan) (*vpcpb.AllocationSpec, error) {
	blocks, _, err := n.c.allocations.subnetSpace(ctx, p.subnet, vpcpb.IpVisibility_PUBLIC, "")
	if err != nil {
		return nil, err
	}
	if len(blocks) > 0 {
		return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{
			Pool: &vpcpb.IPv4PublicAllocationSpec_SubnetId{SubnetId: p.b.SubnetID},
		}}}, nil
	}
	pool, err := n.c.r.Pools.GetByName(ctx, p.b.ProjectID, DefaultPublicPoolName)
	if err != nil {
		return nil, fmt.Errorf("default public pool of %s: %w", p.b.ProjectID, err)
	}
	return &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{
		Pool: &vpcpb.IPv4PublicAllocationSpec_PoolId{PoolId: pool.GetMetadata().GetId()},
	}}}, nil
}

// subnetAddressSpace returns where the subnet hands out addresses of one
// visibility; an empty public space falls back to the default public pool.
func (n *NICService) subnetAddressSpace(ctx context.Context, p *nicPlan, vis vpcpb.IpVisibility) ([]netip.Prefix, error) {
	blocks, _, err := n.c.allocations.subnetSpace(ctx, p.subnet, vis, "")
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 && vis == vpcpb.IpVisibility_PUBLIC {
		pool, err := n.c.r.Pools.GetByName(ctx, p.b.ProjectID, DefaultPublicPoolName)
		if err != nil {
			return nil, fmt.Errorf("default public pool of %s: %w", p.b.ProjectID, err)
		}
		blocks = poolBlocks(pool)
	}
	return prefixes(blocks), nil
}

// checkExisting validates an allocation named by ID for a slot.
func (n *NICService) checkExisting(ctx context.Context, p *nicPlan, s *slot, space map[vpcpb.IpVisibility][]netip.Prefix) (*vpcpb.Allocation, error) {
	a, err := ref(ctx, n.c.r.Allocations, s.field, s.id)
	if err != nil {
		return nil, err
	}
	if a.GetStatus().GetAssignment() != nil {
		cur := a.GetStatus().GetAssignment().GetNetworkInterface()
		if cur != nil && cur.GetInstanceId() == p.b.InstanceID && cur.GetName() == p.b.Name {
			return a, nil
		}
		return nil, errs.BadState(s.id, fmt.Sprintf("allocation %s is assigned to %s", s.id, assignedTo(a)))
	}
	if err := sameParent(s.field, p.b.ProjectID, a); err != nil {
		return nil, err
	}
	req, err := requestOf(a.GetSpec())
	if err != nil || req.vis != s.vis {
		return nil, errs.BadRequest(errs.Violation{Field: s.field, Message: fmt.Sprintf("allocation %s is not %s", s.id, s.vis)})
	}
	if st := a.GetStatus().GetState(); st != vpcpb.AllocationStatus_ALLOCATED {
		return nil, errs.BadState(s.id, fmt.Sprintf("allocation %s is %s, not ALLOCATED", s.id, st))
	}
	d := a.GetStatus().GetDetails()
	if d.GetSubnetId() == p.b.SubnetID {
		return a, nil
	}
	if d.GetSubnetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: s.field, Message: fmt.Sprintf("allocation %s belongs to subnet %s, not %s", s.id, d.GetSubnetId(), p.b.SubnetID)})
	}
	if _, ok := space[s.vis]; !ok {
		if space[s.vis], err = n.subnetAddressSpace(ctx, p, s.vis); err != nil {
			return nil, err
		}
	}
	pre, err := netip.ParsePrefix(d.GetAllocatedCidr())
	if err != nil || !contained(pre, space[s.vis]) {
		return nil, errs.BadRequest(errs.Violation{Field: s.field, Message: fmt.Sprintf("allocation %s (%s) is outside the address space of subnet %s", s.id, d.GetAllocatedCidr(), p.b.SubnetID)})
	}
	return a, nil
}

func assignedTo(a *vpcpb.Allocation) string {
	if ni := a.GetStatus().GetAssignment().GetNetworkInterface(); ni != nil {
		return ni.GetInstanceId() + "/" + ni.GetName()
	}
	return "another resource"
}

// admitApply writes the plan, charging the quotas of the ephemeral allocations
// it creates (they are real allocations, public ones real public addresses).
// The whole write is the admitted apply, so the charge is atomic with it.
func (n *NICService) admitApply(ctx context.Context, p *nicPlan) error {
	amounts := map[string]int64{}
	for _, s := range p.slots {
		if s.created != nil {
			for name, v := range allocationAmounts(s.created) {
				amounts[name] += v
			}
		}
	}
	if len(amounts) == 0 {
		return n.apply(ctx, p)
	}
	claim := peer.QuotaClaim{ContainerID: p.b.ProjectID, Amounts: amounts}
	return n.c.gate.Admit(ctx, claim, func(ctx context.Context) error { return n.apply(ctx, p) })
}

// apply writes the plan: release what the NIC no longer uses, then create and
// assign. Everything was validated, so failures here are store failures.
func (n *NICService) apply(ctx context.Context, p *nicPlan) error {
	keep := map[string]bool{}
	for _, s := range p.slots {
		if s.id != "" {
			keep[s.id] = true
		}
	}
	for _, a := range p.current {
		if !keep[a.GetMetadata().GetId()] {
			if err := n.release(ctx, a); err != nil {
				return err
			}
		}
	}
	for _, s := range p.slots {
		if s.created != nil {
			if err := n.c.r.Allocations.Create(ctx, s.created); err != nil {
				return fmt.Errorf("%s: %w", s.field, err)
			}
			s.id = s.created.GetMetadata().GetId()
			continue
		}
		if err := n.assign(ctx, p.bind[s.id], p, s); err != nil {
			return err
		}
	}
	return nil
}

func (n *NICService) assign(ctx context.Context, a *vpcpb.Allocation, p *nicPlan, s *slot) error {
	want := n.assignment(p, s)
	if proto.Equal(a.GetStatus().GetAssignment(), want) && a.GetStatus().GetState() == vpcpb.AllocationStatus_ASSIGNED {
		return nil
	}
	a.Status.State = vpcpb.AllocationStatus_ASSIGNED
	a.Status.Assignment = want
	if a.GetStatus().GetDetails() == nil {
		a.Status.Details = &vpcpb.AllocationDetails{}
	}
	a.Status.Details.SubnetId = p.b.SubnetID
	return n.touch(ctx, a)
}

// release deletes an ephemeral allocation and unassigns the others.
func (n *NICService) release(ctx context.Context, a *vpcpb.Allocation) error {
	if !a.GetStatus().GetStatic() {
		return n.c.r.Allocations.Delete(ctx, a.GetMetadata().GetId())
	}
	a.Status.State = vpcpb.AllocationStatus_ALLOCATED
	a.Status.Assignment = nil
	if req, err := requestOf(a.GetSpec()); err != nil || req.subnetID == "" {
		a.Status.GetDetails().SubnetId = ""
	}
	return n.touch(ctx, a)
}

func (n *NICService) touch(ctx context.Context, a *vpcpb.Allocation) error {
	md := a.GetMetadata()
	md.ResourceVersion++
	md.UpdatedAt = timestamppb.New(n.c.clk.Now())
	return n.c.r.Allocations.Update(ctx, a)
}

// addresses reads back the effective addresses of the NIC.
func (n *NICService) addresses(ctx context.Context, p *nicPlan) (NICAddresses, error) {
	var out NICAddresses
	for _, s := range p.slots {
		a, err := n.c.r.Allocations.Get(ctx, s.id)
		if err != nil {
			return NICAddresses{}, err
		}
		cidr := a.GetStatus().GetDetails().GetAllocatedCidr()
		pre, err := netip.ParsePrefix(cidr)
		if err != nil {
			return NICAddresses{}, errs.Internal(fmt.Sprintf("allocation %s has malformed allocated_cidr %q", s.id, cidr))
		}
		switch s.typ { //nolint:exhaustive // ALIAS is the default branch
		case vpcpb.NetworkInterfaceAssignment_PRIMARY:
			out.PrivateAddress, out.PrivateAllocationID = pre.Addr().String(), s.id
		case vpcpb.NetworkInterfaceAssignment_PUBLIC:
			out.PublicAddress, out.PublicAllocationID = pre.Addr().String(), s.id
		default:
			out.AliasCIDRs = append(out.AliasCIDRs, cidr)
		}
	}
	return out, nil
}
