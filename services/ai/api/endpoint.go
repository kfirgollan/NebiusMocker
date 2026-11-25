// Package api is the AI gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

const endpointService = "nebius.ai.v1.EndpointService"

// VerbGetSensitiveData is the extra verb Get with view SECRET needs
// (resource.aiendpoint.getSensitiveData in the API docs). Only admins hold it
// in the default catalog: it matches neither the viewer nor the editor grants.
const VerbGetSensitiveData = "getSensitiveData"

// RegisterAuthzRules declares the permission of every EndpointService RPC:
// "ai.endpoints.<verb>" with get/list/create/delete plus start, stop and
// restart (granted to editors, see authz.DefaultCatalog), and
// getSensitiveData for Get with view SECRET.
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(endpointService, "ai", "endpoints")
	m := func(method string) string { return "/" + endpointService + "/" + method }
	get := authz.OnID(authz.Perm("ai", "endpoints", authz.VerbGet))
	get.Also = func(req any) authz.Permission {
		if gr, ok := req.(*aipb.GetEndpointRequest); ok && gr.GetView() == aipb.GetEndpointRequest_SECRET {
			return authz.Perm("ai", "endpoints", VerbGetSensitiveData)
		}
		return ""
	}
	r.Add(m("Get"), get)
	r.Add(m("Start"), authz.OnID(authz.Perm("ai", "endpoints", authz.VerbStart)))
	r.Add(m("Stop"), authz.OnID(authz.Perm("ai", "endpoints", authz.VerbStop)))
	r.Add(m("Restart"), authz.OnID(authz.Perm("ai", "endpoints", authz.VerbRestart)))
}

// EndpointHandler implements nebius.ai.v1.EndpointService.
type EndpointHandler struct {
	aipb.UnimplementedEndpointServiceServer
	svc *domain.EndpointService
}

var _ aipb.EndpointServiceServer = (*EndpointHandler)(nil)

// NewEndpointHandler returns a handler backed by svc.
func NewEndpointHandler(svc *domain.EndpointService) *EndpointHandler {
	return &EndpointHandler{svc: svc}
}

// Get returns an endpoint; view SECRET includes input-only secrets.
func (h *EndpointHandler) Get(ctx context.Context, req *aipb.GetEndpointRequest) (*aipb.Endpoint, error) {
	get := h.svc.Get
	if req.GetView() == aipb.GetEndpointRequest_SECRET {
		get = h.svc.GetSecret
	}
	e, err := get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return e, nil
}

// GetByName returns an endpoint by parent and name.
func (h *EndpointHandler) GetByName(ctx context.Context, req *aipb.GetEndpointByNameRequest) (*aipb.Endpoint, error) {
	e, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return e, nil
}

// List returns one page of a project's endpoints.
func (h *EndpointHandler) List(ctx context.Context, req *aipb.ListEndpointsRequest) (*aipb.ListEndpointsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &aipb.ListEndpointsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts an endpoint creation operation.
func (h *EndpointHandler) Create(ctx context.Context, req *aipb.CreateEndpointRequest) (*commonpb.Operation, error) {
	return toOperation(ctx)(h.svc.Create(ctx, domain.CreateEndpointCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), DryRun: req.GetDryRun(), Request: req,
	}))
}

// Delete starts an endpoint deletion operation.
func (h *EndpointHandler) Delete(ctx context.Context, req *aipb.DeleteEndpointRequest) (*commonpb.Operation, error) {
	return toOperation(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

// Start starts a stopped endpoint.
func (h *EndpointHandler) Start(ctx context.Context, req *aipb.StartEndpointRequest) (*commonpb.Operation, error) {
	return toOperation(ctx)(h.svc.Start(ctx, req.GetId(), req))
}

// Stop stops a running endpoint.
func (h *EndpointHandler) Stop(ctx context.Context, req *aipb.StopEndpointRequest) (*commonpb.Operation, error) {
	return toOperation(ctx)(h.svc.Stop(ctx, req.GetId(), req))
}

// Restart restarts an endpoint.
func (h *EndpointHandler) Restart(ctx context.Context, req *aipb.RestartEndpointRequest) (*commonpb.Operation, error) {
	return toOperation(ctx)(h.svc.Restart(ctx, req.GetId(), req))
}

// toOperation maps a domain operation result to the RPC response.
func toOperation(ctx context.Context) func(*operations.Operation, error) (*commonpb.Operation, error) {
	return func(op *operations.Operation, err error) (*commonpb.Operation, error) {
		if err != nil {
			return nil, serviceerror.FromDomain(ctx, err)
		}
		return opsapi.ToProto(ctx, op), nil
	}
}
