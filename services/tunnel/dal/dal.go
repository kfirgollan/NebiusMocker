// Package dal implements the Tunnels domain repositories over pkg/store.
// It is the only Tunnels package that touches storage.
package dal

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// TunnelRepo stores tunnels as proto-marshaled records. Tunnels are
// unnamed: the store record carries no name, so nothing is unique but the ID.
type TunnelRepo struct{ st store.Store }

var _ domain.TunnelRepository = (*TunnelRepo)(nil)

// NewTunnelRepo returns the tunnel repository over st.
func NewTunnelRepo(st store.Store) *TunnelRepo { return &TunnelRepo{st: st} }

// toDomain maps storage errors to domain error kinds.
func toDomain(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return errs.NotFound(id)
	default:
		return err
	}
}

func decode(rec store.Record) (*tunnelpb.Tunnel, error) {
	m := &tunnelpb.Tunnel{}
	if err := proto.Unmarshal(rec.Data, m); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return m, nil
}

// Get returns the tunnel or errs.NotFound.
func (r *TunnelRepo) Get(ctx context.Context, id string) (*tunnelpb.Tunnel, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindTunnel, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decode(rec)
}

// List returns every tunnel of the project in creation order.
func (r *TunnelRepo) List(ctx context.Context, parentID string) ([]*tunnelpb.Tunnel, error) {
	return r.query(ctx, store.ListQuery{Kind: domain.KindTunnel, ParentID: parentID})
}

func (r *TunnelRepo) query(ctx context.Context, q store.ListQuery) ([]*tunnelpb.Tunnel, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(q)
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", domain.KindTunnel, err)
	}
	out := make([]*tunnelpb.Tunnel, 0, len(recs))
	for _, rec := range recs {
		m, derr := decode(rec)
		if derr != nil {
			return nil, derr
		}
		out = append(out, m)
	}
	return out, nil
}

// Create inserts a new tunnel; errs.AlreadyExists if the ID exists.
func (r *TunnelRepo) Create(ctx context.Context, m *tunnelpb.Tunnel) error {
	return r.write(ctx, m, true)
}

// Update replaces an existing tunnel; errs.NotFound if it does not exist.
func (r *TunnelRepo) Update(ctx context.Context, m *tunnelpb.Tunnel) error {
	return r.write(ctx, m, false)
}

func (r *TunnelRepo) write(ctx context.Context, m *tunnelpb.Tunnel, create bool) error {
	md := m.GetMetadata()
	data, err := marshal.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", domain.KindTunnel, md.GetId(), err)
	}
	rec := store.Record{Kind: domain.KindTunnel, ID: md.GetId(), ParentID: md.GetParentId(), Data: data}
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

// Delete removes the tunnel or returns errs.NotFound.
func (r *TunnelRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindTunnel, id) })
	return toDomain(err, id)
}
