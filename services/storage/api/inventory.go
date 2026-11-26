package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// RegisterInventoryAuthzRules declares the permission of every
// InventoryService RPC. The parent of an inventory is its source bucket.
func RegisterInventoryAuthzRules(r *authz.Rules) {
	r.CRUD("nebius.storage.v1.InventoryService", "storage", "inventories")
}

// InventoryHandler implements nebius.storage.v1.InventoryService.
type InventoryHandler struct {
	storagepb.UnimplementedInventoryServiceServer
	svc *domain.InventoryService
}

var _ storagepb.InventoryServiceServer = (*InventoryHandler)(nil)

// NewInventoryHandler returns a handler backed by svc.
func NewInventoryHandler(svc *domain.InventoryService) *InventoryHandler {
	return &InventoryHandler{svc: svc}
}

// Get returns an inventory by ID.
func (h *InventoryHandler) Get(ctx context.Context, req *storagepb.GetInventoryRequest) (*storagepb.Inventory, error) {
	inv, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return inv, nil
}

// GetByName returns an inventory by source bucket and name.
func (h *InventoryHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*storagepb.Inventory, error) {
	inv, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return inv, nil
}

// List returns one page of a bucket's inventories.
func (h *InventoryHandler) List(ctx context.Context, req *storagepb.ListInventoriesRequest) (*storagepb.ListInventoriesResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &storagepb.ListInventoriesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts an inventory creation operation.
func (h *InventoryHandler) Create(ctx context.Context, req *storagepb.CreateInventoryRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Create(ctx, domain.CreateInventoryCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts an inventory update operation; X-ResetMask selects fields to clear.
func (h *InventoryHandler) Update(ctx context.Context, req *storagepb.UpdateInventoryRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	return opResult(ctx)(h.svc.Update(ctx, domain.UpdateInventoryCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	}))
}

// Delete starts an inventory deletion operation.
func (h *InventoryHandler) Delete(ctx context.Context, req *storagepb.DeleteInventoryRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}
