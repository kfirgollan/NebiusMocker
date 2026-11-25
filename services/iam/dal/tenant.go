package dal

import (
	"context"
	"errors"
	"fmt"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// TenantRepo stores tenants.
type TenantRepo struct{ st store.Store }

var _ domain.ContainerRepository = (*TenantRepo)(nil)

// NewTenantRepo returns a TenantRepo over st.
func NewTenantRepo(st store.Store) *TenantRepo { return &TenantRepo{st: st} }

// Get returns the tenant or errs.NotFound.
func (r *TenantRepo) Get(ctx context.Context, id string) (*iampb.Container, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindTenant, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decode(rec, func() *iampb.Container { return &iampb.Container{} })
}

// Ensure inserts t unless a tenant with the same ID exists.
func (r *TenantRepo) Ensure(ctx context.Context, t *iampb.Container) error {
	md := t.GetMetadata()
	data, err := marshal.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode tenant %s: %w", md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, gerr := tx.Get(domain.KindTenant, md.GetId()); !errors.Is(gerr, store.ErrNotFound) {
			return gerr // nil when it already exists: nothing to do
		}
		_, perr := tx.Put(store.Record{Kind: domain.KindTenant, ID: md.GetId(), Name: md.GetName(), Data: data})
		return perr
	})
	return toDomain(err, md.GetId())
}

// ProjectRepo stores projects (records are children of their tenant).
type ProjectRepo struct{ st store.Store }

var _ domain.ContainerRepository = (*ProjectRepo)(nil)

// NewProjectRepo returns a ProjectRepo over st.
func NewProjectRepo(st store.Store) *ProjectRepo { return &ProjectRepo{st: st} }

// Get returns the project or errs.NotFound.
func (r *ProjectRepo) Get(ctx context.Context, id string) (*iampb.Container, error) {
	return get(ctx, r.st, domain.KindProject, id, func() *iampb.Container { return &iampb.Container{} })
}

// Ensure inserts p unless a project with the same ID exists.
func (r *ProjectRepo) Ensure(ctx context.Context, p *iampb.Container) error {
	if _, err := r.Get(ctx, p.GetMetadata().GetId()); err == nil {
		return nil
	}
	return write(ctx, r.st, domain.KindProject, p, true, nil)
}
