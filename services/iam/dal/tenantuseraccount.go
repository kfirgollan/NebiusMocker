package dal

import (
	"context"
	"errors"
	"fmt"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// TenantUserAccountRepo stores tenant user accounts (records are unnamed
// children of their tenant).
type TenantUserAccountRepo struct{ st store.Store }

var _ domain.TenantUserAccountRepository = (*TenantUserAccountRepo)(nil)

// NewTenantUserAccountRepo returns a TenantUserAccountRepo over st.
func NewTenantUserAccountRepo(st store.Store) *TenantUserAccountRepo {
	return &TenantUserAccountRepo{st: st}
}

func newTenantUserAccount() *iampb.TenantUserAccount { return &iampb.TenantUserAccount{} }

// Get returns the account or errs.NotFound.
func (r *TenantUserAccountRepo) Get(ctx context.Context, id string) (*iampb.TenantUserAccount, error) {
	return get(ctx, r.st, domain.KindTenantUserAccount, id, newTenantUserAccount)
}

// List returns one page of a tenant's accounts in creation order.
func (r *TenantUserAccountRepo) List(ctx context.Context, tenantID string, pageSize int, pageToken string) (domain.TenantUserAccountPage, error) {
	items, next, err := list(ctx, r.st, domain.KindTenantUserAccount, tenantID, pageSize, pageToken, newTenantUserAccount)
	if err != nil {
		return domain.TenantUserAccountPage{}, err
	}
	return domain.TenantUserAccountPage{Items: items, NextPageToken: next}, nil
}

// Ensure inserts a unless an account with the same ID exists.
func (r *TenantUserAccountRepo) Ensure(ctx context.Context, a *iampb.TenantUserAccount) error {
	md := a.GetMetadata()
	data, err := marshal.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode tenant user account %s: %w", md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, gerr := tx.Get(domain.KindTenantUserAccount, md.GetId()); !errors.Is(gerr, store.ErrNotFound) {
			return gerr // nil when it already exists: nothing to do
		}
		_, perr := tx.Put(store.Record{Kind: domain.KindTenantUserAccount, ID: md.GetId(), ParentID: md.GetParentId(), Data: data})
		return perr
	})
	return toDomain(err, md.GetId())
}

// Create inserts an account.
func (r *TenantUserAccountRepo) Create(ctx context.Context, a *iampb.TenantUserAccount) error {
	return write(ctx, r.st, domain.KindTenantUserAccount, a, true, nil)
}

// Update replaces an existing account.
func (r *TenantUserAccountRepo) Update(ctx context.Context, a *iampb.TenantUserAccount) error {
	return write(ctx, r.st, domain.KindTenantUserAccount, a, false, nil)
}

// Delete removes the account or returns errs.NotFound.
func (r *TenantUserAccountRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindTenantUserAccount, id)
}

// CountByFederation scans every account for those linked to federationID.
func (r *TenantUserAccountRepo) CountByFederation(ctx context.Context, federationID string) (int, error) {
	n := 0
	err := r.st.View(ctx, func(tx store.Tx) error {
		recs, err := tx.List(store.ListQuery{Kind: domain.KindTenantUserAccount, AnyParent: true})
		if err != nil {
			return err
		}
		for _, rec := range recs {
			a, err := decode(rec, newTenantUserAccount)
			if err != nil {
				return err
			}
			if a.GetStatus().GetFederationId() == federationID {
				n++
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("tenant user accounts of %s: %w", federationID, err)
	}
	return n, nil
}
