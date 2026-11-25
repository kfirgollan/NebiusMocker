package dal

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

// resource is a stored proto with standard metadata.
type resource interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// protoRepo stores one resource kind as proto records. Disks and snapshots
// share it; noun names the kind in error messages.
type protoRepo[T resource] struct {
	st   store.Store
	kind string
	noun string
	newT func() T
}

func (r protoRepo[T]) get(ctx context.Context, id string) (T, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(r.kind, id)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, r.toDomain(err, id)
	}
	return r.decode(rec)
}

func (r protoRepo[T]) getByName(ctx context.Context, parentID, name string) (T, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(r.kind, parentID, name)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, r.toDomain(err, name)
	}
	return r.decode(rec)
}

// list returns up to pageSize items of parentID in creation order that match
// keep (nil keeps all), and the token of the next page.
func (r protoRepo[T]) list(ctx context.Context, parentID string, pageSize int, pageToken string, keep func(T) bool) (items []T, next string, err error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: r.kind, ParentID: parentID, AfterSeq: after})
		return lerr
	})
	if err != nil {
		return nil, "", fmt.Errorf("list %ss in %s: %w", r.noun, parentID, err)
	}
	items = make([]T, 0, min(len(recs), pageSize))
	var lastSeq uint64
	for _, rec := range recs {
		item, err := r.decode(rec)
		if err != nil {
			return nil, "", err
		}
		if keep != nil && !keep(item) {
			continue
		}
		if len(items) == pageSize {
			// There is at least one more match: the page ends at the last item kept.
			return items, encodePageToken(lastSeq), nil
		}
		items = append(items, item)
		lastSeq = rec.Seq
	}
	return items, "", nil
}

func (r protoRepo[T]) create(ctx context.Context, item T) error {
	md := item.GetMetadata()
	data, err := marshal.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", r.noun, md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, gerr := tx.Get(r.kind, md.GetId()); gerr == nil {
			return errs.AlreadyExists(md.GetId(), r.noun+" already exists")
		}
		_, perr := tx.Put(store.Record{Kind: r.kind, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data})
		return perr
	})
	return r.toDomain(err, md.GetId())
}

// mutate runs get, fn and put in one store transaction.
func (r protoRepo[T]) mutate(ctx context.Context, id string, fn func(T) error) (T, error) {
	var out T
	err := r.st.Update(ctx, func(tx store.Tx) error {
		rec, gerr := tx.Get(r.kind, id)
		if gerr != nil {
			return gerr
		}
		item, derr := r.decode(rec)
		if derr != nil {
			return derr
		}
		if ferr := fn(item); ferr != nil {
			return ferr
		}
		md := item.GetMetadata()
		data, merr := marshal.Marshal(item)
		if merr != nil {
			return fmt.Errorf("encode %s %s: %w", r.noun, id, merr)
		}
		if _, perr := tx.Put(store.Record{Kind: r.kind, ID: id, ParentID: md.GetParentId(), Name: md.GetName(), Data: data}); perr != nil {
			return perr
		}
		out = item
		return nil
	})
	if err != nil {
		var zero T
		return zero, r.toDomain(err, id)
	}
	return out, nil
}

func (r protoRepo[T]) delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(r.kind, id) })
	return r.toDomain(err, id)
}

func (r protoRepo[T]) decode(rec store.Record) (T, error) {
	item := r.newT()
	if err := proto.Unmarshal(rec.Data, item); err != nil {
		var zero T
		return zero, fmt.Errorf("decode %s %s: %w", r.noun, rec.ID, err)
	}
	return item, nil
}

// toDomain maps storage errors to domain error kinds. Domain errors returned
// by mutate callbacks pass through unchanged.
func (r protoRepo[T]) toDomain(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return &errs.Error{Kind: errs.KindNotFound, ResourceID: id, Message: r.noun + " " + id + " not found"}
	case errors.Is(err, store.ErrNameTaken):
		existing := ""
		if nt, ok := errors.AsType[*store.NameTakenError](err); ok {
			existing = nt.ExistingID
		}
		return errs.AlreadyExists(existing, "a "+r.noun+" with this name already exists in the parent")
	default:
		return err
	}
}
