package dal

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// PermitRepo stores access permits (records are children of their group).
type PermitRepo struct{ st store.Store }

var _ domain.PermitRepository = (*PermitRepo)(nil)

// NewPermitRepo returns a PermitRepo over st.
func NewPermitRepo(st store.Store) *PermitRepo { return &PermitRepo{st: st} }

func newPermit() *iampb.AccessPermit { return &iampb.AccessPermit{} }

// Get returns the permit or errs.NotFound.
func (r *PermitRepo) Get(ctx context.Context, id string) (*iampb.AccessPermit, error) {
	return get(ctx, r.st, domain.KindAccessPermit, id, newPermit)
}

// Create inserts the permit unless the group already holds that role on that resource.
func (r *PermitRepo) Create(ctx context.Context, p *iampb.AccessPermit) error {
	return write(ctx, r.st, domain.KindAccessPermit, p, true, func(tx store.Tx) error {
		existing, err := siblings(tx, domain.KindAccessPermit, p.GetMetadata().GetParentId(), newPermit)
		if err != nil {
			return err
		}
		for _, e := range existing {
			if e.GetSpec().GetResourceId() == p.GetSpec().GetResourceId() && e.GetSpec().GetRole() == p.GetSpec().GetRole() {
				return errs.AlreadyExists(e.GetMetadata().GetId(), "the group already has this role on this resource")
			}
		}
		return nil
	})
}

// Delete removes the permit or returns errs.NotFound.
func (r *PermitRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindAccessPermit, id)
}

// ListByGroup returns one page of a group's permits.
func (r *PermitRepo) ListByGroup(ctx context.Context, groupID string, pageSize int, pageToken string) (domain.PermitPage, error) {
	items, next, err := list(ctx, r.st, domain.KindAccessPermit, groupID, pageSize, pageToken, newPermit)
	if err != nil {
		return domain.PermitPage{}, err
	}
	return domain.PermitPage{Items: items, NextPageToken: next}, nil
}
