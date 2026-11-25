// Package api is the Compute gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// ImageHandler implements nebius.compute.v1.ImageService.
type ImageHandler struct {
	computepb.UnimplementedImageServiceServer
	svc *domain.ImageService
}

var _ computepb.ImageServiceServer = (*ImageHandler)(nil)

// NewImageHandler returns a handler backed by svc.
func NewImageHandler(svc *domain.ImageService) *ImageHandler { return &ImageHandler{svc: svc} }

// Get returns an image by ID.
func (h *ImageHandler) Get(ctx context.Context, req *computepb.GetImageRequest) (*computepb.Image, error) {
	img, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return img, nil
}

// GetByName returns an image by parent and name.
func (h *ImageHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*computepb.Image, error) {
	img, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return img, nil
}

// GetLatestByFamily returns the newest non-deprecated image of a family.
func (h *ImageHandler) GetLatestByFamily(ctx context.Context, req *computepb.GetImageLatestByFamilyRequest) (*computepb.Image, error) {
	img, err := h.svc.GetLatestByFamily(ctx, req.GetImageFamily(), req.GetParentId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return img, nil
}

// List returns one page of a project's images.
func (h *ImageHandler) List(ctx context.Context, req *computepb.ListImagesRequest) (*computepb.ListImagesResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListImagesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListPublic returns one page of a region's public images.
func (h *ImageHandler) ListPublic(ctx context.Context, req *computepb.ListPublicRequest) (*computepb.ListImagesResponse, error) {
	page, err := h.svc.ListPublic(ctx, req.GetRegion(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListImagesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts an image creation (import) operation.
func (h *ImageHandler) Create(ctx context.Context, req *computepb.CreateImageRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateImageCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts an image update operation; X-ResetMask selects fields to clear.
func (h *ImageHandler) Update(ctx context.Context, req *computepb.UpdateImageRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.Update(ctx, domain.UpdateImageCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts an image deletion operation.
func (h *ImageHandler) Delete(ctx context.Context, req *computepb.DeleteImageRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// ListOperationsByParent lists the operations on a project's images.
func (h *ImageHandler) ListOperationsByParent(ctx context.Context, req *computepb.ListOperationsByParentRequest) (*commonpb.ListOperationsResponse, error) {
	page, err := h.svc.ListOperationsByParent(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	resp := &commonpb.ListOperationsResponse{NextPageToken: page.NextPageToken}
	for _, op := range page.Items {
		resp.Operations = append(resp.Operations, opsapi.ToProto(ctx, op))
	}
	return resp, nil
}
