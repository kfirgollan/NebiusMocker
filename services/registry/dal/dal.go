// Package dal implements the Container Registry domain repositories over
// pkg/store. It is the only registry package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
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

// RegistryRepo stores registries.
type RegistryRepo struct{ st store.Store }

var _ domain.RegistryRepository = (*RegistryRepo)(nil)

// NewRegistryRepo returns a RegistryRepo over st.
func NewRegistryRepo(st store.Store) *RegistryRepo { return &RegistryRepo{st: st} }

func newRegistry() *registrypb.Registry { return &registrypb.Registry{} }

// Get returns the registry or errs.NotFound.
func (r *RegistryRepo) Get(ctx context.Context, id string) (*registrypb.Registry, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindRegistry, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decode(rec, newRegistry)
}

// GetByName returns the registry named name in parentID or errs.NotFound.
func (r *RegistryRepo) GetByName(ctx context.Context, parentID, name string) (*registrypb.Registry, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(domain.KindRegistry, parentID, name)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, name)
	}
	return decode(rec, newRegistry)
}

// List returns one page of registries in creation order.
func (r *RegistryRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.RegistryPage, error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.RegistryPage{}, err
	}
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: domain.KindRegistry, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
		return lerr
	})
	if err != nil {
		return domain.RegistryPage{}, fmt.Errorf("list registries in %s: %w", parentID, err)
	}
	var page domain.RegistryPage
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	page.Items = make([]*registrypb.Registry, 0, len(recs))
	for _, rec := range recs {
		m, derr := decode(rec, newRegistry)
		if derr != nil {
			return domain.RegistryPage{}, derr
		}
		page.Items = append(page.Items, m)
	}
	return page, nil
}

// Create inserts a new registry; the name must be unique in its project.
func (r *RegistryRepo) Create(ctx context.Context, reg *registrypb.Registry) error {
	return r.write(ctx, reg, true)
}

// Update replaces an existing registry.
func (r *RegistryRepo) Update(ctx context.Context, reg *registrypb.Registry) error {
	return r.write(ctx, reg, false)
}

func (r *RegistryRepo) write(ctx context.Context, reg *registrypb.Registry, create bool) error {
	md := reg.GetMetadata()
	data, err := marshal.Marshal(reg)
	if err != nil {
		return fmt.Errorf("encode registry %s: %w", md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		_, getErr := tx.Get(domain.KindRegistry, md.GetId())
		switch {
		case create && getErr == nil:
			return errs.AlreadyExists(md.GetId(), "registry already exists")
		case !create && getErr != nil:
			return getErr
		}
		_, putErr := tx.Put(store.Record{
			Kind: domain.KindRegistry, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data,
		})
		return putErr
	})
	return toDomain(err, md.GetId())
}

// Delete removes the registry or returns errs.NotFound.
func (r *RegistryRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindRegistry, id) })
	return toDomain(err, id)
}
