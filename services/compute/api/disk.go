package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

const (
	diskService     = "nebius.compute.v1.DiskService"
	snapshotService = "nebius.compute.v1.DiskSnapshotService"
)

// RegisterDiskAuthzRules declares the permission each DiskService and
// DiskSnapshotService RPC needs (compute.disks.* and compute.disksnapshots.*
// on the project).
func RegisterDiskAuthzRules(r *authz.Rules) {
	r.CRUD(diskService, "compute", "disks")
	r.Add("/"+diskService+"/ListOperationsByParent", authz.OnParent(authz.Perm("compute", "disks", authz.VerbList)))

	r.CRUD(snapshotService, "compute", "disksnapshots")
	r.Add("/"+snapshotService+"/ListByDisk", authz.Rule{
		Permission: authz.Perm("compute", "disksnapshots", authz.VerbList),
		Target: func(_ context.Context, req any) (string, error) {
			if g, ok := req.(*computepb.ListDiskSnapshotsByDiskRequest); ok && g.GetDiskId() != "" {
				return g.GetDiskId(), nil
			}
			return "", errs.BadRequest(errs.Violation{Field: "disk_id", Message: "required"})
		},
	})
}

// resetMask reads the X-ResetMask header of an Update.
func resetMask(ctx context.Context) (*mask.Mask, error) {
	m, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()})
	}
	return m, nil
}

// DiskHandler implements nebius.compute.v1.DiskService.
type DiskHandler struct {
	computepb.UnimplementedDiskServiceServer
	svc *domain.DiskService
}

var _ computepb.DiskServiceServer = (*DiskHandler)(nil)

// NewDiskHandler returns a handler backed by svc.
func NewDiskHandler(svc *domain.DiskService) *DiskHandler { return &DiskHandler{svc: svc} }

// Get returns a disk by ID.
func (h *DiskHandler) Get(ctx context.Context, req *computepb.GetDiskRequest) (*computepb.Disk, error) {
	d, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return d, nil
}

// GetByName returns a disk by parent and name.
func (h *DiskHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*computepb.Disk, error) {
	d, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return d, nil
}

// List returns one page of a project's disks.
func (h *DiskHandler) List(ctx context.Context, req *computepb.ListDisksRequest) (*computepb.ListDisksResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter()) //nolint:staticcheck // the deprecated filter is read only to reject it
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListDisksResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a disk creation operation.
func (h *DiskHandler) Create(ctx context.Context, req *computepb.CreateDiskRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateDiskCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a disk update operation; X-ResetMask selects fields to clear.
func (h *DiskHandler) Update(ctx context.Context, req *computepb.UpdateDiskRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateDiskCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a disk deletion operation.
func (h *DiskHandler) Delete(ctx context.Context, req *computepb.DeleteDiskRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// ListOperationsByParent lists the operations on a project's disks.
func (h *DiskHandler) ListOperationsByParent(ctx context.Context, req *computepb.ListOperationsByParentRequest) (*commonpb.ListOperationsResponse, error) {
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
