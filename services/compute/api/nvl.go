package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

const nvlInstanceGroupService = "nebius.compute.v1.NVLInstanceGroupService"

// RegisterNVLInstanceGroupAuthzRules declares the permission each
// NVLInstanceGroupService RPC needs (compute.nvlInstanceGroups.* on the
// project).
func RegisterNVLInstanceGroupAuthzRules(r *authz.Rules) {
	r.CRUD(nvlInstanceGroupService, "compute", "nvlInstanceGroups")
}

// NVLInstanceGroupHandler implements nebius.compute.v1.NVLInstanceGroupService.
type NVLInstanceGroupHandler struct {
	computepb.UnimplementedNVLInstanceGroupServiceServer
	svc *domain.NVLInstanceGroupService
}

var _ computepb.NVLInstanceGroupServiceServer = (*NVLInstanceGroupHandler)(nil)

// NewNVLInstanceGroupHandler returns a handler backed by svc.
func NewNVLInstanceGroupHandler(svc *domain.NVLInstanceGroupService) *NVLInstanceGroupHandler {
	return &NVLInstanceGroupHandler{svc: svc}
}

// Get returns an NVL instance group by ID.
func (h *NVLInstanceGroupHandler) Get(ctx context.Context, req *computepb.GetNVLInstanceGroupRequest) (*computepb.NVLInstanceGroup, error) {
	g, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return g, nil
}

// GetByName returns an NVL instance group by parent and name.
func (h *NVLInstanceGroupHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*computepb.NVLInstanceGroup, error) {
	g, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return g, nil
}

// List returns one page of a project's NVL instance groups.
func (h *NVLInstanceGroupHandler) List(ctx context.Context, req *computepb.ListNVLInstanceGroupsRequest) (*computepb.ListNVLInstanceGroupsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListNVLInstanceGroupsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts an NVL instance group creation operation.
func (h *NVLInstanceGroupHandler) Create(ctx context.Context, req *computepb.CreateNVLInstanceGroupRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateNVLInstanceGroupCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts an NVL instance group update operation; X-ResetMask selects
// fields to clear.
func (h *NVLInstanceGroupHandler) Update(ctx context.Context, req *computepb.UpdateNVLInstanceGroupRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateNVLInstanceGroupCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts an NVL instance group deletion operation.
func (h *NVLInstanceGroupHandler) Delete(ctx context.Context, req *computepb.DeleteNVLInstanceGroupRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
