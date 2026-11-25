package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// SnapshotHandler implements nebius.compute.v1.DiskSnapshotService.
type SnapshotHandler struct {
	computepb.UnimplementedDiskSnapshotServiceServer
	svc *domain.SnapshotService
}

var _ computepb.DiskSnapshotServiceServer = (*SnapshotHandler)(nil)

// NewSnapshotHandler returns a handler backed by svc.
func NewSnapshotHandler(svc *domain.SnapshotService) *SnapshotHandler {
	return &SnapshotHandler{svc: svc}
}

// Get returns a snapshot by ID.
func (h *SnapshotHandler) Get(ctx context.Context, req *computepb.GetDiskSnapshotRequest) (*computepb.DiskSnapshot, error) {
	s, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return s, nil
}

// GetByName returns a snapshot by parent and name.
func (h *SnapshotHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*computepb.DiskSnapshot, error) {
	s, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return s, nil
}

// List returns one page of a project's snapshots.
func (h *SnapshotHandler) List(ctx context.Context, req *computepb.ListDiskSnapshotsRequest) (*computepb.ListDiskSnapshotsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListDiskSnapshotsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListByDisk returns one page of the snapshots of a disk.
func (h *SnapshotHandler) ListByDisk(ctx context.Context, req *computepb.ListDiskSnapshotsByDiskRequest) (*computepb.ListDiskSnapshotsByDiskResponse, error) {
	page, err := h.svc.ListByDisk(ctx, req.GetDiskId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListDiskSnapshotsByDiskResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a snapshot creation operation.
func (h *SnapshotHandler) Create(ctx context.Context, req *computepb.CreateDiskSnapshotRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateSnapshotCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a snapshot update operation; X-ResetMask selects fields to clear.
func (h *SnapshotHandler) Update(ctx context.Context, req *computepb.UpdateDiskSnapshotRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateSnapshotCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a snapshot deletion operation.
func (h *SnapshotHandler) Delete(ctx context.Context, req *computepb.DeleteDiskSnapshotRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
