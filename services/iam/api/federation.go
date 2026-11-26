package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	federationService     = "nebius.iam.v1.FederationService"
	federationCertService = "nebius.iam.v1.FederationCertificateService"
)

// onFederationID checks perm on the federation named by a request's
// federation_id field.
func onFederationID(perm authz.Permission, id func(req any) string) authz.Rule {
	return authz.Rule{
		Permission: perm,
		Target: func(_ context.Context, req any) (string, error) {
			if v := id(req); v != "" {
				return v, nil
			}
			return "", errs.BadRequest(errs.Violation{Field: "federation_id", Message: "required"})
		},
	}
}

// RegisterFederationRules declares the permissions of the federation and
// federation certificate RPCs. Certificates are checked on their own kind
// (their parent is the federation), bulk and list calls on the federation.
func RegisterFederationRules(r *authz.Rules) {
	f := func(method string) string { return "/" + federationService + "/" + method }
	fedUpdate := authz.Perm("iam", "federations", authz.VerbUpdate)
	r.CRUD(federationService, "iam", "federations")
	r.Add(f("Activate"), onFederationID(fedUpdate, func(req any) string {
		q, _ := req.(*iampb.ActivateFederationRequest)
		return q.GetFederationId()
	}))
	r.Add(f("Deactivate"), onFederationID(fedUpdate, func(req any) string {
		q, _ := req.(*iampb.DeactivateFederationRequest)
		return q.GetFederationId()
	}))

	c := func(method string) string { return "/" + federationCertService + "/" + method }
	perm := func(verb string) authz.Permission { return authz.Perm("iam", "federationCertificates", verb) }
	r.Add(c("Get"), authz.OnID(perm(authz.VerbGet)))
	r.Add(c("Create"), authz.OnMetadataParent(perm(authz.VerbCreate)))
	r.Add(c("Update"), authz.OnMetadataID(perm(authz.VerbUpdate)))
	r.Add(c("Delete"), authz.OnID(perm(authz.VerbDelete)))
	r.Add(c("ListByFederation"), onFederationID(perm(authz.VerbList), func(req any) string {
		q, _ := req.(*iampb.ListFederationCertificateByFederationRequest)
		return q.GetFederationId()
	}))
	r.Add(c("UpdateBulk"), onFederationID(perm(authz.VerbUpdate), func(req any) string {
		q, _ := req.(*iampb.UpdateBulkFederationCertificateRequest)
		return q.GetFederationId()
	}))
}

// FederationHandler implements nebius.iam.v1.FederationService.
type FederationHandler struct {
	iampb.UnimplementedFederationServiceServer
	svc *domain.FederationService
}

var _ iampb.FederationServiceServer = (*FederationHandler)(nil)

// NewFederationHandler returns a handler backed by svc.
func NewFederationHandler(svc *domain.FederationService) *FederationHandler {
	return &FederationHandler{svc: svc}
}

// Get returns a federation by ID.
func (h *FederationHandler) Get(ctx context.Context, req *iampb.GetFederationRequest) (*iampb.Federation, error) {
	f, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return f, nil
}

// GetByName returns a federation by tenant and name.
func (h *FederationHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*iampb.Federation, error) {
	f, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return f, nil
}

// List returns one page of a tenant's federations.
func (h *FederationHandler) List(ctx context.Context, req *iampb.ListFederationsRequest) (*iampb.ListFederationsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListFederationsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a federation creation operation.
func (h *FederationHandler) Create(ctx context.Context, req *iampb.CreateFederationRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Create(ctx, domain.CreateFederationCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a federation update; X-ResetMask selects fields to clear.
func (h *FederationHandler) Update(ctx context.Context, req *iampb.UpdateFederationRequest) (*commonpb.Operation, error) {
	resetMask, err := resetMaskOf(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opResult(ctx)(h.svc.Update(ctx, domain.UpdateFederationCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	}))
}

// Activate starts an activation operation.
func (h *FederationHandler) Activate(ctx context.Context, req *iampb.ActivateFederationRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Activate(ctx, req.GetFederationId(), req))
}

// Deactivate starts a deactivation operation.
func (h *FederationHandler) Deactivate(ctx context.Context, req *iampb.DeactivateFederationRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Deactivate(ctx, req.GetFederationId(), req))
}

// Delete starts a federation deletion operation.
func (h *FederationHandler) Delete(ctx context.Context, req *iampb.DeleteFederationRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}
