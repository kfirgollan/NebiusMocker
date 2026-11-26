package vpc

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/vpc/domain"
)

// networksPeer provides peer.Networks to other modules. It is in-process: no
// RPC authorization; the consumer authorizes its own request.
type networksPeer struct{ b *domain.NetworkBindings }

var _ peer.Networks = networksPeer{}

// Network implements peer.Networks.
func (p networksPeer) Network(ctx context.Context, id string) (peer.Network, error) {
	n, err := p.b.Network(ctx, id)
	if err != nil {
		return peer.Network{}, err
	}
	md := n.GetMetadata()
	return peer.Network{ID: md.GetId(), ProjectID: md.GetParentId(), Name: md.GetName()}, nil
}

// Bind implements peer.Networks.
func (p networksPeer) Bind(ctx context.Context, networkID, userID string) error {
	return p.b.Bind(ctx, networkID, userID)
}

// Unbind implements peer.Networks.
func (p networksPeer) Unbind(ctx context.Context, networkID, userID string) error {
	return p.b.Unbind(ctx, networkID, userID)
}

// defaultSubnetsPeer provides peer.DefaultSubnets to other modules. It is
// in-process: no RPC authorization; the consumer authorizes its own request.
type defaultSubnetsPeer struct{ n *domain.NetworkService }

var _ peer.DefaultSubnets = defaultSubnetsPeer{}

// DefaultSubnet implements peer.DefaultSubnets.
func (p defaultSubnetsPeer) DefaultSubnet(ctx context.Context, projectID string) (peer.Subnet, error) {
	d, err := p.n.GetDefault(ctx, projectID)
	if err != nil {
		return peer.Subnet{}, err
	}
	if d.Subnet == nil {
		return peer.Subnet{}, errs.NotFound(projectID + "/default-subnet")
	}
	md := d.Subnet.GetMetadata()
	return peer.Subnet{ID: md.GetId(), ProjectID: md.GetParentId(), NetworkID: d.Subnet.GetSpec().GetNetworkId()}, nil
}
