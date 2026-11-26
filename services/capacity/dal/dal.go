// Package dal implements the capacity domain repositories over pkg/store. It
// is the only capacity package that touches storage.
package dal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/capacity/domain"
)

// kindUsage is the store kind of consumer reservations.
const kindUsage = "capacityusage"

var marshal = proto.MarshalOptions{Deterministic: true}

var (
	_ domain.AllowanceRepository = (*AllowanceRepo)(nil)
	_ domain.UsageRepository     = (*UsageRepo)(nil)
)

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

// AllowanceRepo stores capacity allowances. Allowances are unnamed: the
// metadata name is not unique, so it never reaches the store record.
type AllowanceRepo struct{ st store.Store }

// NewAllowanceRepo returns the allowance repository over st.
func NewAllowanceRepo(st store.Store) *AllowanceRepo { return &AllowanceRepo{st: st} }

func decodeAllowance(rec store.Record) (*capacitypb.CapacityAllowance, error) {
	a := &capacitypb.CapacityAllowance{}
	if err := proto.Unmarshal(rec.Data, a); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return a, nil
}

// Get returns the allowance or errs.NotFound.
func (r *AllowanceRepo) Get(ctx context.Context, id string) (*capacitypb.CapacityAllowance, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindAllowance, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decodeAllowance(rec)
}

// ByProject returns every allowance of the project in creation order.
func (r *AllowanceRepo) ByProject(ctx context.Context, projectID string) ([]*capacitypb.CapacityAllowance, error) {
	return r.query(ctx, store.ListQuery{Kind: domain.KindAllowance, ParentID: projectID}, "")
}

// ByBlockGroup returns every allowance of the block group, of any project,
// in creation order.
func (r *AllowanceRepo) ByBlockGroup(ctx context.Context, blockGroupID string) ([]*capacitypb.CapacityAllowance, error) {
	return r.query(ctx, store.ListQuery{Kind: domain.KindAllowance, AnyParent: true}, blockGroupID)
}

// query lists allowances, keeping only those of blockGroupID when it is set.
func (r *AllowanceRepo) query(ctx context.Context, q store.ListQuery, blockGroupID string) ([]*capacitypb.CapacityAllowance, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(q)
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", q.Kind, err)
	}
	out := make([]*capacitypb.CapacityAllowance, 0, len(recs))
	for _, rec := range recs {
		a, derr := decodeAllowance(rec)
		if derr != nil {
			return nil, derr
		}
		if blockGroupID != "" && a.GetSpec().GetCapacityBlockGroupId() != blockGroupID {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// Create inserts an allowance; errs.AlreadyExists if the ID exists or the
// project already has an allowance for the block group.
func (r *AllowanceRepo) Create(ctx context.Context, a *capacitypb.CapacityAllowance) error {
	return r.write(ctx, a, true)
}

// Update replaces an existing allowance; errs.NotFound if it does not exist.
func (r *AllowanceRepo) Update(ctx context.Context, a *capacitypb.CapacityAllowance) error {
	return r.write(ctx, a, false)
}

func (r *AllowanceRepo) write(ctx context.Context, a *capacitypb.CapacityAllowance, create bool) error {
	id := a.GetMetadata().GetId()
	parent := a.GetMetadata().GetParentId()
	cbg := a.GetSpec().GetCapacityBlockGroupId()
	data, err := marshal.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", domain.KindAllowance, id, err)
	}
	rec := store.Record{Kind: domain.KindAllowance, ID: id, ParentID: parent, Data: data}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		_, getErr := tx.Get(rec.Kind, id)
		switch {
		case create && getErr == nil:
			return errs.AlreadyExists(id, rec.Kind+" already exists")
		case !create && getErr != nil:
			return getErr
		}
		siblings, lerr := tx.List(store.ListQuery{Kind: rec.Kind, ParentID: parent})
		if lerr != nil {
			return lerr
		}
		for _, s := range siblings {
			if s.ID == id {
				continue
			}
			other, derr := decodeAllowance(s)
			if derr != nil {
				return derr
			}
			if other.GetSpec().GetCapacityBlockGroupId() == cbg {
				return errs.AlreadyExists(s.ID, "the project already has a capacity allowance for this capacity block group")
			}
		}
		_, putErr := tx.Put(rec)
		return putErr
	})
	return toDomain(err, id)
}

// Delete removes the allowance or returns errs.NotFound.
func (r *AllowanceRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindAllowance, id) })
	return toDomain(err, id)
}

// UsageRepo stores reservations by consumer.
type UsageRepo struct{ st store.Store }

// NewUsageRepo returns the usage repository over st.
func NewUsageRepo(st store.Store) *UsageRepo { return &UsageRepo{st: st} }

// Put records u, replacing the consumer's previous reservation.
func (r *UsageRepo) Put(ctx context.Context, u domain.Usage) error {
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", kindUsage, u.ConsumerID, err)
	}
	rec := store.Record{Kind: kindUsage, ID: u.ConsumerID, ParentID: u.BlockGroupID, Data: data}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		_, perr := tx.Put(rec)
		return perr
	})
	if err != nil {
		return fmt.Errorf("put %s %s: %w", kindUsage, u.ConsumerID, err)
	}
	return nil
}

// Delete removes the consumer's reservation; a missing one is not an error.
func (r *UsageRepo) Delete(ctx context.Context, consumerID string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(kindUsage, consumerID) })
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("delete %s %s: %w", kindUsage, consumerID, err)
	}
	return nil
}

// ByBlockGroup returns the reservations in a block group in creation order.
func (r *UsageRepo) ByBlockGroup(ctx context.Context, blockGroupID string) ([]domain.Usage, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: kindUsage, ParentID: blockGroupID})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list %s in %s: %w", kindUsage, blockGroupID, err)
	}
	out := make([]domain.Usage, 0, len(recs))
	for _, rec := range recs {
		var u domain.Usage
		if jerr := json.Unmarshal(rec.Data, &u); jerr != nil {
			return nil, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, jerr)
		}
		out = append(out, u)
	}
	return out, nil
}
