// Package dal implements the MysteryBox domain repositories over pkg/store.
// It is the only MysteryBox package that touches storage. Payload records
// hold ciphertext only.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// toDomain maps storage errors to domain error kinds.
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
		return errs.AlreadyExists(existing, "a resource with this name already exists in the parent")
	default:
		return err
	}
}

func decode[T proto.Message](rec store.Record, newMsg func() T) (T, error) {
	m := newMsg()
	if err := proto.Unmarshal(rec.Data, m); err != nil {
		var zero T
		return zero, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return m, nil
}

func encodePageToken(seq uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(seq, 10)))
}

func decodePageToken(token string) (uint64, error) {
	if token == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err == nil {
		if seq, perr := strconv.ParseUint(string(b), 10, 64); perr == nil {
			return seq, nil
		}
	}
	return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
}

// list reads one page of kind under parentID (pageSize 0: every record).
func list[T proto.Message](ctx context.Context, st store.Store, kind, parentID string, pageSize int, pageToken string, newMsg func() T) (items []T, next string, err error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	limit := 0
	if pageSize > 0 {
		limit = pageSize + 1
	}
	var recs []store.Record
	err = st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: kind, ParentID: parentID, AfterSeq: after, Limit: limit})
		return lerr
	})
	if err != nil {
		return nil, "", fmt.Errorf("list %s in %s: %w", kind, parentID, err)
	}
	if pageSize > 0 && len(recs) > pageSize {
		recs = recs[:pageSize]
		next = encodePageToken(recs[len(recs)-1].Seq)
	}
	items = make([]T, 0, len(recs))
	for _, r := range recs {
		m, derr := decode(r, newMsg)
		if derr != nil {
			return nil, "", derr
		}
		items = append(items, m)
	}
	return items, next, nil
}

// metadataOf is implemented by every Nebius resource message.
type metadataOf interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// get reads one record of kind by ID.
func get[T proto.Message](ctx context.Context, st store.Store, kind, id string, newMsg func() T) (T, error) {
	var rec store.Record
	err := st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(kind, id)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, toDomain(err, id)
	}
	return decode(rec, newMsg)
}

// write stores msg under indexName (the name the store keeps unique per
// parent; "" for none). With create it fails if the ID exists; otherwise the
// ID must exist.
func write(ctx context.Context, st store.Store, kind string, msg metadataOf, indexName string, create bool) error {
	md := msg.GetMetadata()
	data, err := marshal.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", kind, md.GetId(), err)
	}
	return putRecord(ctx, st, store.Record{Kind: kind, ID: md.GetId(), ParentID: md.GetParentId(), Name: indexName, Data: data}, create)
}

func putRecord(ctx context.Context, st store.Store, rec store.Record, create bool) error {
	err := st.Update(ctx, func(tx store.Tx) error {
		_, getErr := tx.Get(rec.Kind, rec.ID)
		switch {
		case create && getErr == nil:
			return errs.AlreadyExists(rec.ID, rec.Kind+" already exists")
		case !create && getErr != nil:
			return getErr
		}
		_, putErr := tx.Put(rec)
		return putErr
	})
	return toDomain(err, rec.ID)
}

// remove deletes one record of kind.
func remove(ctx context.Context, st store.Store, kind, id string) error {
	err := st.Update(ctx, func(tx store.Tx) error { return tx.Delete(kind, id) })
	return toDomain(err, id)
}
