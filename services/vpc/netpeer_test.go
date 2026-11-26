package vpc_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// peerProbe is a module that keeps the peer directory, as a consumer would.
type peerProbe struct{ peers *peer.Directory }

func (*peerProbe) Name() string { return "probe" }

func (p *peerProbe) Register(_ context.Context, _ grpc.ServiceRegistrar, deps mocker.Deps) error {
	p.peers = deps.Peers
	return nil
}

func TestDefaultSubnetsPeer(t *testing.T) {
	t.Parallel()
	probe := &peerProbe{}
	inst := testkit.StartT(t, []mocker.Module{vpc.New(), probe})
	ds, ok := peer.Lookup(probe.peers, peer.DefaultSubnetsKey)
	if !ok {
		t.Fatal("vpc does not provide peer.DefaultSubnets")
	}
	sn, err := ds.DefaultSubnet(t.Context(), inst.ProjectID)
	if err != nil {
		t.Fatalf("DefaultSubnet(seeded project): %v", err)
	}
	if !strings.HasPrefix(sn.ID, "vpcsubnet-") || !strings.HasPrefix(sn.NetworkID, "vpcnetwork-") || sn.ProjectID != inst.ProjectID {
		t.Errorf("DefaultSubnet = %+v, want a vpcsubnet of a vpcnetwork in %s", sn, inst.ProjectID)
	}
	nics, _ := peer.Lookup(probe.peers, peer.NetworkInterfacesKey)
	if len(sn.NetworkCIDRs) == 0 {
		t.Errorf("DefaultSubnet = %+v, want the network's private CIDRs", sn)
	}
	if got, err := nics.Subnet(t.Context(), sn.ID); err != nil || !reflect.DeepEqual(got, sn) {
		t.Errorf("NetworkInterfaces.Subnet(%s) = %+v, %v; want %+v", sn.ID, got, err, sn)
	}
	if _, err := ds.DefaultSubnet(t.Context(), "project-e00nodefaultnetwork"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("DefaultSubnet(project without network) error = %v, want not_found", err)
	}
}
