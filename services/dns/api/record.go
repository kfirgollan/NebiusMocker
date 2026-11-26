package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
)

// RecordHandler implements nebius.dns.v1.RecordService.
type RecordHandler struct {
	dnspb.UnimplementedRecordServiceServer
	svc *domain.Service
}

var _ dnspb.RecordServiceServer = (*RecordHandler)(nil)

// NewRecordHandler returns a handler backed by svc.
func NewRecordHandler(svc *domain.Service) *RecordHandler { return &RecordHandler{svc: svc} }

// Get returns a record by ID.
func (h *RecordHandler) Get(ctx context.Context, req *dnspb.GetRecordRequest) (*dnspb.Record, error) {
	r, err := h.svc.GetRecord(ctx, req.GetId(), req.GetResourceVersion())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return r, nil
}

// GetByName returns a record by zone and resource name.
func (h *RecordHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*dnspb.Record, error) {
	r, err := h.svc.GetRecordByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return r, nil
}

// List returns one page of a zone's records.
func (h *RecordHandler) List(ctx context.Context, req *dnspb.ListRecordsRequest) (*dnspb.ListRecordsResponse, error) {
	page, err := h.svc.ListRecords(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &dnspb.ListRecordsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a record creation operation.
func (h *RecordHandler) Create(ctx context.Context, req *dnspb.CreateRecordRequest) (*commonpb.Operation, error) {
	op, err := h.svc.CreateRecord(ctx, domain.CreateRecordCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a record update operation; X-ResetMask selects fields to clear.
func (h *RecordHandler) Update(ctx context.Context, req *dnspb.UpdateRecordRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.UpdateRecord(ctx, domain.UpdateRecordCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a record deletion operation; force deletes a protected record.
func (h *RecordHandler) Delete(ctx context.Context, req *dnspb.DeleteRecordRequest) (*commonpb.Operation, error) {
	op, err := h.svc.DeleteRecord(ctx, domain.DeleteRecordCmd{
		ID: req.GetId(), ResourceVersion: req.GetResourceVersion(), Force: req.GetForce(), Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
