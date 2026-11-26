package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// NodeGroupHandler implements nebius.mk8s.v1.NodeGroupService.
type NodeGroupHandler struct {
	mk8spb.UnimplementedNodeGroupServiceServer
	svc *domain.Service
}

var _ mk8spb.NodeGroupServiceServer = (*NodeGroupHandler)(nil)

// NewNodeGroupHandler returns a handler backed by svc.
func NewNodeGroupHandler(svc *domain.Service) *NodeGroupHandler { return &NodeGroupHandler{svc: svc} }

// Get returns a node group by ID (resource_version is deprecated and ignored).
func (h *NodeGroupHandler) Get(ctx context.Context, req *mk8spb.GetNodeGroupRequest) (*mk8spb.NodeGroup, error) {
	g, err := h.svc.GetNodeGroup(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return g, nil
}

// GetByName returns a node group by cluster and name.
func (h *NodeGroupHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*mk8spb.NodeGroup, error) {
	g, err := h.svc.GetNodeGroupByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return g, nil
}

// List returns one page of a cluster's node groups.
func (h *NodeGroupHandler) List(ctx context.Context, req *mk8spb.ListNodeGroupsRequest) (*mk8spb.ListNodeGroupsResponse, error) {
	page, err := h.svc.ListNodeGroups(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &mk8spb.ListNodeGroupsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a node group creation operation.
func (h *NodeGroupHandler) Create(ctx context.Context, req *mk8spb.CreateNodeGroupRequest) (*commonpb.Operation, error) {
	op, err := h.svc.CreateNodeGroup(ctx, domain.CreateNodeGroupCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a node group update operation; X-ResetMask selects fields to clear.
func (h *NodeGroupHandler) Update(ctx context.Context, req *mk8spb.UpdateNodeGroupRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.UpdateNodeGroup(ctx, domain.UpdateNodeGroupCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a node group deletion operation.
func (h *NodeGroupHandler) Delete(ctx context.Context, req *mk8spb.DeleteNodeGroupRequest) (*commonpb.Operation, error) {
	op, err := h.svc.DeleteNodeGroup(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Upgrade starts an upgrade to the latest node image (the only upgrade type).
func (h *NodeGroupHandler) Upgrade(ctx context.Context, req *mk8spb.UpgradeNodeGroupRequest) (*commonpb.Operation, error) {
	op, err := h.svc.UpgradeNodeGroup(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// PreflightCheck evaluates a create or update without changing anything.
func (h *NodeGroupHandler) PreflightCheck(ctx context.Context, req *mk8spb.PreflightCheckNodeGroupRequest) (*mk8spb.PreflightCheckNodeGroupResponse, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	res, err := h.svc.PreflightCheckNodeGroup(ctx, domain.PreflightCmd{
		Context: req.GetContext(), Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &mk8spb.PreflightCheckNodeGroupResponse{Diagnostics: res.Diagnostics, PathsRequireRecreate: res.PathsRequireRecreate}, nil
}

// GetCompatibilityMatrix lists the node images a cluster version supports.
func (h *NodeGroupHandler) GetCompatibilityMatrix(ctx context.Context, req *mk8spb.GetNodeGroupCompatibilityMatrixRequest) (*mk8spb.NodeGroupCompatibilityMatrix, error) {
	m, err := h.svc.CompatibilityMatrix(req.GetClusterKubernetesVersion(), req.GetPlatform())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return m, nil
}
