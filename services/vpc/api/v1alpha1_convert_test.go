package api

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"
)

func TestAlphaPool(t *testing.T) {
	md := &commonpb.ResourceMetadata{Id: "vpcpool-1"}
	cidrs := []*vpcpb.PoolCidr{{Cidr: "10.0.0.0/16", State: vpcpb.AddressBlockState_DISABLED, MaxMaskLength: 24}}
	wantCidrs := []*vpcv1alpha1pb.PoolCidr{{Cidr: "10.0.0.0/16", State: vpcv1alpha1pb.PoolCidrState_DISABLED, AllowedMask: 24}}
	status := &vpcpb.PoolStatus{State: vpcpb.PoolStatus_READY, Cidrs: []string{"10.0.0.0/16"}, ScopeId: "vpcscope-1"}
	wantStatus := &vpcv1alpha1pb.PoolStatus{State: vpcv1alpha1pb.PoolStatus_READY, Cidrs: []string{"10.0.0.0/16"}, ScopeId: "vpcscope-1"}
	tests := []struct {
		name string
		in   *vpcpb.Pool
		want *vpcv1alpha1pb.Pool
	}{
		{
			name: "root pool sources its scope",
			in: &vpcpb.Pool{Metadata: md, Status: status, Spec: &vpcpb.PoolSpec{
				Version: vpcpb.IpVersion_IPV4, Visibility: vpcpb.IpVisibility_PRIVATE, Cidrs: cidrs,
			}},
			want: &vpcv1alpha1pb.Pool{Metadata: md, Status: wantStatus, Spec: &vpcv1alpha1pb.PoolSpec{
				Version: vpcv1alpha1pb.IpVersion_IPV4, Cidrs: wantCidrs,
				Source: &vpcv1alpha1pb.PoolSpec_SourceScopeId{SourceScopeId: "vpcscope-1"},
			}},
		},
		{
			name: "child pool sources its pool",
			in: &vpcpb.Pool{Metadata: md, Status: status, Spec: &vpcpb.PoolSpec{
				SourcePoolId: "vpcpool-0", Version: vpcpb.IpVersion_IPV4, Cidrs: cidrs,
			}},
			want: &vpcv1alpha1pb.Pool{Metadata: md, Status: wantStatus, Spec: &vpcv1alpha1pb.PoolSpec{
				Version: vpcv1alpha1pb.IpVersion_IPV4, Cidrs: wantCidrs,
				Source: &vpcv1alpha1pb.PoolSpec_SourcePoolId{SourcePoolId: "vpcpool-0"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmpDiff(tt.want, alphaPool(tt.in)); diff != "" {
				t.Fatalf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
	if alphaPool(nil) != nil {
		t.Fatal("nil pool must stay nil")
	}
}

func TestAlphaNetwork(t *testing.T) {
	in := &vpcpb.Network{
		Metadata: &commonpb.ResourceMetadata{Id: "vpcnetwork-1"},
		Spec: &vpcpb.NetworkSpec{
			Ipv4PrivatePools: &vpcpb.IPv4PrivateNetworkPools{Pools: []*vpcpb.NetworkPool{{Id: "vpcpool-1"}, {Id: "vpcpool-2"}}},
			Ipv4PublicPools:  &vpcpb.IPv4PublicNetworkPools{Pools: []*vpcpb.NetworkPool{{Id: "vpcpool-pub"}}},
		},
		Status: &vpcpb.NetworkStatus{State: vpcpb.NetworkStatus_READY, Default: true},
	}
	want := &vpcv1alpha1pb.Network{
		Metadata: in.GetMetadata(),
		Spec:     &vpcv1alpha1pb.NetworkSpec{Pools: []*vpcv1alpha1pb.NetworkPool{{PoolId: "vpcpool-1"}, {PoolId: "vpcpool-2"}}},
		Status:   &vpcv1alpha1pb.NetworkStatus{State: vpcv1alpha1pb.NetworkStatus_READY, Default: true, ScopeId: "vpcscope-1"},
	}
	if diff := cmpDiff(want, alphaNetwork(in, "vpcscope-1")); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

func TestAlphaSubnet(t *testing.T) {
	md := &commonpb.ResourceMetadata{Id: "vpcsubnet-1"}
	statusPools := []*vpcpb.SubnetStatusPool{
		{PoolId: "vpcpool-1", Cidrs: []string{"10.0.0.0/24"}},
		{PoolId: "vpcpool-2", Cidrs: []string{"10.1.0.0/24", "10.1.1.0/24"}},
	}
	wantStatus := &vpcv1alpha1pb.SubnetStatus{
		State: vpcv1alpha1pb.SubnetStatus_READY, Default: true,
		Ipv4Cidrs: []string{"10.0.0.0/24", "10.1.0.0/24", "10.1.1.0/24"},
	}
	tests := []struct {
		name string
		spec *vpcpb.SubnetSpec
		want *vpcv1alpha1pb.SubnetSpec
	}{
		{
			name: "network pools become pool ids from status",
			spec: &vpcpb.SubnetSpec{NetworkId: "vpcnetwork-1", Ipv4PrivatePools: &vpcpb.IPv4PrivateSubnetPools{UseNetworkPools: true}},
			want: &vpcv1alpha1pb.SubnetSpec{NetworkId: "vpcnetwork-1", Pools: []*vpcv1alpha1pb.SubnetPool{
				{Pool: &vpcv1alpha1pb.SubnetPool_PoolId{PoolId: "vpcpool-1"}},
				{Pool: &vpcv1alpha1pb.SubnetPool_PoolId{PoolId: "vpcpool-2"}},
			}},
		},
		{
			name: "own pools become inline specs",
			spec: &vpcpb.SubnetSpec{NetworkId: "vpcnetwork-1", Ipv4PrivatePools: &vpcpb.IPv4PrivateSubnetPools{
				Pools: []*vpcpb.SubnetPool{{Cidrs: []*vpcpb.SubnetCidr{{Cidr: "/24", State: vpcpb.AddressBlockState_AVAILABLE, MaxMaskLength: 28}}}},
			}},
			want: &vpcv1alpha1pb.SubnetSpec{NetworkId: "vpcnetwork-1", Pools: []*vpcv1alpha1pb.SubnetPool{
				{Pool: &vpcv1alpha1pb.SubnetPool_Spec{Spec: &vpcv1alpha1pb.SubnetPoolSpec{
					Version: vpcv1alpha1pb.IpVersion_IPV4,
					Cidrs:   []*vpcv1alpha1pb.SubnetCidr{{Cidr: "/24", State: vpcv1alpha1pb.PoolCidrState_AVAILABLE, AllowedMask: 28}},
				}}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := &vpcpb.Subnet{Metadata: md, Spec: tt.spec, Status: &vpcpb.SubnetStatus{
				State: vpcpb.SubnetStatus_READY, Default: true, Ipv4PrivatePools: statusPools,
			}}
			want := &vpcv1alpha1pb.Subnet{Metadata: md, Spec: tt.want, Status: wantStatus}
			if diff := cmpDiff(want, alphaSubnet(in)); diff != "" {
				t.Fatalf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAlphaAllocation(t *testing.T) {
	md := &commonpb.ResourceMetadata{Id: "vpcallocation-1"}
	tests := []struct {
		name string
		in   *vpcpb.Allocation
		want *vpcv1alpha1pb.Allocation
	}{
		{
			name: "private by subnet with empty cidr and nic assignment",
			in: &vpcpb.Allocation{
				Metadata: md,
				Spec: &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
					Pool: &vpcpb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: "vpcsubnet-1"},
				}}},
				Status: &vpcpb.AllocationStatus{
					State:   vpcpb.AllocationStatus_ASSIGNED,
					Details: &vpcpb.AllocationDetails{AllocatedCidr: "10.0.0.5/32", PoolId: "vpcpool-1", Version: vpcpb.IpVersion_IPV4, SubnetId: "vpcsubnet-1"},
					Assignment: &vpcpb.Assignment{Type: &vpcpb.Assignment_NetworkInterface{NetworkInterface: &vpcpb.NetworkInterfaceAssignment{
						InstanceId: "computeinstance-1", Name: "eth0", Type: vpcpb.NetworkInterfaceAssignment_PRIMARY,
					}}},
				},
			},
			want: &vpcv1alpha1pb.Allocation{
				Metadata: md,
				Spec: &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcv1alpha1pb.IPv4PrivateAllocationSpec{
					Pool: &vpcv1alpha1pb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: "vpcsubnet-1"},
				}}},
				Status: &vpcv1alpha1pb.AllocationStatus{
					State:   vpcv1alpha1pb.AllocationStatus_ASSIGNED,
					Details: &vpcv1alpha1pb.AllocationDetails{AllocatedCidr: "10.0.0.5/32", PoolId: "vpcpool-1", Version: vpcv1alpha1pb.IpVersion_IPV4},
					Assignment: &vpcv1alpha1pb.Assignment{Type: &vpcv1alpha1pb.Assignment_NetworkInterface{NetworkInterface: &vpcv1alpha1pb.NetworkInterfaceAssignment{
						InstanceId: "computeinstance-1", Name: "eth0", Type: vpcv1alpha1pb.NetworkInterfaceAssignment_PRIMARY,
					}}},
				},
			},
		},
		{
			name: "public by pool keeps cidr, load balancer, static",
			in: &vpcpb.Allocation{
				Metadata: md,
				Spec: &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{
					Cidr: "/32", Pool: &vpcpb.IPv4PublicAllocationSpec_PoolId{PoolId: "vpcpool-pub"},
				}}},
				Status: &vpcpb.AllocationStatus{
					State: vpcpb.AllocationStatus_ASSIGNED, Static: true,
					Assignment: &vpcpb.Assignment{Type: &vpcpb.Assignment_LoadBalancer{LoadBalancer: &vpcpb.LoadBalancerAssignment{Id: "lb-1"}}},
				},
			},
			want: &vpcv1alpha1pb.Allocation{
				Metadata: md,
				Spec: &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcv1alpha1pb.IPv4PublicAllocationSpec{
					Cidr: "/32", PoolId: "vpcpool-pub",
				}}},
				Status: &vpcv1alpha1pb.AllocationStatus{
					State: vpcv1alpha1pb.AllocationStatus_ASSIGNED, Static: true,
					Assignment: &vpcv1alpha1pb.Assignment{Type: &vpcv1alpha1pb.Assignment_LoadBalancer{LoadBalancer: &vpcv1alpha1pb.LoadBalancerAssignment{Id: "lb-1"}}},
				},
			},
		},
		{
			name: "public by subnet drops the subnet",
			in: &vpcpb.Allocation{
				Metadata: md,
				Spec: &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{
					Pool: &vpcpb.IPv4PublicAllocationSpec_SubnetId{SubnetId: "vpcsubnet-1"},
				}}},
			},
			want: &vpcv1alpha1pb.Allocation{
				Metadata: md,
				Spec:     &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcv1alpha1pb.IPv4PublicAllocationSpec{}}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmpDiff(tt.want, alphaAllocation(tt.in)); diff != "" {
				t.Fatalf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestV1AllocationSpec(t *testing.T) {
	tests := []struct {
		name string
		in   *vpcv1alpha1pb.AllocationSpec
		want *vpcpb.AllocationSpec
	}{
		{name: "nil stays nil"},
		{
			name: "private by pool",
			in: &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcv1alpha1pb.IPv4PrivateAllocationSpec{
				Cidr: "/28", Pool: &vpcv1alpha1pb.IPv4PrivateAllocationSpec_PoolId{PoolId: "vpcpool-1"},
			}}},
			want: &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
				Cidr: "/28", Pool: &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: "vpcpool-1"},
			}}},
		},
		{
			name: "private by subnet",
			in: &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcv1alpha1pb.IPv4PrivateAllocationSpec{
				Pool: &vpcv1alpha1pb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: "vpcsubnet-1"},
			}}},
			want: &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
				Pool: &vpcpb.IPv4PrivateAllocationSpec_SubnetId{SubnetId: "vpcsubnet-1"},
			}}},
		},
		{
			name: "public with pool",
			in: &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcv1alpha1pb.IPv4PublicAllocationSpec{
				Cidr: "/32", PoolId: "vpcpool-pub",
			}}},
			want: &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{
				Cidr: "/32", Pool: &vpcpb.IPv4PublicAllocationSpec_PoolId{PoolId: "vpcpool-pub"},
			}}},
		},
		{
			name: "public without pool leaves the oneof unset",
			in:   &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcv1alpha1pb.IPv4PublicAllocationSpec{}}},
			want: &vpcpb.AllocationSpec{IpSpec: &vpcpb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcpb.IPv4PublicAllocationSpec{}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmpDiff(tt.want, v1AllocationSpec(tt.in)); diff != "" {
				t.Fatalf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func cmpDiff(want, got any) string {
	return cmp.Diff(want, got, protocmp.Transform())
}
