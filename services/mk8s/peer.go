package mk8s

import (
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// The domain service resolves clusters for other areas in-process, without
// RPC authorization: the consumer authorizes its own request.
var _ peer.MK8sClusters = (*domain.Service)(nil)

// peers finds the capabilities mk8s uses at call time (module order is not
// guaranteed; a provider may be disabled).
type peers struct{ d *peer.Directory }

var _ domain.Peers = peers{}

func (p peers) ComputeNodes() (peer.ComputeNodes, bool) {
	return peer.Lookup(p.d, peer.ComputeNodesKey)
}

func (p peers) DefaultSubnets() (peer.DefaultSubnets, bool) {
	return peer.Lookup(p.d, peer.DefaultSubnetsKey)
}

func (p peers) NetworkInterfaces() (peer.NetworkInterfaces, bool) {
	return peer.Lookup(p.d, peer.NetworkInterfacesKey)
}

func (p peers) Networks() (peer.Networks, bool) { return peer.Lookup(p.d, peer.NetworksKey) }

func (p peers) ClusterReleases() (peer.ClusterReleases, bool) {
	return peer.Lookup(p.d, peer.ClusterReleasesKey)
}
