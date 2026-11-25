package api

import (
	"context"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/kms/domain"
)

// AsymmetricCryptoHandler implements nebius.kms.v1.AsymmetricCryptoService.
type AsymmetricCryptoHandler struct {
	kmspb.UnimplementedAsymmetricCryptoServiceServer
	svc *domain.AsymmetricKeyService
}

var _ kmspb.AsymmetricCryptoServiceServer = (*AsymmetricCryptoHandler)(nil)

// NewAsymmetricCryptoHandler returns a handler backed by svc.
func NewAsymmetricCryptoHandler(svc *domain.AsymmetricKeyService) *AsymmetricCryptoHandler {
	return &AsymmetricCryptoHandler{svc: svc}
}

// SignHash signs a precomputed hash.
func (h *AsymmetricCryptoHandler) SignHash(ctx context.Context, req *kmspb.AsymmetricSignHashRequest) (*kmspb.AsymmetricSignHashResponse, error) {
	sig, err := h.svc.SignHash(ctx, req.GetKeyId(), req.GetHash())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &kmspb.AsymmetricSignHashResponse{KeyId: req.GetKeyId(), Signature: sig}, nil
}

// GetPublicKey returns the PEM public key.
func (h *AsymmetricCryptoHandler) GetPublicKey(ctx context.Context, req *kmspb.AsymmetricGetPublicKeyRequest) (*kmspb.AsymmetricGetPublicKeyResponse, error) {
	pub, err := h.svc.GetPublicKey(ctx, req.GetKeyId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &kmspb.AsymmetricGetPublicKeyResponse{KeyId: req.GetKeyId(), PublicKey: pub}, nil
}

// Decrypt decrypts ciphertext encrypted to the public key.
func (h *AsymmetricCryptoHandler) Decrypt(ctx context.Context, req *kmspb.AsymmetricDecryptRequest) (*kmspb.AsymmetricDecryptResponse, error) {
	pt, err := h.svc.Decrypt(ctx, req.GetKeyId(), req.GetCiphertext())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &kmspb.AsymmetricDecryptResponse{KeyId: req.GetKeyId(), Plaintext: pt}, nil
}
