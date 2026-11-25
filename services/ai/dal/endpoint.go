package dal

import (
	"context"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

// EndpointRepo stores AI endpoints.
type EndpointRepo struct{ st store.Store }

var _ domain.EndpointRepository = (*EndpointRepo)(nil)

// NewEndpointRepo returns an EndpointRepo over st.
func NewEndpointRepo(st store.Store) *EndpointRepo { return &EndpointRepo{st: st} }

func newEndpoint() *aipb.Endpoint { return &aipb.Endpoint{} }

// Get returns the endpoint or errs.NotFound.
func (r *EndpointRepo) Get(ctx context.Context, id string) (*aipb.Endpoint, error) {
	return get(ctx, r.st, domain.KindEndpoint, id, newEndpoint)
}

// GetByName returns the endpoint named name in parentID or errs.NotFound.
func (r *EndpointRepo) GetByName(ctx context.Context, parentID, name string) (*aipb.Endpoint, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(domain.KindEndpoint, parentID, name)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, name)
	}
	return decode(rec, newEndpoint)
}

// List returns one page of endpoints in creation order.
func (r *EndpointRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.EndpointPage, error) {
	items, next, err := list(ctx, r.st, domain.KindEndpoint, parentID, pageSize, pageToken, newEndpoint)
	if err != nil {
		return domain.EndpointPage{}, err
	}
	return domain.EndpointPage{Items: items, NextPageToken: next}, nil
}

// Create inserts a new endpoint; the name must be unique in its parent.
func (r *EndpointRepo) Create(ctx context.Context, e *aipb.Endpoint) error {
	return write(ctx, r.st, domain.KindEndpoint, e, true)
}

// Update replaces an existing endpoint.
func (r *EndpointRepo) Update(ctx context.Context, e *aipb.Endpoint) error {
	return write(ctx, r.st, domain.KindEndpoint, e, false)
}

// Delete removes the endpoint or returns errs.NotFound.
func (r *EndpointRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindEndpoint, id)
}
