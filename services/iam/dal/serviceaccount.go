package dal

import (
	"context"

	"google.golang.org/protobuf/proto"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// ServiceAccountRepo stores service accounts (records are children of their project).
type ServiceAccountRepo struct{ st store.Store }

var _ domain.ServiceAccountRepository = (*ServiceAccountRepo)(nil)

// NewServiceAccountRepo returns a ServiceAccountRepo over st.
func NewServiceAccountRepo(st store.Store) *ServiceAccountRepo { return &ServiceAccountRepo{st: st} }

func newServiceAccount() *iampb.ServiceAccount { return &iampb.ServiceAccount{} }

// Get returns the service account or errs.NotFound.
func (r *ServiceAccountRepo) Get(ctx context.Context, id string) (*iampb.ServiceAccount, error) {
	return get(ctx, r.st, domain.KindServiceAccount, id, newServiceAccount)
}

// GetByName returns the service account named name in parentID or errs.NotFound.
func (r *ServiceAccountRepo) GetByName(ctx context.Context, parentID, name string) (*iampb.ServiceAccount, error) {
	return getByName(ctx, r.st, domain.KindServiceAccount, parentID, name, newServiceAccount)
}

// List returns one page of service accounts in creation order.
func (r *ServiceAccountRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.ServiceAccountPage, error) {
	items, next, err := list(ctx, r.st, domain.KindServiceAccount, parentID, pageSize, pageToken, newServiceAccount)
	if err != nil {
		return domain.ServiceAccountPage{}, err
	}
	return domain.ServiceAccountPage{Items: items, NextPageToken: next}, nil
}

// Create inserts a service account; the name must be unique in its project.
func (r *ServiceAccountRepo) Create(ctx context.Context, sa *iampb.ServiceAccount) error {
	return write(ctx, r.st, domain.KindServiceAccount, sa, true, nil)
}

// Update replaces an existing service account.
func (r *ServiceAccountRepo) Update(ctx context.Context, sa *iampb.ServiceAccount) error {
	return write(ctx, r.st, domain.KindServiceAccount, sa, false, nil)
}

// Delete removes the service account or returns errs.NotFound.
func (r *ServiceAccountRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindServiceAccount, id)
}

// getByName reads the record of kind named name under parentID.
func getByName[T proto.Message](ctx context.Context, st store.Store, kind, parentID, name string, newMsg func() T) (T, error) {
	var rec store.Record
	err := st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(kind, parentID, name)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, toDomain(err, name)
	}
	return decode(rec, newMsg)
}
