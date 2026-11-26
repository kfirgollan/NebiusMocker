// Package api is the Applications gRPC view layer: it converts between
// protos and domain inputs and maps domain errors. It makes no business
// decisions.
package api

import (
	"context"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/applications/domain"
)

// ReleaseHandler implements nebius.applications.v1alpha1.K8sReleaseService.
type ReleaseHandler struct {
	applicationsv1alpha1pb.UnimplementedK8SReleaseServiceServer
	svc *domain.Service
}

var _ applicationsv1alpha1pb.K8SReleaseServiceServer = (*ReleaseHandler)(nil)

// NewReleaseHandler returns a handler backed by svc.
func NewReleaseHandler(svc *domain.Service) *ReleaseHandler { return &ReleaseHandler{svc: svc} }

// Get returns a release by ID.
func (h *ReleaseHandler) Get(ctx context.Context, req *applicationsv1alpha1pb.GetK8SReleaseRequest) (*applicationsv1alpha1pb.K8SRelease, error) {
	r, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return r, nil
}

// List returns one page of the releases installed into a cluster.
func (h *ReleaseHandler) List(ctx context.Context, req *applicationsv1alpha1pb.ListK8SReleasesRequest) (*applicationsv1alpha1pb.ListK8SReleasesResponse, error) {
	page, err := h.svc.List(ctx, domain.ListCmd{
		ParentID: req.GetParentId(), ClusterID: req.GetClusterId(),
		PageSize: req.GetPageSize(), PageToken: req.GetPageToken(), Filter: req.GetFilter(),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &applicationsv1alpha1pb.ListK8SReleasesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a release creation operation.
func (h *ReleaseHandler) Create(ctx context.Context, req *applicationsv1alpha1pb.CreateK8SReleaseRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a release update operation.
func (h *ReleaseHandler) Update(ctx context.Context, req *applicationsv1alpha1pb.UpdateK8SReleaseRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Update(ctx, domain.UpdateCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a release deletion operation.
func (h *ReleaseHandler) Delete(ctx context.Context, req *applicationsv1alpha1pb.DeleteK8SReleaseRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
