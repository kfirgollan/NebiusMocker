package domain

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// AsymmetricKeyService manages asymmetric keys and performs asymmetric crypto.
type AsymmetricKeyService struct {
	lc *lifecycle[*kmspb.AsymmetricKey]
}

// NewAsymmetricKeyService wires an AsymmetricKeyService.
func NewAsymmetricKeyService(d KeyDeps[*kmspb.AsymmetricKey]) (*AsymmetricKeyService, error) {
	lc := &lifecycle[*kmspb.AsymmetricKey]{
		kind: KindAsymmetricKey, label: "asymmetric key",
		repo: d.Keys, ops: d.Ops, ids: d.IDs, hierarchy: d.Hierarchy, clk: d.Clock,
		status: func(k *kmspb.AsymmetricKey) keyStatus {
			s := k.GetStatus()
			return keyStatus{State: s.GetState(), DeletedAt: s.GetDeletedAt(), PurgeAt: s.GetPurgeAt()}
		},
		setStatus: func(k *kmspb.AsymmetricKey, s keyStatus) {
			k.Status = &kmspb.AsymmetricKeyStatus{State: s.State, DeletedAt: s.DeletedAt, PurgeAt: s.PurgeAt}
		},
	}
	if err := lc.validate(); err != nil {
		return nil, err
	}
	return &AsymmetricKeyService{lc: lc}, nil
}

// CreateAsymmetricKeyCmd is the input of Create.
type CreateAsymmetricKeyCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *kmspb.AsymmetricKeySpec
	Request  proto.Message
}

// UpdateAsymmetricKeyCmd is the input of Update.
type UpdateAsymmetricKeyCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *kmspb.AsymmetricKeySpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a key by ID.
func (s *AsymmetricKeyService) Get(ctx context.Context, id string, vis Visibility) (*kmspb.AsymmetricKey, error) {
	return s.lc.Get(ctx, id, vis)
}

// GetByName returns the ACTIVE key with the name in the project.
func (s *AsymmetricKeyService) GetByName(ctx context.Context, parentID, name string) (*kmspb.AsymmetricKey, error) {
	return s.lc.GetByName(ctx, parentID, name)
}

// List returns one page of the project's keys.
func (s *AsymmetricKeyService) List(ctx context.Context, parentID string, pageSize int64, pageToken string, vis Visibility) (Page[*kmspb.AsymmetricKey], error) {
	return s.lc.List(ctx, parentID, pageSize, pageToken, vis)
}

// Create generates a key pair and stores a new ACTIVE key.
func (s *AsymmetricKeyService) Create(ctx context.Context, cmd CreateAsymmetricKeyCmd) (*operations.Operation, error) {
	spec := proto.CloneOf(cmd.Spec)
	if spec == nil {
		spec = &kmspb.AsymmetricKeySpec{}
	}
	if spec.GetAlgorithm() == kmspb.AsymmetricAlgorithm_ASYMMETRIC_ALGORITHM_UNSPECIFIED {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.algorithm", Message: "required"})
	}
	md, err := s.lc.newMetadata(ctx, cmd.Metadata)
	if err != nil {
		return nil, err
	}
	key := &kmspb.AsymmetricKey{
		Metadata: md, Spec: spec,
		Status: &kmspb.AsymmetricKeyStatus{State: kmspb.KeyState_ACTIVE},
	}
	return s.lc.submit(ctx, md.GetId(), "Create asymmetric key", cmd.Request, func(ctx context.Context) error {
		m, err := newAsymMaterial(defaultRand, spec.GetAlgorithm())
		if err != nil {
			return errs.BadRequest(errs.Violation{Field: "spec.algorithm", Message: err.Error()})
		}
		return s.lc.repo.Create(ctx, key, m.der)
	})
}

