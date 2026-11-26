package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

// PricingPolicyHandler implements nebius.billing.v1.PricingPolicyService.
type PricingPolicyHandler struct {
	billingpb.UnimplementedPricingPolicyServiceServer
	svc *domain.PricingPolicyService
}

var _ billingpb.PricingPolicyServiceServer = (*PricingPolicyHandler)(nil)

// NewPricingPolicyHandler returns a handler backed by svc.
func NewPricingPolicyHandler(svc *domain.PricingPolicyService) *PricingPolicyHandler {
	return &PricingPolicyHandler{svc: svc}
}

// Get returns a pricing policy by ID.
func (h *PricingPolicyHandler) Get(ctx context.Context, req *billingpb.GetPricingPolicyRequest) (*billingpb.PricingPolicy, error) {
	p, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return p, nil
}

// GetByName returns a pricing policy by project and name.
func (h *PricingPolicyHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*billingpb.PricingPolicy, error) {
	p, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return p, nil
}

// List returns one page of a project's pricing policies.
func (h *PricingPolicyHandler) List(ctx context.Context, req *billingpb.ListPricingPoliciesRequest) (*billingpb.ListPricingPoliciesResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &billingpb.ListPricingPoliciesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a pricing policy creation operation.
func (h *PricingPolicyHandler) Create(ctx context.Context, req *billingpb.CreatePricingPolicyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreatePricingPolicyCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a pricing policy update operation; X-ResetMask selects fields to clear.
func (h *PricingPolicyHandler) Update(ctx context.Context, req *billingpb.UpdatePricingPolicyRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.Update(ctx, domain.UpdatePricingPolicyCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a pricing policy deletion operation.
func (h *PricingPolicyHandler) Delete(ctx context.Context, req *billingpb.DeletePricingPolicyRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
