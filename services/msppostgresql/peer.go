package msppostgresql

import (
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// peers finds the capabilities Managed PostgreSQL uses at call time (module
// order is not guaranteed; a provider may be disabled).
type peers struct{ d *peer.Directory }

var _ domain.Peers = peers{}

func (p peers) Networks() (peer.Networks, bool) { return peer.Lookup(p.d, peer.NetworksKey) }

func (p peers) DefaultSubnets() (peer.DefaultSubnets, bool) {
	return peer.Lookup(p.d, peer.DefaultSubnetsKey)
}

func (p peers) ProjectRegions() (peer.ProjectRegions, bool) {
	return peer.Lookup(p.d, peer.ProjectRegionsKey)
}
