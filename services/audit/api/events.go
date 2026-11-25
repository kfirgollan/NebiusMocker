// Package api is the audit gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

// EventHandler implements nebius.audit.v2.AuditEventService.
type EventHandler struct {
	auditv2pb.UnimplementedAuditEventServiceServer
	svc *domain.EventService
}

var _ auditv2pb.AuditEventServiceServer = (*EventHandler)(nil)

// NewEventHandler returns a handler backed by svc.
func NewEventHandler(svc *domain.EventService) *EventHandler { return &EventHandler{svc: svc} }

// List returns one page of audit events.
func (h *EventHandler) List(ctx context.Context, req *auditv2pb.ListAuditEventRequest) (*auditv2pb.ListAuditEventResponse, error) {
	items, next, err := h.svc.List(ctx, domain.ListEventsQuery{
		ParentID:  req.GetParentId(),
		Region:    req.GetRegion(),
		Start:     req.GetStart().AsTime(),
		End:       req.GetEnd().AsTime(),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
		Filter:    req.GetFilter(),
		EventType: req.GetEventType(),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &auditv2pb.ListAuditEventResponse{Items: items, NextPageToken: next}, nil
}

// ExportHandler implements nebius.audit.v2.AuditEventExportService.
type ExportHandler struct {
	auditv2pb.UnimplementedAuditEventExportServiceServer
	svc *domain.ExportService
}

var _ auditv2pb.AuditEventExportServiceServer = (*ExportHandler)(nil)

// NewExportHandler returns a handler backed by svc.
func NewExportHandler(svc *domain.ExportService) *ExportHandler { return &ExportHandler{svc: svc} }

// Start starts an export operation.
func (h *ExportHandler) Start(ctx context.Context, req *auditv2pb.StartRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Start(ctx, domain.StartExportCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), Region: req.GetRegion(), Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Cancel starts a cancel operation for a running export.
func (h *ExportHandler) Cancel(ctx context.Context, req *auditv2pb.CancelRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Cancel(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Get returns an export by ID.
func (h *ExportHandler) Get(ctx context.Context, req *auditv2pb.GetRequest) (*auditv2pb.AuditEventExport, error) {
	e, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return e, nil
}

// List returns one page of a parent's exports.
func (h *ExportHandler) List(ctx context.Context, req *auditv2pb.ListRequest) (*auditv2pb.ListAuditEventExportResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetRegion(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &auditv2pb.ListAuditEventExportResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}
