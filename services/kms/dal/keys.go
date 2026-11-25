package dal

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/kms/domain"
)

var (
	_ domain.SymmetricKeyRepository  = (*KeyRepo[*kmspb.SymmetricKey])(nil)
	_ domain.AsymmetricKeyRepository = (*KeyRepo[*kmspb.AsymmetricKey])(nil)
)

// keyMessage is implemented by both KMS key messages.
type keyMessage interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// KeyRepo stores keys of one kind plus their secret material. The material
// lives in a separate record kind so it never appears in key listings.
type KeyRepo[T keyMessage] struct {
	st           store.Store
	kind         string
	materialKind string
	newMsg       func() T
	active       func(T) bool
}

// NewSymmetricKeyRepo returns a repository of symmetric keys over st.
func NewSymmetricKeyRepo(st store.Store) *KeyRepo[*kmspb.SymmetricKey] {
	return newKeyRepo(st, domain.KindSymmetricKey,
		func() *kmspb.SymmetricKey { return &kmspb.SymmetricKey{} },
		func(k *kmspb.SymmetricKey) bool { return k.GetStatus().GetState() == kmspb.KeyState_ACTIVE })
}

// NewAsymmetricKeyRepo returns a repository of asymmetric keys over st.
func NewAsymmetricKeyRepo(st store.Store) *KeyRepo[*kmspb.AsymmetricKey] {
	return newKeyRepo(st, domain.KindAsymmetricKey,
		func() *kmspb.AsymmetricKey { return &kmspb.AsymmetricKey{} },
		func(k *kmspb.AsymmetricKey) bool { return k.GetStatus().GetState() == kmspb.KeyState_ACTIVE })
}

func newKeyRepo[T keyMessage](st store.Store, kind string, newMsg func() T, active func(T) bool) *KeyRepo[T] {
	return &KeyRepo[T]{st: st, kind: kind, materialKind: kind + ".material", newMsg: newMsg, active: active}
}

// record builds the store record for key; only ACTIVE keys are name-indexed.
func (r *KeyRepo[T]) record(key T) (store.Record, error) {
	md := key.GetMetadata()
	data, err := marshal.Marshal(key)
	if err != nil {
		return store.Record{}, fmt.Errorf("encode %s %s: %w", r.kind, md.GetId(), err)
	}
	name := ""
	if r.active(key) {
		name = md.GetName()
	}
	return store.Record{Kind: r.kind, ID: md.GetId(), ParentID: md.GetParentId(), Name: name, Data: data}, nil
}

func (r *KeyRepo[T]) materialRecord(id string, material []byte) store.Record {
	return store.Record{Kind: r.materialKind, ID: id, ParentID: id, Data: material}
}

// Get returns the key in any state, or errs.NotFound.
func (r *KeyRepo[T]) Get(ctx context.Context, id string) (T, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(r.kind, id)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, toDomain(err, id)
	}
	return decode(rec, r.newMsg)
}

// GetByName returns the ACTIVE key named name in parentID, or errs.NotFound.
func (r *KeyRepo[T]) GetByName(ctx context.Context, parentID, name string) (T, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(r.kind, parentID, name)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, toDomain(err, name)
	}
	return decode(rec, r.newMsg)
}

// List returns one page of the parent's keys in creation order.
func (r *KeyRepo[T]) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.Page[T], error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.Page[T]{}, err
	}
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: r.kind, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
		return lerr
	})
	if err != nil {
		return domain.Page[T]{}, fmt.Errorf("list %s in %s: %w", r.kind, parentID, err)
	}
	var page domain.Page[T]
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	page.Items = make([]T, 0, len(recs))
	for _, rec := range recs {
		m, derr := decode(rec, r.newMsg)
		if derr != nil {
			return domain.Page[T]{}, derr
		}
		page.Items = append(page.Items, m)
	}
	return page, nil
}

// Create stores a new key and its material in one transaction.
func (r *KeyRepo[T]) Create(ctx context.Context, key T, material []byte) error {
	id := key.GetMetadata().GetId()
	rec, err := r.record(key)
	if err != nil {
		return err
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, gerr := tx.Get(r.kind, id); gerr == nil {
			return errs.AlreadyExists(id, r.kind+" already exists")
		}
		if _, perr := tx.Put(rec); perr != nil {
			return perr
		}
		_, perr := tx.Put(r.materialRecord(id, material))
		return perr
	})
	return toDomain(err, id)
}

// Update replaces an existing key, leaving its material untouched.
func (r *KeyRepo[T]) Update(ctx context.Context, key T) error {
	id := key.GetMetadata().GetId()
	rec, err := r.record(key)
	if err != nil {
		return err
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, gerr := tx.Get(r.kind, id); gerr != nil {
			return gerr
		}
		_, perr := tx.Put(rec)
		return perr
	})
	return toDomain(err, id)
}

// Material returns the key's material, or errs.NotFound.
func (r *KeyRepo[T]) Material(ctx context.Context, id string) ([]byte, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(r.materialKind, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return rec.Data, nil
}

// UpdateWithMaterial replaces an existing key and its material in one
// transaction, if the stored key's resource_version is still prevVersion
// (compare-and-swap). A mismatch is errs.Conflict.
func (r *KeyRepo[T]) UpdateWithMaterial(ctx context.Context, key T, material []byte, prevVersion int64) error {
	id := key.GetMetadata().GetId()
	rec, err := r.record(key)
	if err != nil {
		return err
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		cur, gerr := tx.Get(r.kind, id)
		if gerr != nil {
			return gerr
		}
		stored, derr := decode(cur, r.newMsg)
		if derr != nil {
			return derr
		}
		if v := stored.GetMetadata().GetResourceVersion(); v != prevVersion {
			return errs.Conflict(id, fmt.Sprintf("resource_version is %d, not %d", v, prevVersion))
		}
		if _, perr := tx.Put(rec); perr != nil {
			return perr
		}
		_, perr := tx.Put(r.materialRecord(id, material))
		return perr
	})
	return toDomain(err, id)
}

// Delete removes the key and its material.
func (r *KeyRepo[T]) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		if derr := tx.Delete(r.kind, id); derr != nil {
			return derr
		}
		if derr := tx.Delete(r.materialKind, id); derr != nil && !isNotFound(derr) {
			return derr
		}
		return nil
	})
	return toDomain(err, id)
}
