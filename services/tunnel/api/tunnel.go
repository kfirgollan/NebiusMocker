// Package api is the Tunnels gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/domain"
)

// TunnelHandler implements nebius.tunnel.v1.TunnelService.
type TunnelHandler struct {
	tunnelpb.UnimplementedTunnelServiceServer
	svc *domain.Service
}

var _ tunnelpb.TunnelServiceServer = (*TunnelHandler)(nil)

// NewTunnelHandler returns a handler backed by svc.
func NewTunnelHandler(svc *domain.Service) *TunnelHandler { return &TunnelHandler{svc: svc} }

// Get returns a tunnel by ID.
func (h *TunnelHandler) Get(ctx context.Context, req *tunnelpb.GetTunnelRequest) (*tunnelpb.Tunnel, error) {
	t, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return t, nil
}

// List returns one page of the project's tunnels.
func (h *TunnelHandler) List(ctx context.Context, req *tunnelpb.ListTunnelRequest) (*tunnelpb.ListTunnelsResponse, error) {
	page, err := h.svc.List(ctx, domain.ListCmd{
		ParentID: req.GetParentId(), PageSize: req.GetPageSize(), PageToken: req.GetPageToken(), Filter: req.GetFilter(),
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &tunnelpb.ListTunnelsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a tunnel creation operation.
func (h *TunnelHandler) Create(ctx context.Context, req *tunnelpb.CreateTunnelRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a tunnel update operation; X-ResetMask selects fields to clear.
func (h *TunnelHandler) Update(ctx context.Context, req *tunnelpb.UpdateTunnelRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.Update(ctx, domain.UpdateCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a tunnel deletion operation.
func (h *TunnelHandler) Delete(ctx context.Context, req *tunnelpb.DeleteTunnelRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
