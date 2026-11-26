package contract

import (
	"context"
	"net/netip"
	"testing"

	"github.com/nebius/gosdk/operations/alphaops"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"
	vpcalpha "github.com/nebius/gosdk/services/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// alphaClients are gosdk clients for the v1alpha1 VPC services.
type alphaClients struct {
	allocations vpcalpha.AllocationService
	pools       vpcalpha.PoolService
	networks    vpcalpha.NetworkService
	subnets     vpcalpha.SubnetService
	scopes      vpcalpha.ScopeService
}

func alphaClientsFor(t *testing.T, inst *testkit.Instance, token string) alphaClients {
	t.Helper()
	sdk := inst.SDKAs(t, token)
	return alphaClients{
		allocations: vpcalpha.NewAllocationService(sdk),
		pools:       vpcalpha.NewPoolService(sdk),
		networks:    vpcalpha.NewNetworkService(sdk),
		subnets:     vpcalpha.NewSubnetService(sdk),
		scopes:      vpcalpha.NewScopeService(sdk),
	}
}

// startAlpha returns v1 and v1alpha1 clients over one mocker (admin token).
func startAlpha(t *testing.T) (vpcClients, alphaClients) {
	t.Helper()
	c := startVPC(t)
	return c, alphaClientsFor(t, c.inst, c.inst.Token)
}

// alphaWait waits for a v1alpha1 operation and returns its resource ID.
//
//nolint:staticcheck // SA1019: alphaops.Operation is deprecated upstream, still what v1alpha1 APIs return
func alphaWait(t *testing.T) func(*alphaops.Operation, error) string {
	t.Helper()
	return func(op *alphaops.Operation, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("start operation: %v", err)
		}
		if _, err := op.Wait(t.Context()); err != nil {
			t.Fatalf("operation %s: %v", op.ID(), err)
		}
		return op.ResourceID()
	}
}

// alphaOK waits for a started v1alpha1 operation and returns its error:
// alphaOK(ctx)(svc.Update(...)).
//
//nolint:staticcheck // SA1019: alphaops.Operation is deprecated upstream, still what v1alpha1 APIs return
func alphaOK(ctx context.Context) func(*alphaops.Operation, error) error {
	return func(op *alphaops.Operation, err error) error {
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
}

func alphaPrivSpec(subnetID, poolID, cidr string) *vpcv1alpha1pb.AllocationSpec {
	p := &vpcv1alpha1pb.IPv4PrivateAllocationSpec{Cidr: cidr}
	switch {
	case subnetID != "":
		p.Pool = &vpcv1alpha1pb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: subnetID}
	case poolID != "":
		p.Pool = &vpcv1alpha1pb.IPv4PrivateAllocationSpec_PoolId{PoolId: poolID}
	}
	return &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Private{Ipv4Private: p}}
}

func alphaPubSpec(poolID, cidr string) *vpcv1alpha1pb.AllocationSpec {
	return &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Public{
		Ipv4Public: &vpcv1alpha1pb.IPv4PublicAllocationSpec{PoolId: poolID, Cidr: cidr},
	}}
}

func alphaAllocReq(project, name string, spec *vpcv1alpha1pb.AllocationSpec) *vpcv1alpha1pb.CreateAllocationRequest {
	return &vpcv1alpha1pb.CreateAllocationRequest{Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: name}, Spec: spec}
}

func newAlphaAlloc(t *testing.T, c vpcClients, a alphaClients, name string, spec *vpcv1alpha1pb.AllocationSpec) string {
	t.Helper()
	return alphaWait(t)(a.allocations.Create(t.Context(), alphaAllocReq(c.inst.ProjectID, name, spec)))
}

func getAlphaAlloc(t *testing.T, a alphaClients, id string) *vpcv1alpha1pb.Allocation {
	t.Helper()
	r, err := a.allocations.Get(t.Context(), &vpcv1alpha1pb.GetAllocationRequest{Id: id})
	if err != nil {
		t.Fatalf("v1alpha1 Get Allocation %s: %v", id, err)
	}
	return r
}

// defaultSubnet returns the project's default subnet ID via v1alpha1.
func defaultSubnet(t *testing.T, a alphaClients, project string) string {
	t.Helper()
	r, err := a.networks.GetDefault(t.Context(), &vpcv1alpha1pb.GetDefaultNetworkRequest{ParentId: project})
	if err != nil {
		t.Fatalf("v1alpha1 GetDefault: %v", err)
	}
	return r.GetSubnet().GetMetadata().GetId()
}

// hostIn returns the address n hosts into block, as a /32.
func hostIn(t *testing.T, block string, n int) string {
	t.Helper()
	p, err := netip.ParsePrefix(block)
	if err != nil {
		t.Fatalf("parse %q: %v", block, err)
	}
	ip := p.Masked().Addr()
	for range n {
		ip = ip.Next()
	}
	return netip.PrefixFrom(ip, 32).String()
}
