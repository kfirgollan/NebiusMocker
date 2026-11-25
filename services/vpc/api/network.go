package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
)

// GetDefault returns the project's default network and default subnet.
func (h *NetworkHandler) GetDefault(ctx context.Context, req *vpcpb.GetDefaultNetworkRequest) (*vpcpb.GetDefaultNetworkResponse, error) {
	d, err := h.svc.GetDefault(ctx, req.GetParentId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &vpcpb.GetDefaultNetworkResponse{Network: d.Network, Subnet: d.Subnet}, nil
}

// CreateDefault starts creating the project's default network and subnet.
func (h *NetworkHandler) CreateDefault(ctx context.Context, req *vpcpb.CreateDefaultNetworkRequest) (*commonpb.Operation, error) {
	return opReply(ctx)(h.svc.CreateDefault(ctx, req.GetMetadata(), req))
}
