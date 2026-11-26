package dal

import (
	"context"
	"errors"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/vpc/domain"
)

// kindNetworkUser is the store kind of a binding of another service's
// resource to a network. It is not a NID type: the record ID is
// "<network>/<user>", its parent the network ID, its data the user ID.
const kindNetworkUser = "vpcnetwork.user"

// NetworkUserRepo implements domain.NetworkUserRepository over the store.
type NetworkUserRepo struct{ st store.Store }

var _ domain.NetworkUserRepository = (*NetworkUserRepo)(nil)

// NewNetworkUserRepo returns the network binding repository over st.
func NewNetworkUserRepo(st store.Store) *NetworkUserRepo { return &NetworkUserRepo{st: st} }

func networkUserID(networkID, userID string) string { return networkID + "/" + userID }

// Add implements domain.NetworkUserRepository.
func (r *NetworkUserRepo) Add(ctx context.Context, networkID, userID string) error {
	return r.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindNetworkUser, ID: networkUserID(networkID, userID), ParentID: networkID, Data: []byte(userID)})
		return err
	})
}

// Remove implements domain.NetworkUserRepository.
func (r *NetworkUserRepo) Remove(ctx context.Context, networkID, userID string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(kindNetworkUser, networkUserID(networkID, userID)) })
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// List implements domain.NetworkUserRepository.
func (r *NetworkUserRepo) List(ctx context.Context, networkID string) ([]string, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: kindNetworkUser, ParentID: networkID})
		return lerr
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, string(rec.Data))
	}
	return out, nil
}
