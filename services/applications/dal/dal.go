// Package dal implements the Applications domain repositories over pkg/store.
// It is the only Applications package that touches storage.
package dal

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/applications/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// ReleaseRepo stores K8s releases as proto-marshaled records. Releases are
// unnamed: the store record carries no name, so nothing is unique but the ID.
type ReleaseRepo struct{ st store.Store }

var _ domain.ReleaseRepository = (*ReleaseRepo)(nil)

// NewReleaseRepo returns the release repository over st.
func NewReleaseRepo(st store.Store) *ReleaseRepo { return &ReleaseRepo{st: st} }

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

func decode(rec store.Record) (*applicationsv1alpha1pb.K8SRelease, error) {
	m := &applicationsv1alpha1pb.K8SRelease{}
	if err := proto.Unmarshal(rec.Data, m); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return m, nil
}

// Get returns the release or errs.NotFound.
func (r *ReleaseRepo) Get(ctx context.Context, id string) (*applicationsv1alpha1pb.K8SRelease, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindRelease, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decode(rec)
}

// List returns every release of the project in creation order.
func (r *ReleaseRepo) List(ctx context.Context, parentID string) ([]*applicationsv1alpha1pb.K8SRelease, error) {
	return r.query(ctx, store.ListQuery{Kind: domain.KindRelease, ParentID: parentID})
}

// All returns every release of every project in creation order.
func (r *ReleaseRepo) All(ctx context.Context) ([]*applicationsv1alpha1pb.K8SRelease, error) {
	return r.query(ctx, store.ListQuery{Kind: domain.KindRelease, AnyParent: true})
}

func (r *ReleaseRepo) query(ctx context.Context, q store.ListQuery) ([]*applicationsv1alpha1pb.K8SRelease, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(q)
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", domain.KindRelease, err)
	}
	out := make([]*applicationsv1alpha1pb.K8SRelease, 0, len(recs))
	for _, rec := range recs {
		m, derr := decode(rec)
		if derr != nil {
			return nil, derr
		}
		out = append(out, m)
	}
	return out, nil
}

// Create inserts a new release; errs.AlreadyExists if the ID exists.
func (r *ReleaseRepo) Create(ctx context.Context, m *applicationsv1alpha1pb.K8SRelease) error {
	return r.write(ctx, m, true)
}

// Update replaces an existing release; errs.NotFound if it does not exist.
func (r *ReleaseRepo) Update(ctx context.Context, m *applicationsv1alpha1pb.K8SRelease) error {
	return r.write(ctx, m, false)
}

func (r *ReleaseRepo) write(ctx context.Context, m *applicationsv1alpha1pb.K8SRelease, create bool) error {
	md := m.GetMetadata()
	data, err := marshal.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", domain.KindRelease, md.GetId(), err)
	}
	rec := store.Record{Kind: domain.KindRelease, ID: md.GetId(), ParentID: md.GetParentId(), Data: data}
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

// Delete removes the release or returns errs.NotFound.
func (r *ReleaseRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindRelease, id) })
	return toDomain(err, id)
}
