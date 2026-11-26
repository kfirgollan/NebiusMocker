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

// ZoneHandler implements nebius.dns.v1.ZoneService.
type ZoneHandler struct {
	dnspb.UnimplementedZoneServiceServer
	svc *domain.Service
}

var _ dnspb.ZoneServiceServer = (*ZoneHandler)(nil)

// NewZoneHandler returns a handler backed by svc.
func NewZoneHandler(svc *domain.Service) *ZoneHandler { return &ZoneHandler{svc: svc} }

// Get returns a zone by ID.
func (h *ZoneHandler) Get(ctx context.Context, req *dnspb.GetZoneRequest) (*dnspb.Zone, error) {
	z, err := h.svc.GetZone(ctx, req.GetId(), req.GetResourceVersion())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return z, nil
}

// GetByName returns a zone by project and name.
func (h *ZoneHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*dnspb.Zone, error) {
	z, err := h.svc.GetZoneByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return z, nil
}

// List returns one page of a project's zones.
func (h *ZoneHandler) List(ctx context.Context, req *dnspb.ListZonesRequest) (*dnspb.ListZonesResponse, error) {
	page, err := h.svc.ListZones(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &dnspb.ListZonesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListByNetwork returns one page of the zones bound to a network.
func (h *ZoneHandler) ListByNetwork(ctx context.Context, req *dnspb.ListZonesByNetworkRequest) (*dnspb.ListZonesResponse, error) {
	page, err := h.svc.ListZonesByNetwork(ctx, req.GetNetworkId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &dnspb.ListZonesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a zone creation operation.
func (h *ZoneHandler) Create(ctx context.Context, req *dnspb.CreateZoneRequest) (*commonpb.Operation, error) {
	op, err := h.svc.CreateZone(ctx, domain.CreateZoneCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a zone update operation; X-ResetMask selects fields to clear.
func (h *ZoneHandler) Update(ctx context.Context, req *dnspb.UpdateZoneRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.UpdateZone(ctx, domain.UpdateZoneCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a zone deletion operation.
func (h *ZoneHandler) Delete(ctx context.Context, req *dnspb.DeleteZoneRequest) (*commonpb.Operation, error) {
	op, err := h.svc.DeleteZone(ctx, req.GetId(), req.GetResourceVersion(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
