package api

import (
	"context"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// PayloadHandler implements nebius.mysterybox.v1.PayloadService (dpl.mysterybox).
type PayloadHandler struct {
	mysteryboxpb.UnimplementedPayloadServiceServer
	svc *domain.Service
}

var _ mysteryboxpb.PayloadServiceServer = (*PayloadHandler)(nil)

// NewPayloadHandler returns a handler backed by svc.
func NewPayloadHandler(svc *domain.Service) *PayloadHandler { return &PayloadHandler{svc: svc} }

// Get returns every payload entry of a version (default: the primary).
func (h *PayloadHandler) Get(ctx context.Context, req *mysteryboxpb.GetPayloadRequest) (*mysteryboxpb.SecretPayload, error) {
	p, err := h.svc.GetPayload(ctx, domain.PayloadRef{SecretID: req.GetSecretId(), VersionID: req.GetVersionId()})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return p, nil
}

// GetByKey returns one payload entry of a version (default: the primary).
func (h *PayloadHandler) GetByKey(ctx context.Context, req *mysteryboxpb.GetPayloadByKeyRequest) (*mysteryboxpb.SecretPayloadEntry, error) {
	e, err := h.svc.GetPayloadByKey(ctx, domain.PayloadRef{SecretID: req.GetSecretId(), VersionID: req.GetVersionId()}, req.GetKey())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return e, nil
}
