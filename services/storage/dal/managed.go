package dal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// kindTransferManaged records the destination objects a transfer wrote:
// ID = hex sha256(destination + "\x00" + key), data = the ETag it wrote.
const kindTransferManaged = "storagetransfermanaged"

// ManagedRepo implements domain.ManagedObjects over the store.
type ManagedRepo struct{ st store.Store }

var _ domain.ManagedObjects = (*ManagedRepo)(nil)

// NewManagedRepo returns a ManagedRepo over st.
func NewManagedRepo(st store.Store) *ManagedRepo { return &ManagedRepo{st: st} }

func managedID(destination, key string) string {
	sum := sha256.Sum256([]byte(destination + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// Managed reports whether key at destination still has the ETag a transfer wrote.
func (r *ManagedRepo) Managed(ctx context.Context, destination, key, etag string) (bool, error) {
	var managed bool
	err := r.st.View(ctx, func(tx store.Tx) error {
		rec, err := tx.Get(kindTransferManaged, managedID(destination, key))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		managed = string(rec.Data) == etag
		return nil
	})
	return managed, err
}

// MarkManaged records that a transfer wrote key at destination with etag.
func (r *ManagedRepo) MarkManaged(ctx context.Context, destination, key, etag string) error {
	return r.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindTransferManaged, ID: managedID(destination, key), Data: []byte(etag)})
		return err
	})
}

// Forget drops the record of key at destination (idempotent).
func (r *ManagedRepo) Forget(ctx context.Context, destination, key string) error {
	return r.st.Update(ctx, func(tx store.Tx) error {
		if err := tx.Delete(kindTransferManaged, managedID(destination, key)); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	})
}