// Update changes the name, labels or description.
func (s *AsymmetricKeyService) Update(ctx context.Context, cmd UpdateAsymmetricKeyCmd) (*operations.Operation, error) {
	patch := &kmspb.AsymmetricKey{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}
	return s.lc.update(ctx, cmd.Metadata, patch, cmd.ResetMask, cmd.Request, func(stored, updated *kmspb.AsymmetricKey) error {
		if updated.GetSpec() == nil {
			updated.Spec = &kmspb.AsymmetricKeySpec{}
		}
		alg := cmd.Spec.GetAlgorithm()
		if alg != kmspb.AsymmetricAlgorithm_ASYMMETRIC_ALGORITHM_UNSPECIFIED && alg != stored.GetSpec().GetAlgorithm() {
			return errs.BadRequest(errs.Violation{Field: "spec.algorithm", Message: "field is immutable"})
		}
		updated.Spec.Algorithm = stored.GetSpec().GetAlgorithm()
		return nil
	})
}

// Delete schedules the key for deletion.
func (s *AsymmetricKeyService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.lc.Delete(ctx, id, request)
}

// UpdateDeletionDelay reschedules the purge of a key scheduled for deletion.
func (s *AsymmetricKeyService) UpdateDeletionDelay(ctx context.Context, id string, delay time.Duration, request proto.Message) (*operations.Operation, error) {
	return s.lc.UpdateDeletionDelay(ctx, id, delay, request)
}

// Undelete restores a key scheduled for deletion.
func (s *AsymmetricKeyService) Undelete(ctx context.Context, id, newName string, request proto.Message) (*operations.Operation, error) {
	return s.lc.Undelete(ctx, id, newName, request)
}

// usableKey returns an ACTIVE key and its material.
func (s *AsymmetricKeyService) usableKey(ctx context.Context, keyID string) (*kmspb.AsymmetricKey, asymMaterial, error) {
	if keyID == "" {
		return nil, asymMaterial{}, errs.BadRequest(errs.Violation{Field: "key_id", Message: "required"})
	}
	k, err := s.lc.usable(ctx, keyID)
	if err != nil {
		return nil, asymMaterial{}, err
	}
	der, err := s.lc.repo.Material(ctx, keyID)
	if err != nil {
		return nil, asymMaterial{}, fmt.Errorf("material of %s: %w", keyID, err)
	}
	return k, asymMaterial{der: der}, nil
}

// SignHash signs a precomputed digest (SHA-256 for P-256 keys, SHA-384 for
// P-384 keys) and returns an ASN.1 DER ECDSA signature.
func (s *AsymmetricKeyService) SignHash(ctx context.Context, keyID string, hash []byte) ([]byte, error) {
	k, m, err := s.usableKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	alg := k.GetSpec().GetAlgorithm()
	want := hashSize(alg)
	if want == 0 {
		return nil, errs.BadRequest(errs.Violation{Field: "key_id", Message: fmt.Sprintf("a %s key cannot sign", alg)})
	}
	if len(hash) != want {
		return nil, errs.BadRequest(errs.Violation{Field: "hash", Message: fmt.Sprintf("must be %d bytes for %s", want, alg)})
	}
	return m.signHash(defaultRand, hash)
}

// GetPublicKey returns the key's public half as a PKIX PEM block.
func (s *AsymmetricKeyService) GetPublicKey(ctx context.Context, keyID string) (string, error) {
	_, m, err := s.usableKey(ctx, keyID)
	if err != nil {
		return "", err
	}
	return m.publicPEM()
}

// Decrypt decrypts an RSA-OAEP-SHA-256 ciphertext made with the public key.
func (s *AsymmetricKeyService) Decrypt(ctx context.Context, keyID string, ciphertext []byte) ([]byte, error) {
	k, m, err := s.usableKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if alg := k.GetSpec().GetAlgorithm(); alg != kmspb.AsymmetricAlgorithm_RSA_4096_ENC_OAEP_SHA_256 {
		return nil, errs.BadRequest(errs.Violation{Field: "key_id", Message: fmt.Sprintf("a %s key cannot decrypt", alg)})
	}
	pt, err := m.decrypt(defaultRand, ciphertext)
	if err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "ciphertext", Message: err.Error()})
	}
	return pt, nil
}
