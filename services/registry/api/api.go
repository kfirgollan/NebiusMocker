// Package api is the Container Registry gRPC view layer: it converts between
// protos and domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

// Service names of the gRPC services served by this module.
const (
	RegistryServiceName = "nebius.registry.v1.RegistryService"
	ArtifactServiceName = "nebius.registry.v1.ArtifactService"
)

// RegisterAuthzRules declares which permission each registry RPC needs.
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(RegistryServiceName, "registry", "registries")
	m := func(method string) string { return "/" + ArtifactServiceName + "/" + method }
	r.Add(m("Get"), authz.OnID(authz.Perm("registry", "artifacts", authz.VerbGet)))
	r.Add(m("List"), authz.OnParent(authz.Perm("registry", "artifacts", authz.VerbList)))
	r.Add(m("Delete"), authz.OnID(authz.Perm("registry", "artifacts", authz.VerbDelete)))
}

// RegistryHandler implements nebius.registry.v1.RegistryService.
type RegistryHandler struct {
	registrypb.UnimplementedRegistryServiceServer
	svc *domain.RegistryService
}

var _ registrypb.RegistryServiceServer = (*RegistryHandler)(nil)

// NewRegistryHandler returns a handler backed by svc.
func NewRegistryHandler(svc *domain.RegistryService) *RegistryHandler {
	return &RegistryHandler{svc: svc}
}

// Get returns a registry by ID.
func (h *RegistryHandler) Get(ctx context.Context, req *registrypb.GetRegistryRequest) (*registrypb.Registry, error) {
	r, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return r, nil
}

// GetByName returns a registry by project and name.
func (h *RegistryHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*registrypb.Registry, error) {
	r, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return r, nil
}

// List returns one page of a project's registries.
func (h *RegistryHandler) List(ctx context.Context, req *registrypb.ListRegistriesRequest) (*registrypb.ListRegistriesResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &registrypb.ListRegistriesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a registry creation operation.
func (h *RegistryHandler) Create(ctx context.Context, req *registrypb.CreateRegistryRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateRegistryCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a registry update operation; X-ResetMask selects fields to clear.
func (h *RegistryHandler) Update(ctx context.Context, req *registrypb.UpdateRegistryRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	op, err := h.svc.Update(ctx, domain.UpdateRegistryCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts a registry deletion operation.
func (h *RegistryHandler) Delete(ctx context.Context, req *registrypb.DeleteRegistryRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, domain.DeleteRegistryCmd{ID: req.GetId(), Force: req.GetForce(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// ArtifactHandler implements nebius.registry.v1.ArtifactService.
type ArtifactHandler struct {
	registrypb.UnimplementedArtifactServiceServer
	svc *domain.ArtifactService
}

var _ registrypb.ArtifactServiceServer = (*ArtifactHandler)(nil)

// NewArtifactHandler returns a handler backed by svc.
func NewArtifactHandler(svc *domain.ArtifactService) *ArtifactHandler {
	return &ArtifactHandler{svc: svc}
}

// Get returns an artifact by ID.
func (h *ArtifactHandler) Get(ctx context.Context, req *registrypb.GetArtifactRequest) (*registrypb.Artifact, error) {
	a, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return a, nil
}

// List returns one page of a registry's artifacts.
func (h *ArtifactHandler) List(ctx context.Context, req *registrypb.ListArtifactsRequest) (*registrypb.ListArtifactsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &registrypb.ListArtifactsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Delete starts an artifact deletion operation.
func (h *ArtifactHandler) Delete(ctx context.Context, req *registrypb.DeleteArtifactRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}
