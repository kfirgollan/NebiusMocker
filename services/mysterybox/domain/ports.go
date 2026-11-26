// Package domain holds the MysteryBox business logic: secrets, their immutable
// versions and the sealed payloads those versions carry. It is transport-free
// and depends only on the ports declared in this file.
//
// Payloads are sensitive. The domain never logs them, never puts them in
// errors, and returns them only from the payload read methods.
package domain

import (
	"context"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Resource type prefixes (NID types) owned by MysteryBox.
const (
	KindSecret        = "mbsec"
	KindSecretVersion = "mbsecver"
	// KindProject is the only parent kind a secret may have (owned by IAM).
	KindProject = "project"
)

// SecretPage is one page of a secret listing.
type SecretPage struct {
	Items         []*mysteryboxpb.Secret
	NextPageToken string
}

// SecretRepository stores secrets, including soft-deleted ones. Only secrets
// in state ACTIVE hold their name: a secret scheduled for deletion releases it.
type SecretRepository interface {
	// Get returns the secret in any state, or errs.NotFound.
	Get(ctx context.Context, id string) (*mysteryboxpb.Secret, error)
	// GetByName returns the active secret with the name, or errs.NotFound.
	GetByName(ctx context.Context, parentID, name string) (*mysteryboxpb.Secret, error)
	// List returns one page of the parent's secrets in any state.
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (SecretPage, error)
	// Create fails with errs.AlreadyExists if an active secret has the name.
	Create(ctx context.Context, s *mysteryboxpb.Secret) error
	// Update fails with errs.NotFound if the secret does not exist, and with
	// errs.AlreadyExists if an active secret has the (new) name.
	Update(ctx context.Context, s *mysteryboxpb.Secret) error
	Delete(ctx context.Context, id string) error
}

// VersionPage is one page of a version listing.
type VersionPage struct {
	Items         []*mysteryboxpb.SecretVersion
	NextPageToken string
}

// VersionRepository stores secret versions (metadata only: never payloads).
type VersionRepository interface {
	// Get returns the version in any state, or errs.NotFound.
	Get(ctx context.Context, id string) (*mysteryboxpb.SecretVersion, error)
	// List returns one page of the secret's versions in any state, oldest first.
	List(ctx context.Context, secretID string, pageSize int, pageToken string) (VersionPage, error)
	// All returns every version of the secret in any state, oldest first.
	All(ctx context.Context, secretID string) ([]*mysteryboxpb.SecretVersion, error)
	Create(ctx context.Context, v *mysteryboxpb.SecretVersion) error
	Update(ctx context.Context, v *mysteryboxpb.SecretVersion) error
	Delete(ctx context.Context, id string) error
}

// PayloadRepository stores sealed (encrypted) payloads, one per version, plus
// one per secret created with an inline version (its sealed spec, keyed by
// the secret ID). It only ever sees ciphertext.
type PayloadRepository interface {
	Put(ctx context.Context, versionID, secretID string, sealed []byte) error
	// Get returns the sealed payload of a version, or errs.NotFound.
	Get(ctx context.Context, versionID string) ([]byte, error)
	Delete(ctx context.Context, versionID string) error
}

// PayloadSealer encrypts payloads at rest. The mocker's implementation is
// AES-GCM under an instance key (backend/aesgcm); a KMS-backed sealer can
// replace it without touching the domain.
type PayloadSealer interface {
	// KeyID is the NID of the key payloads are sealed with, reported as
	// SecretStatus.effective_kms_key_id.
	KeyID() string
	// Seal encrypts plaintext, binding it to aad (the version ID).
	Seal(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	// Open decrypts a sealed payload produced by Seal with the same aad.
	Open(ctx context.Context, sealed, aad []byte) ([]byte, error)
}

// Hierarchy resolves resource ancestry (satisfied by *authz.Engine). It is
// how the domain checks that a parent project exists without importing IAM.
type Hierarchy interface {
	// Parent returns the parent of id, or errs.NotFound if id does not exist.
	Parent(ctx context.Context, id string) (string, error)
}

// Operations starts long-running operations (satisfied by *operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates NIDs (satisfied by pkg/nid generators).
type IDGenerator interface {
	New(typ string) (string, error)
}

// QuotaGate admits a quota claim and runs apply, the store write that makes the
// consumption visible to the usage source (satisfied by peer.QuotaGate).
type QuotaGate interface {
	Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error
}
