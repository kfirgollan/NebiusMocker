package peer

import "context"

// NetworkInterfacesKey is the capability provided by the vpc module: the
// addresses of Compute instance network interfaces.
var NetworkInterfacesKey = NewKey[NetworkInterfaces]("vpc.network-interfaces")

// NetworkInterfaces gives Compute instance network interfaces (NICs) their
// VPC addresses. Every address of a NIC is a vpcallocation whose
// status.assignment.network_interface names the instance and the NIC, with
// type PRIMARY (private address), PUBLIC or ALIAS, and state ASSIGNED.
//
// Allocations created by BindInterface for a NIC (ephemeral addresses) have
// status.static=false: their lifecycle follows the NIC, so they are deleted
// when the NIC releases them. Allocations the user created beforehand are
// only unassigned, and survive.
//
// It is an internal, privileged call: the provider runs no IAM checks, since
// the consumer has already authorized its own request. Every method is
// atomic with respect to other VPC changes.
type NetworkInterfaces interface {
	// Subnet returns a subnet's project and network, or errs.NotFound.
	Subnet(ctx context.Context, subnetID string) (Subnet, error)

	// BindInterface makes the allocations assigned to one NIC match b, and
	// returns the effective addresses. It is idempotent and reconciling:
	//
	//   - Private: b.PrivateAllocationID binds that allocation as PRIMARY.
	//     Empty keeps the NIC's current ephemeral private address, or creates
	//     one in the subnet (lowest free address). A private address that
	//     changes (another allocation ID) releases the previous one.
	//   - Public: nil means no public address (the current one is released).
	//     A binding with an AllocationID assigns that allocation as PUBLIC;
	//     without one, the current ephemeral public address is kept, or a new
	//     one is created from the subnet's public space.
	//   - Aliases: exactly AliasAllocationIDs are assigned as ALIAS.
	//   - Security groups: each must exist and belong to the subnet's
	//     network; empty means the network's default security group, which
	//     is returned in InterfaceAddresses.SecurityGroupIDs.
	//
	// An explicit allocation must exist (errs.NotFound), be in b.ProjectID,
	// have the right visibility, lie in the subnet's address space (or be
	// associated with that subnet), and not be assigned to another NIC
	// (errs.BadState). Violations name fields relative to the NIC spec, e.g.
	// "ip_address.allocation_id" or "security_groups[1].id". On any error
	// nothing changes.
	BindInterface(ctx context.Context, b InterfaceBinding) (InterfaceAddresses, error)

	// ReleaseInterface frees every allocation assigned to the NIC: ephemeral
	// ones are deleted, the others are unassigned (state ALLOCATED). A NIC
	// with nothing assigned is not an error.
	ReleaseInterface(ctx context.Context, instanceID, name string) error
}

// Subnet is a resolved VPC subnet.
type Subnet struct {
	ID        string
	ProjectID string
	NetworkID string
	// NetworkCIDRs are the network's private IPv4 CIDRs (its private pools'
	// and the subnet's own); filled by NetworkInterfaces.Subnet.
	NetworkCIDRs []string
}

// InterfaceBinding is the desired state of one instance NIC.
type InterfaceBinding struct {
	InstanceID string
	ProjectID  string
	// Name is the NIC name, unique within the instance.
	Name     string
	SubnetID string
	// PrivateAllocationID is an existing private allocation, or "" for an
	// ephemeral address.
	PrivateAllocationID string
	// Public is the public address, or nil for none.
	Public             *PublicBinding
	AliasAllocationIDs []string
	SecurityGroupIDs   []string
}

// PublicBinding asks for a public address: an existing allocation, or an
// ephemeral one when AllocationID is "".
type PublicBinding struct {
	AllocationID string
}

// InterfaceAddresses are the effective addresses of a NIC.
type InterfaceAddresses struct {
	NetworkID string
	// PrivateAddress is the private IPv4 address without a prefix length.
	PrivateAddress      string
	PrivateAllocationID string
	// PublicAddress is "" when the NIC has no public address.
	PublicAddress      string
	PublicAllocationID string
	// AliasCIDRs are the alias allocations' CIDRs, in AliasAllocationIDs order.
	AliasCIDRs []string
	// SecurityGroupIDs are the effective security groups.
	SecurityGroupIDs []string
}
