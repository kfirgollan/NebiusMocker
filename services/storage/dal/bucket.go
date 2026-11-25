// Package dal implements the Object Storage repositories over pkg/store. It
// is the only storage package that touches the store.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// kindBucketName is the global name index: record ID = bucket name, data =
// bucket NID. Bucket names are unique across the mocker (S3 DNS semantics).
const kindBucketName = "storagebucketname"

var marshal = proto.MarshalOptions{Deterministic: true}

// BucketRepo stores buckets (records are children of their project).
type BucketRepo struct{ st store.Store }

var _ domain.BucketRepository = (*BucketRepo)(nil)

// NewBucketRepo returns a BucketRepo over st.
func NewBucketRepo(st store.Store) *BucketRepo { return &BucketRepo{st: st} }

// Get returns the bucket or errs.NotFound.
func (r *BucketRepo) Get(ctx context.Context, id string) (*storagepb.Bucket, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		rec, err = tx.Get(domain.KindBucket, id)
		return err
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decode(rec)
}

// GetByName returns the bucket named name in project parentID.
func (r *BucketRepo) GetByName(ctx context.Context, parentID, name string) (*storagepb.Bucket, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		rec, err = tx.ByName(domain.KindBucket, parentID, name)
		return err
	})
	if err != nil {
		return nil, toDomain(err, name)
	}
	return decode(rec)
}

// GetByBucketName returns the bucket with the global name.
func (r *BucketRepo) GetByBucketName(ctx context.Context, name string) (*storagepb.Bucket, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		idx, err := tx.Get(kindBucketName, name)
		if err != nil {
			return err
		}
		rec, err = tx.Get(domain.KindBucket, string(idx.Data))
		return err
	})
	if err != nil {
		return nil, toDomain(err, name)
	}
	return decode(rec)
}

// List returns one page of a project's buckets in creation order.
func (r *BucketRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.BucketPage, error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.BucketPage{}, err
	}
	recs, err := r.records(ctx, store.ListQuery{Kind: domain.KindBucket, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
	if err != nil {
		return domain.BucketPage{}, err
	}
	var page domain.BucketPage
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	if page.Items, err = decodeAll(recs); err != nil {
		return domain.BucketPage{}, err
	}
	return page, nil
}

// ListAll returns every bucket in creation order.
func (r *BucketRepo) ListAll(ctx context.Context) ([]*storagepb.Bucket, error) {
	recs, err := r.records(ctx, store.ListQuery{Kind: domain.KindBucket, AnyParent: true})
	if err != nil {
		return nil, err
	}
	return decodeAll(recs)
}

func (r *BucketRepo) records(ctx context.Context, q store.ListQuery) ([]store.Record, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		recs, err = tx.List(q)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	return recs, nil
}

// Create inserts a bucket and reserves its global name atomically.
func (r *BucketRepo) Create(ctx context.Context, b *storagepb.Bucket) error {
	md := b.GetMetadata()
	data, err := marshal.Marshal(b)
	if err != nil {
		return fmt.Errorf("encode bucket %s: %w", md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if idx, err := tx.Get(kindBucketName, md.GetName()); err == nil {
			return errs.AlreadyExists(string(idx.Data), "a bucket with this name already exists")
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := tx.Put(store.Record{Kind: kindBucketName, ID: md.GetName(), Data: []byte(md.GetId())}); err != nil {
			return err
		}
		_, err := tx.Put(store.Record{Kind: domain.KindBucket, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data})
		return err
	})
	return toDomain(err, md.GetId())
}

// Update replaces an existing bucket (names never change).
func (r *BucketRepo) Update(ctx context.Context, b *storagepb.Bucket) error {
	md := b.GetMetadata()
	data, err := marshal.Marshal(b)
	if err != nil {
		return fmt.Errorf("encode bucket %s: %w", md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.Get(domain.KindBucket, md.GetId()); err != nil {
			return err
		}
		_, err := tx.Put(store.Record{Kind: domain.KindBucket, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data})
		return err
	})
	return toDomain(err, md.GetId())
}

// Delete removes the bucket and frees its name.
func (r *BucketRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		rec, err := tx.Get(domain.KindBucket, id)
		if err != nil {
			return err
		}
		if err := tx.Delete(kindBucketName, rec.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return tx.Delete(domain.KindBucket, id)
	})
	return toDomain(err, id)
}

func toDomain(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return errs.NotFound(id)
	case errors.Is(err, store.ErrNameTaken):
		existing := ""
		if nt, ok := errors.AsType[*store.NameTakenError](err); ok {
			existing = nt.ExistingID
		}
		return errs.AlreadyExists(existing, "a bucket with this name already exists")
	default:
		return err
	}
}

func decode(rec store.Record) (*storagepb.Bucket, error) {
	b := &storagepb.Bucket{}
	if err := proto.Unmarshal(rec.Data, b); err != nil {
		return nil, fmt.Errorf("decode bucket %s: %w", rec.ID, err)
	}
	return b, nil
}

func decodeAll(recs []store.Record) ([]*storagepb.Bucket, error) {
	out := make([]*storagepb.Bucket, 0, len(recs))
	for _, rec := range recs {
		b, err := decode(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func encodePageToken(seq uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(seq, 10)))
}

func decodePageToken(token string) (uint64, error) {
	if token == "" {
		return 0, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(token); err == nil {
		if seq, err := strconv.ParseUint(string(b), 10, 64); err == nil {
			return seq, nil
		}
	}
	return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
}
