package dal

import (
	"context"
	"errors"
	"fmt"

	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// kindAwsKeyIndex maps an AWS access key ID (record ID) to the access key's
// NID (record data). It makes AWS IDs unique and S3 lookups O(1).
const kindAwsKeyIndex = "accesskeyawsid"

// AccessKeyRepo stores access keys, including their secrets.
type AccessKeyRepo struct{ st store.Store }

var _ domain.AccessKeyRepository = (*AccessKeyRepo)(nil)

// NewAccessKeyRepo returns an AccessKeyRepo over st.
func NewAccessKeyRepo(st store.Store) *AccessKeyRepo { return &AccessKeyRepo{st: st} }

func newAccessKey() *iamv2pb.AccessKey { return &iamv2pb.AccessKey{} }

// Get returns the key or errs.NotFound.
func (r *AccessKeyRepo) Get(ctx context.Context, id string) (*iamv2pb.AccessKey, error) {
	return get(ctx, r.st, domain.KindAccessKey, id, newAccessKey)
}

// GetByName returns the key named name in parentID or errs.NotFound.
func (r *AccessKeyRepo) GetByName(ctx context.Context, parentID, name string) (*iamv2pb.AccessKey, error) {
	return getByName(ctx, r.st, domain.KindAccessKey, parentID, name, newAccessKey)
}

// GetByAwsID returns the key with the AWS access key ID or errs.NotFound.
func (r *AccessKeyRepo) GetByAwsID(ctx context.Context, awsID string) (*iamv2pb.AccessKey, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		idx, err := tx.Get(kindAwsKeyIndex, awsID)
		if err != nil {
			return err
		}
		rec, err = tx.Get(domain.KindAccessKey, string(idx.Data))
		return err
	})
	if err != nil {
		return nil, toDomain(err, awsID)
	}
	return decode(rec, newAccessKey)
}

// List returns one page of a parent's keys in creation order.
func (r *AccessKeyRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.AccessKeyPage, error) {
	items, next, err := list(ctx, r.st, domain.KindAccessKey, parentID, pageSize, pageToken, newAccessKey)
	if err != nil {
		return domain.AccessKeyPage{}, err
	}
	return domain.AccessKeyPage{Items: items, NextPageToken: next}, nil
}

// ListByAccount scans every key for those of accountID.
func (r *AccessKeyRepo) ListByAccount(ctx context.Context, accountID string) ([]*iamv2pb.AccessKey, error) {
	var out []*iamv2pb.AccessKey
	err := r.st.View(ctx, func(tx store.Tx) error {
		recs, err := tx.List(store.ListQuery{Kind: domain.KindAccessKey, AnyParent: true})
		if err != nil {
			return err
		}
		for _, rec := range recs {
			k, err := decode(rec, newAccessKey)
			if err != nil {
				return err
			}
			a := k.GetSpec().GetAccount()
			if a.GetServiceAccount().GetId() == accountID || a.GetUserAccount().GetId() == accountID {
				out = append(out, k)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("access keys of %s: %w", accountID, err)
	}
	return out, nil
}

// Create inserts a key and its AWS ID index entry atomically.
func (r *AccessKeyRepo) Create(ctx context.Context, k *iamv2pb.AccessKey) error {
	awsID := k.GetStatus().GetAwsAccessKeyId()
	return write(ctx, r.st, domain.KindAccessKey, k, true, func(tx store.Tx) error {
		if _, err := tx.Get(kindAwsKeyIndex, awsID); err == nil {
			return errs.AlreadyExists(awsID, "the AWS access key ID is already in use")
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		_, err := tx.Put(store.Record{Kind: kindAwsKeyIndex, ID: awsID, Data: []byte(k.GetMetadata().GetId())})
		return err
	})
}

// Update replaces an existing key (the AWS ID never changes).
func (r *AccessKeyRepo) Update(ctx context.Context, k *iamv2pb.AccessKey) error {
	return write(ctx, r.st, domain.KindAccessKey, k, false, nil)
}

// Delete removes the key and its index entry, or returns errs.NotFound.
func (r *AccessKeyRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		rec, err := tx.Get(domain.KindAccessKey, id)
		if err != nil {
			return err
		}
		k, err := decode(rec, newAccessKey)
		if err != nil {
			return err
		}
		if err := tx.Delete(kindAwsKeyIndex, k.GetStatus().GetAwsAccessKeyId()); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return tx.Delete(domain.KindAccessKey, id)
	})
	return toDomain(err, id)
}
