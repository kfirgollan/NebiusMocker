package dal

import (
	"context"
	"fmt"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// GroupRepo stores IAM groups.
type GroupRepo struct{ st store.Store }

var _ domain.GroupRepository = (*GroupRepo)(nil)

// NewGroupRepo returns a GroupRepo over st.
func NewGroupRepo(st store.Store) *GroupRepo { return &GroupRepo{st: st} }

func newGroup() *iampb.Group { return &iampb.Group{} }

// Get returns the group or errs.NotFound.
func (r *GroupRepo) Get(ctx context.Context, id string) (*iampb.Group, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindGroup, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decode(rec, newGroup)
}

// GetByName returns the group named name in parentID or errs.NotFound.
func (r *GroupRepo) GetByName(ctx context.Context, parentID, name string) (*iampb.Group, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(domain.KindGroup, parentID, name)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, name)
	}
	return decode(rec, newGroup)
}

// List returns one page of groups in creation order.
func (r *GroupRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.GroupPage, error) {
	items, next, err := list(ctx, r.st, domain.KindGroup, parentID, pageSize, pageToken, newGroup)
	if err != nil {
		return domain.GroupPage{}, err
	}
	return domain.GroupPage{Items: items, NextPageToken: next}, nil
}

// Create inserts a new group; the name must be unique in its parent.
func (r *GroupRepo) Create(ctx context.Context, g *iampb.Group) error {
	return r.write(ctx, g, true)
}

// Update replaces an existing group.
func (r *GroupRepo) Update(ctx context.Context, g *iampb.Group) error {
	return r.write(ctx, g, false)
}

// Delete removes the group or returns errs.NotFound.
func (r *GroupRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindGroup, id) })
	return toDomain(err, id)
}

func (r *GroupRepo) write(ctx context.Context, g *iampb.Group, create bool) error {
	md := g.GetMetadata()
	data, err := marshal.Marshal(g)
	if err != nil {
		return fmt.Errorf("encode group %s: %w", md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		_, getErr := tx.Get(domain.KindGroup, md.GetId())
		switch {
		case create && getErr == nil:
			return errs.AlreadyExists(md.GetId(), "group already exists")
		case !create && getErr != nil:
			return getErr
		}
		_, putErr := tx.Put(store.Record{
			Kind: domain.KindGroup, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data,
		})
		return putErr
	})
	return toDomain(err, md.GetId())
}
