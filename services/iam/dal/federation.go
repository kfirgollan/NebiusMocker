package dal

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// FederationRepo stores federations; names are unique per tenant.
type FederationRepo struct{ st store.Store }

var _ domain.FederationRepository = (*FederationRepo)(nil)

// NewFederationRepo returns a FederationRepo over st.
func NewFederationRepo(st store.Store) *FederationRepo { return &FederationRepo{st: st} }

func newFederation() *iampb.Federation { return &iampb.Federation{} }

// Get returns the federation or errs.NotFound.
func (r *FederationRepo) Get(ctx context.Context, id string) (*iampb.Federation, error) {
	return get(ctx, r.st, domain.KindFederation, id, newFederation)
}

// GetByName returns the federation named name in tenantID or errs.NotFound.
func (r *FederationRepo) GetByName(ctx context.Context, tenantID, name string) (*iampb.Federation, error) {
	return getByName(ctx, r.st, domain.KindFederation, tenantID, name, newFederation)
}

// List returns one page of a tenant's federations in creation order.
func (r *FederationRepo) List(ctx context.Context, tenantID string, pageSize int, pageToken string) (domain.FederationPage, error) {
	items, next, err := list(ctx, r.st, domain.KindFederation, tenantID, pageSize, pageToken, newFederation)
	if err != nil {
		return domain.FederationPage{}, err
	}
	return domain.FederationPage{Items: items, NextPageToken: next}, nil
}

// Create inserts a federation; errs.AlreadyExists if the name is taken.
func (r *FederationRepo) Create(ctx context.Context, f *iampb.Federation) error {
	return write(ctx, r.st, domain.KindFederation, f, true, nil)
}

// Update replaces an existing federation.
func (r *FederationRepo) Update(ctx context.Context, f *iampb.Federation) error {
	return write(ctx, r.st, domain.KindFederation, f, false, nil)
}

// Delete removes the federation or returns errs.NotFound.
func (r *FederationRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindFederation, id)
}
