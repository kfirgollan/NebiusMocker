package dal

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// kindInvitationValidity is the internal record kind holding how long an
// invitation was created to stay valid. It is not an authz resource kind.
const kindInvitationValidity = "invitationvalidity"

// InvitationRepo stores invitations (unnamed children of their tenant).
type InvitationRepo struct{ st store.Store }

var _ domain.InvitationRepository = (*InvitationRepo)(nil)

// NewInvitationRepo returns an InvitationRepo over st.
func NewInvitationRepo(st store.Store) *InvitationRepo { return &InvitationRepo{st: st} }

func newInvitation() *iampb.Invitation { return &iampb.Invitation{} }

// Get returns the invitation or errs.NotFound.
func (r *InvitationRepo) Get(ctx context.Context, id string) (*iampb.Invitation, error) {
	return get(ctx, r.st, domain.KindInvitation, id, newInvitation)
}

// List returns one page of a tenant's invitations in creation order.
func (r *InvitationRepo) List(ctx context.Context, tenantID string, pageSize int, pageToken string) (domain.InvitationPage, error) {
	items, next, err := list(ctx, r.st, domain.KindInvitation, tenantID, pageSize, pageToken, newInvitation)
	if err != nil {
		return domain.InvitationPage{}, err
	}
	return domain.InvitationPage{Items: items, NextPageToken: next}, nil
}

// Create inserts the invitation and its validity record in one transaction.
func (r *InvitationRepo) Create(ctx context.Context, inv *iampb.Invitation, validity time.Duration) error {
	id := inv.GetMetadata().GetId()
	var window [8]byte
	binary.BigEndian.PutUint64(window[:], uint64(validity)) //nolint:gosec // validity is validated positive by the domain
	return write(ctx, r.st, domain.KindInvitation, inv, true, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindInvitationValidity, ID: id, ParentID: inv.GetMetadata().GetParentId(), Data: window[:]})
		return err
	})
}

// Update replaces an existing invitation.
func (r *InvitationRepo) Update(ctx context.Context, inv *iampb.Invitation) error {
	return write(ctx, r.st, domain.KindInvitation, inv, false, nil)
}

// Delete removes the invitation and its validity record.
func (r *InvitationRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		if err := tx.Delete(domain.KindInvitation, id); err != nil {
			return err
		}
		return tx.Delete(kindInvitationValidity, id)
	})
	return toDomain(err, id)
}

// Validity returns the window the invitation was created with.
func (r *InvitationRepo) Validity(ctx context.Context, id string) (time.Duration, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(kindInvitationValidity, id)
		return gerr
	})
	if err != nil {
		return 0, toDomain(err, id)
	}
	if len(rec.Data) != 8 {
		return 0, fmt.Errorf("invitation validity %s: malformed record", id)
	}
	return time.Duration(binary.BigEndian.Uint64(rec.Data)), nil //nolint:gosec // written by Create from a positive duration
}
