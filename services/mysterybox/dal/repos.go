package dal

import (
	"context"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// kindPayload is the store kind of sealed payloads (keyed by version ID).
const kindPayload = "mbsecpayload"

// SecretRepo stores secrets.
type SecretRepo struct{ st store.Store }

var _ domain.SecretRepository = (*SecretRepo)(nil)

// NewSecretRepo returns a SecretRepo over st.
func NewSecretRepo(st store.Store) *SecretRepo { return &SecretRepo{st: st} }

func newSecret() *mysteryboxpb.Secret { return &mysteryboxpb.Secret{} }

// indexName is the name a secret holds in the store's per-project name index:
// a secret scheduled for deletion releases its name.
func indexName(s *mysteryboxpb.Secret) string {
	if s.GetStatus().GetState() == mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION {
		return ""
	}
	return s.GetMetadata().GetName()
}

// Get returns the secret or errs.NotFound.
func (r *SecretRepo) Get(ctx context.Context, id string) (*mysteryboxpb.Secret, error) {
	return get(ctx, r.st, domain.KindSecret, id, newSecret)
}

// GetByName returns the active secret named name in parentID or errs.NotFound.
func (r *SecretRepo) GetByName(ctx context.Context, parentID, name string) (*mysteryboxpb.Secret, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(domain.KindSecret, parentID, name)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, name)
	}
	return decode(rec, newSecret)
}

// List returns one page of secrets in creation order.
func (r *SecretRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.SecretPage, error) {
	items, next, err := list(ctx, r.st, domain.KindSecret, parentID, pageSize, pageToken, newSecret)
	if err != nil {
		return domain.SecretPage{}, err
	}
	return domain.SecretPage{Items: items, NextPageToken: next}, nil
}

// Create inserts a new secret.
func (r *SecretRepo) Create(ctx context.Context, s *mysteryboxpb.Secret) error {
	return write(ctx, r.st, domain.KindSecret, s, indexName(s), true)
}

// Update replaces an existing secret.
func (r *SecretRepo) Update(ctx context.Context, s *mysteryboxpb.Secret) error {
	return write(ctx, r.st, domain.KindSecret, s, indexName(s), false)
}

// Delete removes the secret or returns errs.NotFound.
func (r *SecretRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindSecret, id)
}

// VersionRepo stores secret versions.
type VersionRepo struct{ st store.Store }

var _ domain.VersionRepository = (*VersionRepo)(nil)

// NewVersionRepo returns a VersionRepo over st.
func NewVersionRepo(st store.Store) *VersionRepo { return &VersionRepo{st: st} }

func newVersion() *mysteryboxpb.SecretVersion { return &mysteryboxpb.SecretVersion{} }

// Get returns the version or errs.NotFound.
func (r *VersionRepo) Get(ctx context.Context, id string) (*mysteryboxpb.SecretVersion, error) {
	return get(ctx, r.st, domain.KindSecretVersion, id, newVersion)
}

// List returns one page of a secret's versions, oldest first.
func (r *VersionRepo) List(ctx context.Context, secretID string, pageSize int, pageToken string) (domain.VersionPage, error) {
	items, next, err := list(ctx, r.st, domain.KindSecretVersion, secretID, pageSize, pageToken, newVersion)
	if err != nil {
		return domain.VersionPage{}, err
	}
	return domain.VersionPage{Items: items, NextPageToken: next}, nil
}

// All returns every version of a secret, oldest first.
func (r *VersionRepo) All(ctx context.Context, secretID string) ([]*mysteryboxpb.SecretVersion, error) {
	items, _, err := list(ctx, r.st, domain.KindSecretVersion, secretID, 0, "", newVersion)
	return items, err
}

// Create inserts a new version (versions are unnamed).
func (r *VersionRepo) Create(ctx context.Context, v *mysteryboxpb.SecretVersion) error {
	return write(ctx, r.st, domain.KindSecretVersion, v, "", true)
}

// Update replaces an existing version.
func (r *VersionRepo) Update(ctx context.Context, v *mysteryboxpb.SecretVersion) error {
	return write(ctx, r.st, domain.KindSecretVersion, v, "", false)
}

// Delete removes the version or returns errs.NotFound.
func (r *VersionRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindSecretVersion, id)
}

// PayloadRepo stores sealed payloads.
type PayloadRepo struct{ st store.Store }

var _ domain.PayloadRepository = (*PayloadRepo)(nil)

// NewPayloadRepo returns a PayloadRepo over st.
func NewPayloadRepo(st store.Store) *PayloadRepo { return &PayloadRepo{st: st} }

// Put stores (or replaces) the sealed payload of a version.
func (r *PayloadRepo) Put(ctx context.Context, versionID, secretID string, sealed []byte) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		_, perr := tx.Put(store.Record{Kind: kindPayload, ID: versionID, ParentID: secretID, Data: sealed})
		return perr
	})
	return toDomain(err, versionID)
}

// Get returns the sealed payload of a version or errs.NotFound.
func (r *PayloadRepo) Get(ctx context.Context, versionID string) ([]byte, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(kindPayload, versionID)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, versionID)
	}
	return rec.Data, nil
}

// Delete removes the sealed payload of a version or returns errs.NotFound.
func (r *PayloadRepo) Delete(ctx context.Context, versionID string) error {
	return remove(ctx, r.st, kindPayload, versionID)
}
