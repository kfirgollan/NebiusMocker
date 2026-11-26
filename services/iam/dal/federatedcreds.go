package dal

import (
	"context"
	"fmt"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// FederatedCredentialsRepo stores federated credentials. Their names are
// optional: the store enforces per-parent uniqueness only for non-empty names.
type FederatedCredentialsRepo struct{ st store.Store }

var _ domain.FederatedCredentialsRepository = (*FederatedCredentialsRepo)(nil)

// NewFederatedCredentialsRepo returns a FederatedCredentialsRepo over st.
func NewFederatedCredentialsRepo(st store.Store) *FederatedCredentialsRepo {
	return &FederatedCredentialsRepo{st: st}
}

func newFederatedCredentials() *iampb.FederatedCredentials { return &iampb.FederatedCredentials{} }

// Get returns the credentials or errs.NotFound.
func (r *FederatedCredentialsRepo) Get(ctx context.Context, id string) (*iampb.FederatedCredentials, error) {
	return get(ctx, r.st, domain.KindFederatedCredentials, id, newFederatedCredentials)
}

// GetByName returns the credentials named name in parentID or errs.NotFound.
func (r *FederatedCredentialsRepo) GetByName(ctx context.Context, parentID, name string) (*iampb.FederatedCredentials, error) {
	return getByName(ctx, r.st, domain.KindFederatedCredentials, parentID, name, newFederatedCredentials)
}

// List returns one page of a parent's credentials in creation order.
func (r *FederatedCredentialsRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.FederatedCredentialsPage, error) {
	items, next, err := list(ctx, r.st, domain.KindFederatedCredentials, parentID, pageSize, pageToken, newFederatedCredentials)
	if err != nil {
		return domain.FederatedCredentialsPage{}, err
	}
	return domain.FederatedCredentialsPage{Items: items, NextPageToken: next}, nil
}

// ListBySubject scans every credential for those whose subject is saID.
func (r *FederatedCredentialsRepo) ListBySubject(ctx context.Context, saID string) ([]*iampb.FederatedCredentials, error) {
	var out []*iampb.FederatedCredentials
	err := r.st.View(ctx, func(tx store.Tx) error {
		recs, err := tx.List(store.ListQuery{Kind: domain.KindFederatedCredentials, AnyParent: true})
		if err != nil {
			return err
		}
		for _, rec := range recs {
			c, err := decode(rec, newFederatedCredentials)
			if err != nil {
				return err
			}
			if c.GetSpec().GetSubjectId() == saID {
				out = append(out, c)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("federated credentials of %s: %w", saID, err)
	}
	return out, nil
}

// Create inserts credentials.
func (r *FederatedCredentialsRepo) Create(ctx context.Context, c *iampb.FederatedCredentials) error {
	return write(ctx, r.st, domain.KindFederatedCredentials, c, true, nil)
}

// Update replaces existing credentials.
func (r *FederatedCredentialsRepo) Update(ctx context.Context, c *iampb.FederatedCredentials) error {
	return write(ctx, r.st, domain.KindFederatedCredentials, c, false, nil)
}

// Delete removes the credentials or returns errs.NotFound.
func (r *FederatedCredentialsRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindFederatedCredentials, id)
}
