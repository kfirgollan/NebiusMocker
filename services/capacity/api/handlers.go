package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/capacity/domain"
)

// AllowanceHandler implements nebius.capacity.v1.CapacityAllowanceService.
type AllowanceHandler struct {
	capacitypb.UnimplementedCapacityAllowanceServiceServer
	svc *domain.Service
}

var _ capacitypb.CapacityAllowanceServiceServer = (*AllowanceHandler)(nil)

// NewAllowanceHandler returns a handler backed by svc.
func NewAllowanceHandler(svc *domain.Service) *AllowanceHandler { return &AllowanceHandler{svc: svc} }

// Get returns an allowance by ID.
func (h *AllowanceHandler) Get(ctx context.Context, req *capacitypb.GetCapacityAllowanceRequest) (*capacitypb.CapacityAllowance, error) {
	a, err := h.svc.GetAllowance(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return a, nil
}

// GetByParentAndCapacityBlockGroup returns a project's allowance for a
// block group, or the default one.
func (h *AllowanceHandler) GetByParentAndCapacityBlockGroup(ctx context.Context, req *capacitypb.GetCapacityAllowanceByParentAndCapacityBlockGroupRequest) (*capacitypb.CapacityAllowance, error) {
	a, err := h.svc.GetAllowanceByGroup(ctx, req.GetParentId(), req.GetCapacityBlockGroupId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return a, nil
}

// List returns one page of a project's allowances.
func (h *AllowanceHandler) List(ctx context.Context, req *capacitypb.ListCapacityAllowancesRequest) (*capacitypb.ListCapacityAllowancesResponse, error) {
	page, err := h.svc.ListAllowances(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &capacitypb.ListCapacityAllowancesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListByCapacityBlockGroup returns one page of a block group's allowances.
func (h *AllowanceHandler) ListByCapacityBlockGroup(ctx context.Context, req *capacitypb.ListCapacityAllowancesByCapacityBlockGroupRequest) (*capacitypb.ListCapacityAllowancesResponse, error) {
	page, err := h.svc.ListAllowancesByGroup(ctx, req.GetCapacityBlockGroupId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &capacitypb.ListCapacityAllowancesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts an allowance creation operation.
func (h *AllowanceHandler) Create(ctx context.Context, req *capacitypb.CreateCapacityAllowanceRequest) (*commonpb.Operation, error) {
	op, err := h.svc.CreateAllowance(ctx, domain.CreateAllowanceCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts an allowance update operation; X-ResetMask selects fields
// to clear.
func (h *AllowanceHandler) Update(ctx context.Context, req *capacitypb.UpdateCapacityAllowanceRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.UpdateAllowance(ctx, domain.UpdateAllowanceCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts an allowance deletion operation.
func (h *AllowanceHandler) Delete(ctx context.Context, req *capacitypb.DeleteCapacityAllowanceRequest) (*commonpb.Operation, error) {
	op, err := h.svc.DeleteAllowance(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// BlockGroupHandler implements nebius.capacity.v1.CapacityBlockGroupService.
type BlockGroupHandler struct {
	capacitypb.UnimplementedCapacityBlockGroupServiceServer
	svc *domain.Service
}

var _ capacitypb.CapacityBlockGroupServiceServer = (*BlockGroupHandler)(nil)

// NewBlockGroupHandler returns a handler backed by svc.
func NewBlockGroupHandler(svc *domain.Service) *BlockGroupHandler {
	return &BlockGroupHandler{svc: svc}
}

// Get returns a block group by ID.
func (h *BlockGroupHandler) Get(ctx context.Context, req *capacitypb.GetCapacityBlockGroupRequest) (*capacitypb.CapacityBlockGroup, error) {
	g, err := h.svc.GetBlockGroup(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return g, nil
}

// GetByResourceAffinity returns the tenant's block group for a region and
// resource affinity.
func (h *BlockGroupHandler) GetByResourceAffinity(ctx context.Context, req *capacitypb.GetCapacityBlockGroupByResourceAffinityRequest) (*capacitypb.CapacityBlockGroup, error) {
	cv1 := req.GetResourceAffinity().GetComputeV1()
	if cv1 == nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: "resource_affinity.compute_v1", Message: "required"}))
	}
	g, err := h.svc.GetBlockGroupByAffinity(ctx, domain.Affinity{
		TenantID: req.GetParentId(), Region: req.GetRegion(), Platform: cv1.GetPlatform(), Fabric: cv1.GetFabric(),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return g, nil
}

// List returns one page of a tenant's block groups.
func (h *BlockGroupHandler) List(ctx context.Context, req *capacitypb.ListCapacityBlockGroupsRequest) (*capacitypb.ListCapacityBlockGroupsResponse, error) {
	page, err := h.svc.ListBlockGroups(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &capacitypb.ListCapacityBlockGroupsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListResources returns the IDs of the instances holding the block group's
// capacity.
func (h *BlockGroupHandler) ListResources(ctx context.Context, req *capacitypb.ListCapacityBlockGroupResourcesRequest) (*capacitypb.ListCapacityBlockGroupResourcesResponse, error) {
	ids, err := h.svc.ListBlockGroupResources(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &capacitypb.ListCapacityBlockGroupResourcesResponse{ResourceIds: ids}, nil
}

// IntervalHandler implements nebius.capacity.v1.CapacityIntervalService.
type IntervalHandler struct {
	capacitypb.UnimplementedCapacityIntervalServiceServer
	svc *domain.Service
}

var _ capacitypb.CapacityIntervalServiceServer = (*IntervalHandler)(nil)

// NewIntervalHandler returns a handler backed by svc.
func NewIntervalHandler(svc *domain.Service) *IntervalHandler { return &IntervalHandler{svc: svc} }

// Get returns an interval by ID.
func (h *IntervalHandler) Get(ctx context.Context, req *capacitypb.GetCapacityIntervalRequest) (*capacitypb.CapacityInterval, error) {
	iv, err := h.svc.GetInterval(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return iv, nil
}

// List returns one page of a block group's intervals.
func (h *IntervalHandler) List(ctx context.Context, req *capacitypb.ListCapacityIntervalsRequest) (*capacitypb.ListCapacityIntervalsResponse, error) {
	page, err := h.svc.ListIntervals(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &capacitypb.ListCapacityIntervalsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// AdviceHandler implements nebius.capacity.v1.ResourceAdviceService.
type AdviceHandler struct {
	capacitypb.UnimplementedResourceAdviceServiceServer
	svc *domain.Service
}

var _ capacitypb.ResourceAdviceServiceServer = (*AdviceHandler)(nil)

// NewAdviceHandler returns a handler backed by svc.
func NewAdviceHandler(svc *domain.Service) *AdviceHandler { return &AdviceHandler{svc: svc} }

// List returns one page of the tenant's resource advice.
func (h *AdviceHandler) List(ctx context.Context, req *capacitypb.ListResourceAdviceRequest) (*capacitypb.ListResourceAdviceResponse, error) {
	page, err := h.svc.ListResourceAdvice(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &capacitypb.ListResourceAdviceResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}
