package dal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

func newContainer() *iampb.Container { return &iampb.Container{} }

// TenantRepo stores tenants (roots: records have no parent).
type TenantRepo struct{ st store.Store }

var (
	_ domain.TenantRepository = (*TenantRepo)(nil)
	_ domain.TenantStore      = (*TenantRepo)(nil)
)

// NewTenantRepo returns a TenantRepo over st.
func NewTenantRepo(st store.Store) *TenantRepo { return &TenantRepo{st: st} }

// Get returns the tenant or errs.NotFound.
func (r *TenantRepo) Get(ctx context.Context, id string) (*iampb.Container, error) {
	return get(ctx, r.st, domain.KindTenant, id, newContainer)
}

// GetByName returns the tenant named name or errs.NotFound.
func (r *TenantRepo) GetByName(ctx context.Context, name string) (*iampb.Container, error) {
	return getByName(ctx, r.st, domain.KindTenant, "", name, newContainer)
}

// List returns one page of tenants in creation order.
func (r *TenantRepo) List(ctx context.Context, pageSize int, pageToken string) (domain.ContainerPage, error) {
	items, next, err := list(ctx, r.st, domain.KindTenant, "", pageSize, pageToken, newContainer)
	if err != nil {
		return domain.ContainerPage{}, err
	}
	return domain.ContainerPage{Items: items, NextPageToken: next}, nil
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

// Update replaces an existing tenant; the name must stay unique among tenants.
func (r *TenantRepo) Update(ctx context.Context, t *iampb.Container) error {
	return write(ctx, r.st, domain.KindTenant, t, false, nil)
}

// ProjectRepo stores projects (records are children of their tenant).
type ProjectRepo struct{ st store.Store }

var _ domain.ProjectRepository = (*ProjectRepo)(nil)

// NewProjectRepo returns a ProjectRepo over st.
func NewProjectRepo(st store.Store) *ProjectRepo { return &ProjectRepo{st: st} }

// Get returns the project or errs.NotFound.
func (r *ProjectRepo) Get(ctx context.Context, id string) (*iampb.Container, error) {
	return get(ctx, r.st, domain.KindProject, id, newContainer)
}

// GetByName returns the project named name in tenantID or errs.NotFound.
func (r *ProjectRepo) GetByName(ctx context.Context, tenantID, name string) (*iampb.Container, error) {
	return getByName(ctx, r.st, domain.KindProject, tenantID, name, newContainer)
}

// List returns one page of a tenant's projects in creation order.
func (r *ProjectRepo) List(ctx context.Context, tenantID string, pageSize int, pageToken string) (domain.ContainerPage, error) {
	items, next, err := list(ctx, r.st, domain.KindProject, tenantID, pageSize, pageToken, newContainer)
	if err != nil {
		return domain.ContainerPage{}, err
	}
	return domain.ContainerPage{Items: items, NextPageToken: next}, nil
}

// Ensure inserts p unless a project with the same ID exists.
func (r *ProjectRepo) Ensure(ctx context.Context, p *iampb.Container) error {
	if _, err := r.Get(ctx, p.GetMetadata().GetId()); err == nil {
		return nil
	}
	return write(ctx, r.st, domain.KindProject, p, true, nil)
}

// Create inserts a project; the name must be unique in its tenant.
func (r *ProjectRepo) Create(ctx context.Context, p *iampb.Container) error {
	return write(ctx, r.st, domain.KindProject, p, true, nil)
}

// Update replaces an existing project.
func (r *ProjectRepo) Update(ctx context.Context, p *iampb.Container) error {
	return write(ctx, r.st, domain.KindProject, p, false, nil)
}

// maxNamedChildren bounds how many blocking resources an error names.
const maxNamedChildren = 5

// CheckEmpty fails with errs.BadState naming the resources (records of a kind
// isResource accepts) that still have the project as parent.
func (r *ProjectRepo) CheckEmpty(ctx context.Context, id string, isResource func(kind string) bool) error {
	err := r.st.View(ctx, func(tx store.Tx) error { return checkEmpty(tx, id, isResource) })
	return toDomain(err, id)
}

// DeleteIfEmpty removes the project unless CheckEmpty fails. The check and the
// delete run in one transaction, so a resource created concurrently either
// blocks the delete or no longer finds its parent.
func (r *ProjectRepo) DeleteIfEmpty(ctx context.Context, id string, isResource func(kind string) bool) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		if err := checkEmpty(tx, id, isResource); err != nil {
			return err
		}
		return tx.Delete(domain.KindProject, id)
	})
	return toDomain(err, id)
}

func checkEmpty(tx store.Tx, id string, isResource func(kind string) bool) error {
	if _, err := tx.Get(domain.KindProject, id); err != nil {
		return err
	}
	recs, err := tx.List(store.ListQuery{ParentID: id, AnyKind: true})
	if err != nil {
		return fmt.Errorf("children of %s: %w", id, err)
	}
	var blocking []string
	for _, rec := range recs {
		if isResource(rec.Kind) {
			blocking = append(blocking, rec.ID)
		}
	}
	if len(blocking) == 0 {
		return nil
	}
	named := blocking[:min(len(blocking), maxNamedChildren)]
	more := ""
	if n := len(blocking) - len(named); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	return errs.BadState(id, fmt.Sprintf("project %s is not empty: delete its resources first (%s%s)",
		id, strings.Join(named, ", "), more))
}
