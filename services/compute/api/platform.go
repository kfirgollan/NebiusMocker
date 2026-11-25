package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// PlatformHandler implements nebius.compute.v1.PlatformService.
type PlatformHandler struct {
	computepb.UnimplementedPlatformServiceServer
	svc *domain.PlatformService
}

var _ computepb.PlatformServiceServer = (*PlatformHandler)(nil)

// NewPlatformHandler returns a handler backed by svc.
func NewPlatformHandler(svc *domain.PlatformService) *PlatformHandler {
	return &PlatformHandler{svc: svc}
}

// GetByName returns a platform of the project's region by name.
func (h *PlatformHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*computepb.Platform, error) {
	p, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return p, nil
}

// List returns one page of the platforms offered in the project's region.
func (h *PlatformHandler) List(ctx context.Context, req *computepb.ListPlatformsRequest) (*computepb.ListPlatformsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListPlatformsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}
