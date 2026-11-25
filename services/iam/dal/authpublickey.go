package dal

import (
	"context"
	"fmt"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// AuthPublicKeyRepo stores auth public keys. Their names are optional: the
// store enforces per-parent uniqueness only for non-empty names.
type AuthPublicKeyRepo struct{ st store.Store }

var _ domain.AuthPublicKeyRepository = (*AuthPublicKeyRepo)(nil)

// NewAuthPublicKeyRepo returns an AuthPublicKeyRepo over st.
func NewAuthPublicKeyRepo(st store.Store) *AuthPublicKeyRepo { return &AuthPublicKeyRepo{st: st} }

func newAuthPublicKey() *iampb.AuthPublicKey { return &iampb.AuthPublicKey{} }

// Get returns the key or errs.NotFound.
func (r *AuthPublicKeyRepo) Get(ctx context.Context, id string) (*iampb.AuthPublicKey, error) {
	return get(ctx, r.st, domain.KindAuthPublicKey, id, newAuthPublicKey)
}

// GetByName returns the key named name in parentID or errs.NotFound.
func (r *AuthPublicKeyRepo) GetByName(ctx context.Context, parentID, name string) (*iampb.AuthPublicKey, error) {
	return getByName(ctx, r.st, domain.KindAuthPublicKey, parentID, name, newAuthPublicKey)
}

// List returns one page of a parent's keys in creation order.
func (r *AuthPublicKeyRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.AuthPublicKeyPage, error) {
	items, next, err := list(ctx, r.st, domain.KindAuthPublicKey, parentID, pageSize, pageToken, newAuthPublicKey)
	if err != nil {
		return domain.AuthPublicKeyPage{}, err
	}
	return domain.AuthPublicKeyPage{Items: items, NextPageToken: next}, nil
}

// ListByAccount scans every key for those of accountID.
func (r *AuthPublicKeyRepo) ListByAccount(ctx context.Context, accountID string) ([]*iampb.AuthPublicKey, error) {
	var out []*iampb.AuthPublicKey
	err := r.st.View(ctx, func(tx store.Tx) error {
		recs, err := tx.List(store.ListQuery{Kind: domain.KindAuthPublicKey, AnyParent: true})
		if err != nil {
			return err
		}
		for _, rec := range recs {
			k, err := decode(rec, newAuthPublicKey)
			if err != nil {
				return err
			}
			if k.GetSpec().GetAccount().GetServiceAccount().GetId() == accountID {
				out = append(out, k)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("auth public keys of %s: %w", accountID, err)
	}
	return out, nil
}

// Create inserts a key.
func (r *AuthPublicKeyRepo) Create(ctx context.Context, k *iampb.AuthPublicKey) error {
	return write(ctx, r.st, domain.KindAuthPublicKey, k, true, nil)
}

// Update replaces an existing key.
func (r *AuthPublicKeyRepo) Update(ctx context.Context, k *iampb.AuthPublicKey) error {
	return write(ctx, r.st, domain.KindAuthPublicKey, k, false, nil)
}

// Delete removes the key or returns errs.NotFound.
func (r *AuthPublicKeyRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindAuthPublicKey, id)
}
