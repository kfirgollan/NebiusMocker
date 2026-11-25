package vpc

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/vpc/domain"
)

// nicPeer provides peer.NetworkInterfaces to other modules. It is in-process:
// no RPC authorization; the consumer authorizes its own request.
type nicPeer struct{ svc *domain.NICService }

var _ peer.NetworkInterfaces = nicPeer{}

// Subnet implements peer.NetworkInterfaces.
func (p nicPeer) Subnet(ctx context.Context, subnetID string) (peer.Subnet, error) {
	sn, err := p.svc.Subnet(ctx, subnetID)
	if err != nil {
		return peer.Subnet{}, err
	}
	return peer.Subnet{ID: sn.ID, ProjectID: sn.ProjectID, NetworkID: sn.NetworkID}, nil
}

// BindInterface implements peer.NetworkInterfaces.
func (p nicPeer) BindInterface(ctx context.Context, b peer.InterfaceBinding) (peer.InterfaceAddresses, error) {
	nb := domain.NICBinding{
		InstanceID: b.InstanceID, ProjectID: b.ProjectID, Name: b.Name, SubnetID: b.SubnetID,
		PrivateAllocationID: b.PrivateAllocationID, AliasAllocationIDs: b.AliasAllocationIDs, SecurityGroupIDs: b.SecurityGroupIDs,
	}
	if b.Public != nil {
		nb.Public = &domain.NICPublic{AllocationID: b.Public.AllocationID}
	}
	a, err := p.svc.BindInterface(ctx, nb)
	if err != nil {
		return peer.InterfaceAddresses{}, err
	}
	return peer.InterfaceAddresses{
		NetworkID: a.NetworkID, PrivateAddress: a.PrivateAddress, PrivateAllocationID: a.PrivateAllocationID,
		PublicAddress: a.PublicAddress, PublicAllocationID: a.PublicAllocationID,
		AliasCIDRs: a.AliasCIDRs, SecurityGroupIDs: a.SecurityGroupIDs,
	}, nil
}

// ReleaseInterface implements peer.NetworkInterfaces.
func (p nicPeer) ReleaseInterface(ctx context.Context, instanceID, name string) error {
	return p.svc.ReleaseInterface(ctx, instanceID, name)
}
