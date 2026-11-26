package interceptors

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	billingv1alpha1pb "github.com/nebius/gosdk/proto/nebius/billing/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"
)

func TestValidatePartialUpdates(t *testing.T) {
	t.Parallel()
	validate, err := Validate()
	if err != nil {
		t.Fatal(err)
	}
	ok := func(context.Context, any) (any, error) { return "ok", nil }
	tests := []struct {
		name   string
		method string
		req    any
		reset  string // X-ResetMask of the call, if any
		want   codes.Code
	}{
		{
			name: "create without source is rejected", method: "/nebius.compute.v1.ImageService/Create",
			req:  &computepb.CreateImageRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00x"}, Spec: &computepb.ImageSpec{}},
			want: codes.InvalidArgument,
		},
		{
			// What `nebius compute image update --id X --description d` sends.
			name: "patch update without parent and source passes", method: "/nebius.compute.v1.ImageService/Update",
			req:  &computepb.UpdateImageRequest{Metadata: &commonpb.ResourceMetadata{Id: "computeimage-e00x"}, Spec: &computepb.ImageSpec{Description: "d"}},
			want: codes.OK,
		},
		{
			name: "update still checks value rules", method: "/nebius.compute.v1.ImageService/Update",
			req:  &computepb.UpdateImageRequest{Metadata: &commonpb.ResourceMetadata{Id: "computeimage-e00x"}, Spec: &computepb.ImageSpec{Description: strings.Repeat("x", 2000)}},
			want: codes.InvalidArgument,
		},
		{
			// What `nebius vpc v1alpha1 allocation update --id X --labels k=v` sends:
			// no name and no X-ResetMask, so the name is kept.
			name: "patch update without name passes the name format rule", method: "/nebius.vpc.v1alpha1.AllocationService/Update",
			req:  &vpcv1alpha1pb.UpdateAllocationRequest{Metadata: &commonpb.ResourceMetadata{Id: "vpcallocation-e00x", Labels: map[string]string{"k": "v"}}},
			want: codes.OK,
		},
		{
			name: "update with a malformed name is rejected", method: "/nebius.vpc.v1.AllocationService/Update",
			req:  &vpcpb.UpdateAllocationRequest{Metadata: &commonpb.ResourceMetadata{Id: "vpcallocation-e00x", Name: "-bad"}},
			want: codes.InvalidArgument,
		},
		{
			name: "update clearing the name through the reset mask is rejected", method: "/nebius.vpc.v1.AllocationService/Update",
			req:   &vpcpb.UpdateAllocationRequest{Metadata: &commonpb.ResourceMetadata{Id: "vpcallocation-e00x"}},
			reset: "metadata.name",
			want:  codes.InvalidArgument,
		},
		{
			// What `nebius mk8s node-group update --id X --fixed-node-count 1` previews first.
			name: "patch preflight check without parent and template passes", method: "/nebius.mk8s.v1.NodeGroupService/PreflightCheck",
			req: &mk8spb.PreflightCheckNodeGroupRequest{
				Context:  &commonpb.PreflightCheckContext{Action: commonpb.PreflightCheckContext_UPDATE},
				Metadata: &commonpb.ResourceMetadata{Id: "mk8snodegroup-e00x"},
				Spec:     &mk8spb.NodeGroupSpec{Size: &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: 1}},
			},
			want: codes.OK,
		},
		{
			// The documented `nebius billing v1alpha1 calculator estimate`: no boot disk, no NIC.
			name: "calculator estimate of a partial instance spec passes", method: "/nebius.billing.v1alpha1.CalculatorService/Estimate",
			req: &billingv1alpha1pb.EstimateRequest{ResourceSpec: &billingv1alpha1pb.ResourceSpec{
				ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeInstanceSpec{ComputeInstanceSpec: &computepb.CreateInstanceRequest{
					Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00x"},
					Spec: &computepb.InstanceSpec{
						Resources: &computepb.ResourcesSpec{Platform: "gpu-h100-sxm", Size: &computepb.ResourcesSpec_Preset{Preset: "1gpu-16vcpu-200gb"}},
					},
				}},
			}},
			want: codes.OK,
		},
		{
			name: "the same instance spec is rejected by compute's Create", method: "/nebius.compute.v1.InstanceService/Create",
			req: &computepb.CreateInstanceRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00x", Name: "vm"},
				Spec: &computepb.InstanceSpec{
					Resources: &computepb.ResourcesSpec{Platform: "gpu-h100-sxm", Size: &computepb.ResourcesSpec_Preset{Preset: "1gpu-16vcpu-200gb"}},
				},
			},
			want: codes.InvalidArgument,
		},
		{
			name: "create without a name is rejected", method: "/nebius.vpc.v1alpha1.AllocationService/Create",
			req: &vpcv1alpha1pb.CreateAllocationRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00x"},
				Spec:     &vpcv1alpha1pb.AllocationSpec{IpSpec: &vpcv1alpha1pb.AllocationSpec_Ipv4Public{Ipv4Public: &vpcv1alpha1pb.IPv4PublicAllocationSpec{}}},
			},
			want: codes.InvalidArgument,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tt.reset != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(grpcheader.ResetMask, tt.reset))
			}
			_, err := validate(ctx, tt.req, &grpc.UnaryServerInfo{FullMethod: tt.method}, ok)
			if got := status.Code(err); got != tt.want {
				t.Fatalf("Validate(%s) code = %v, want %v (err: %v)", tt.method, got, tt.want, err)
			}
		})
	}
}
