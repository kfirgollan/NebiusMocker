// Package dal implements the DNS domain repositories over pkg/store. It is
// the only DNS package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// resource is the common shape of Zone and Record.
type resource interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// Repo stores one DNS resource kind as proto-marshaled records. Names are
// unique per parent when set.
type Repo[T resource] struct {
	st     store.Store
	kind   string
	newMsg func() T
}

var (
	_ domain.ZoneRepository   = (*ZoneRepo)(nil)
	_ domain.RecordRepository = (*RecordRepo)(nil)
)

// ZoneRepo stores zones.
type ZoneRepo struct{ Repo[*dnspb.Zone] }

// RecordRepo stores records.
type RecordRepo struct{ Repo[*dnspb.Record] }

// NewZoneRepo returns the zone repository over st.
func NewZoneRepo(st store.Store) *ZoneRepo {
	return &ZoneRepo{Repo[*dnspb.Zone]{st: st, kind: domain.KindZone, newMsg: func() *dnspb.Zone { return &dnspb.Zone{} }}}
}

// NewRecordRepo returns the record repository over st.
func NewRecordRepo(st store.Store) *RecordRepo {
	return &RecordRepo{Repo[*dnspb.Record]{st: st, kind: domain.KindRecord, newMsg: func() *dnspb.Record { return &dnspb.Record{} }}}
}

// All returns every zone of every project in creation order.
func (r *ZoneRepo) All(ctx context.Context) ([]*dnspb.Zone, error) {
	return r.query(ctx, store.ListQuery{Kind: r.kind, AnyParent: true})
}

// All returns every record of the zone in creation order.
func (r *RecordRepo) All(ctx context.Context, zoneID string) ([]*dnspb.Record, error) {
	return r.query(ctx, store.ListQuery{Kind: r.kind, ParentID: zoneID})
}

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

func (r *Repo[T]) decode(rec store.Record) (T, error) {
	m := r.newMsg()
	if err := proto.Unmarshal(rec.Data, m); err != nil {
		var zero T
		return zero, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return m, nil
}

// Get returns the resource or errs.NotFound.
func (r *Repo[T]) Get(ctx context.Context, id string) (T, error) {
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
	return r.decode(rec)
}

// GetByName returns the resource named name in parentID, or errs.NotFound.
func (r *Repo[T]) GetByName(ctx context.Context, parentID, name string) (T, error) {
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
	return r.decode(rec)
}

// List returns one page of the parent's resources in creation order.
func (r *Repo[T]) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.Page[T], error) {
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
	page.Items, err = r.decodeAll(recs)
	return page, err
}

func (r *Repo[T]) query(ctx context.Context, q store.ListQuery) ([]T, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(q)
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", r.kind, err)
	}
	return r.decodeAll(recs)
}

func (r *Repo[T]) decodeAll(recs []store.Record) ([]T, error) {
	out := make([]T, 0, len(recs))
	for _, rec := range recs {
		m, err := r.decode(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Create inserts a new resource; errs.AlreadyExists if the ID exists or the
// name is taken in the parent.
func (r *Repo[T]) Create(ctx context.Context, m T) error { return r.write(ctx, m, true) }

// Update replaces an existing resource; errs.NotFound if it does not exist.
func (r *Repo[T]) Update(ctx context.Context, m T) error { return r.write(ctx, m, false) }

func (r *Repo[T]) write(ctx context.Context, m T, create bool) error {
	md := m.GetMetadata()
	data, err := marshal.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", r.kind, md.GetId(), err)
	}
	rec := store.Record{Kind: r.kind, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data}
	err = r.st.Update(ctx, func(tx store.Tx) error {
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

// Delete removes the resource or returns errs.NotFound.
func (r *Repo[T]) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(r.kind, id) })
	return toDomain(err, id)
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
