package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// FederationCertificateHandler implements nebius.iam.v1.FederationCertificateService.
type FederationCertificateHandler struct {
	iampb.UnimplementedFederationCertificateServiceServer
	svc *domain.FederationCertificateService
}

var _ iampb.FederationCertificateServiceServer = (*FederationCertificateHandler)(nil)

// NewFederationCertificateHandler returns a handler backed by svc.
func NewFederationCertificateHandler(svc *domain.FederationCertificateService) *FederationCertificateHandler {
	return &FederationCertificateHandler{svc: svc}
}

// Get returns a certificate by ID.
func (h *FederationCertificateHandler) Get(ctx context.Context, req *iampb.GetFederationCertificateRequest) (*iampb.FederationCertificate, error) {
	c, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return c, nil
}

// ListByFederation returns one page of a federation's certificates.
func (h *FederationCertificateHandler) ListByFederation(ctx context.Context, req *iampb.ListFederationCertificateByFederationRequest) (*iampb.ListFederationCertificateResponse, error) {
	page, err := h.svc.ListByFederation(ctx, req.GetFederationId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListFederationCertificateResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a certificate creation operation.
func (h *FederationCertificateHandler) Create(ctx context.Context, req *iampb.CreateFederationCertificateRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Create(ctx, domain.CreateFederationCertificateCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a certificate update; X-ResetMask selects fields to clear.
func (h *FederationCertificateHandler) Update(ctx context.Context, req *iampb.UpdateFederationCertificateRequest) (*commonpb.Operation, error) {
	resetMask, err := resetMaskOf(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opResult(ctx)(h.svc.Update(ctx, domain.UpdateFederationCertificateCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask,
	}, req))
}

// UpdateBulk starts an operation replacing a federation's certificate set.
// Each element is applied like Update without a reset mask.
func (h *FederationCertificateHandler) UpdateBulk(ctx context.Context, req *iampb.UpdateBulkFederationCertificateRequest) (*commonpb.Operation, error) {
	updates := make([]domain.UpdateFederationCertificateCmd, 0, len(req.GetUpdates()))
	for _, u := range req.GetUpdates() {
		updates = append(updates, domain.UpdateFederationCertificateCmd{Metadata: u.GetMetadata(), Spec: u.GetSpec()})
	}
	return opResult(ctx)(h.svc.UpdateBulk(ctx, req.GetFederationId(), updates, req))
}

// Delete starts a certificate deletion operation.
func (h *FederationCertificateHandler) Delete(ctx context.Context, req *iampb.DeleteFederationCertificateRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}
