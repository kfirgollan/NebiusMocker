package interceptors

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := validate(t.Context(), tt.req, &grpc.UnaryServerInfo{FullMethod: tt.method}, ok)
			if got := status.Code(err); got != tt.want {
				t.Fatalf("Validate(%s) code = %v, want %v (err: %v)", tt.method, got, tt.want, err)
			}
		})
	}
}
