package dal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// kindLastAccess holds the last read of an object, per bucket.
const kindLastAccess = "storagelastaccess"

// AccessLogRepo is the read log behind the LAST_ACCESS inventory field.
type AccessLogRepo struct{ st store.Store }

var _ domain.AccessLog = (*AccessLogRepo)(nil)

// NewAccessLogRepo returns an AccessLogRepo over st.
func NewAccessLogRepo(st store.Store) *AccessLogRepo { return &AccessLogRepo{st: st} }

// accessID is the record ID of one object: the hash keeps any key valid.
func accessID(bucketID, key string) string {
	sum := sha256.Sum256([]byte(bucketID + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// Touch records that key in bucketID was read at t.
func (r *AccessLogRepo) Touch(ctx context.Context, bucketID, key string, t time.Time) error {
	rec := store.Record{Kind: kindLastAccess, ID: accessID(bucketID, key), ParentID: bucketID, Data: []byte(t.UTC().Format(time.RFC3339Nano))}
	if err := r.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(rec)
		return err
	}); err != nil {
		return fmt.Errorf("record read of %q: %w", key, err)
	}
	return nil
}

// LastAccess returns the last recorded read of key, ok=false when none.
func (r *AccessLogRepo) LastAccess(ctx context.Context, bucketID, key string) (time.Time, bool, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		rec, err = tx.Get(kindLastAccess, accessID(bucketID, key))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("last access of %q: %w", key, err)
	}
	t, err := time.Parse(time.RFC3339Nano, string(rec.Data))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("decode last access of %q: %w", key, err)
	}
	return t, true, nil
}

// Forget drops every record of a bucket.
func (r *AccessLogRepo) Forget(ctx context.Context, bucketID string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		recs, err := tx.List(store.ListQuery{Kind: kindLastAccess, ParentID: bucketID})
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if err := tx.Delete(kindLastAccess, rec.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("forget reads of bucket %s: %w", bucketID, err)
	}
	return nil
}
