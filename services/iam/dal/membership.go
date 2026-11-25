package dal

import (
	"context"
	"fmt"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// MembershipRepo stores group memberships (records are children of their group).
type MembershipRepo struct{ st store.Store }

var _ domain.MembershipRepository = (*MembershipRepo)(nil)

// NewMembershipRepo returns a MembershipRepo over st.
func NewMembershipRepo(st store.Store) *MembershipRepo { return &MembershipRepo{st: st} }

func newMembership() *iampb.GroupMembership { return &iampb.GroupMembership{} }

// Get returns the membership or errs.NotFound.
func (r *MembershipRepo) Get(ctx context.Context, id string) (*iampb.GroupMembership, error) {
	return get(ctx, r.st, domain.KindGroupMembership, id, newMembership)
}

// Create inserts the membership unless the group already has the member.
func (r *MembershipRepo) Create(ctx context.Context, m *iampb.GroupMembership) error {
	return write(ctx, r.st, domain.KindGroupMembership, m, true, func(tx store.Tx) error {
		existing, err := siblings(tx, domain.KindGroupMembership, m.GetMetadata().GetParentId(), newMembership)
		if err != nil {
			return err
		}
		for _, e := range existing {
			if e.GetSpec().GetMemberId() == m.GetSpec().GetMemberId() {
				return errs.AlreadyExists(e.GetMetadata().GetId(), "the group already has this member")
			}
		}
		return nil
	})
}

// Delete removes the membership or returns errs.NotFound.
func (r *MembershipRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindGroupMembership, id)
}

// ListByGroup returns one page of a group's memberships.
func (r *MembershipRepo) ListByGroup(ctx context.Context, groupID string, pageSize int, pageToken string) (domain.MembershipPage, error) {
	items, next, err := list(ctx, r.st, domain.KindGroupMembership, groupID, pageSize, pageToken, newMembership)
	if err != nil {
		return domain.MembershipPage{}, err
	}
	return domain.MembershipPage{Items: items, NextPageToken: next}, nil
}

// ListByMember scans memberships across all groups for memberID.
func (r *MembershipRepo) ListByMember(ctx context.Context, memberID string) ([]*iampb.GroupMembership, error) {
	var out []*iampb.GroupMembership
	err := r.st.View(ctx, func(tx store.Tx) error {
		recs, err := tx.List(store.ListQuery{Kind: domain.KindGroupMembership, AnyParent: true})
		if err != nil {
			return err
		}
		for _, rec := range recs {
			m, err := decode(rec, newMembership)
			if err != nil {
				return err
			}
			if m.GetSpec().GetMemberId() == memberID {
				out = append(out, m)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("memberships of %s: %w", memberID, err)
	}
	return out, nil
}
