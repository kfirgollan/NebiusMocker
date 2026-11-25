package api

import (
	"context"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/kms/domain"
)

// SymmetricCryptoHandler implements nebius.kms.v1.SymmetricCryptoService.
type SymmetricCryptoHandler struct {
	kmspb.UnimplementedSymmetricCryptoServiceServer
	svc *domain.SymmetricKeyService
}

var _ kmspb.SymmetricCryptoServiceServer = (*SymmetricCryptoHandler)(nil)

// NewSymmetricCryptoHandler returns a handler backed by svc.
func NewSymmetricCryptoHandler(svc *domain.SymmetricKeyService) *SymmetricCryptoHandler {
	return &SymmetricCryptoHandler{svc: svc}
}

// Encrypt encrypts plaintext with the key.
func (h *SymmetricCryptoHandler) Encrypt(ctx context.Context, req *kmspb.SymmetricEncryptRequest) (*kmspb.SymmetricEncryptResponse, error) {
	ct, err := h.svc.Encrypt(ctx, req.GetKeyId(), req.GetPlaintext(), req.GetAadContext())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &kmspb.SymmetricEncryptResponse{KeyId: req.GetKeyId(), Ciphertext: ct}, nil
}

// Decrypt decrypts ciphertext produced by Encrypt.
func (h *SymmetricCryptoHandler) Decrypt(ctx context.Context, req *kmspb.SymmetricDecryptRequest) (*kmspb.SymmetricDecryptResponse, error) {
	pt, err := h.svc.Decrypt(ctx, req.GetKeyId(), req.GetCiphertext(), req.GetAadContext())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &kmspb.SymmetricDecryptResponse{KeyId: req.GetKeyId(), Plaintext: pt}, nil
}

// GenerateDataKey returns a fresh data key wrapped by the key.
func (h *SymmetricCryptoHandler) GenerateDataKey(ctx context.Context, req *kmspb.GenerateDataKeyRequest) (*kmspb.GenerateDataKeyResponse, error) {
	dk, err := h.svc.GenerateDataKey(ctx, req.GetKeyId(), req.GetDataKeySpec(), req.GetAadContext())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	resp := &kmspb.GenerateDataKeyResponse{KeyId: req.GetKeyId(), DataKeyCiphertext: dk.Ciphertext}
	if !req.GetSkipPlaintext() {
		resp.DataKeyPlaintext = dk.Plaintext
	}
	return resp, nil
}
