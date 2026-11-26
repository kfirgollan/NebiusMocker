// Package dal stores billing resources in pkg/store. It is the only billing
// package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// PricingPolicyRepo stores pricing policies as proto-marshaled records.
// Names are unique per parent.
type PricingPolicyRepo struct{ st store.Store }

var _ domain.PricingPolicyRepository = (*PricingPolicyRepo)(nil)

// NewPricingPolicyRepo returns the repository over st.
func NewPricingPolicyRepo(st store.Store) *PricingPolicyRepo { return &PricingPolicyRepo{st: st} }

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
		return errs.AlreadyExists(existing, "a pricing policy with this name already exists in the project")
	default:
		return err
	}
}

func decode(rec store.Record) (*billingpb.PricingPolicy, error) {
	p := &billingpb.PricingPolicy{}
	if err := proto.Unmarshal(rec.Data, p); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return p, nil
}

// Get returns the policy or errs.NotFound.
func (r *PricingPolicyRepo) Get(ctx context.Context, id string) (*billingpb.PricingPolicy, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindPricingPolicy, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decode(rec)
}

// GetByName returns the policy named name in parentID, or errs.NotFound.
func (r *PricingPolicyRepo) GetByName(ctx context.Context, parentID, name string) (*billingpb.PricingPolicy, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(domain.KindPricingPolicy, parentID, name)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, name)
	}
	return decode(rec)
}

// List returns one page of the parent's policies in creation order.
func (r *PricingPolicyRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.Page[*billingpb.PricingPolicy], error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.Page[*billingpb.PricingPolicy]{}, err
	}
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: domain.KindPricingPolicy, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
		return lerr
	})
	if err != nil {
		return domain.Page[*billingpb.PricingPolicy]{}, fmt.Errorf("list pricing policies in %s: %w", parentID, err)
	}
	var page domain.Page[*billingpb.PricingPolicy]
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	page.Items = make([]*billingpb.PricingPolicy, 0, len(recs))
	for _, rec := range recs {
		p, err := decode(rec)
		if err != nil {
			return domain.Page[*billingpb.PricingPolicy]{}, err
		}
		page.Items = append(page.Items, p)
	}
	return page, nil
}

// Create inserts a policy; errs.AlreadyExists if the ID exists or the name
// is taken in the parent.
func (r *PricingPolicyRepo) Create(ctx context.Context, p *billingpb.PricingPolicy) error {
	return r.write(ctx, p, true)
}

// Update replaces an existing policy; errs.NotFound if it does not exist.
func (r *PricingPolicyRepo) Update(ctx context.Context, p *billingpb.PricingPolicy) error {
	return r.write(ctx, p, false)
}

func (r *PricingPolicyRepo) write(ctx context.Context, p *billingpb.PricingPolicy, create bool) error {
	md := p.GetMetadata()
	data, err := marshal.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode pricing policy %s: %w", md.GetId(), err)
	}
	rec := store.Record{Kind: domain.KindPricingPolicy, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		_, getErr := tx.Get(rec.Kind, rec.ID)
		switch {
		case create && getErr == nil:
			return errs.AlreadyExists(rec.ID, "pricing policy already exists")
		case !create && getErr != nil:
			return getErr
		}
		_, putErr := tx.Put(rec)
		return putErr
	})
	return toDomain(err, rec.ID)
}

// Delete removes the policy or returns errs.NotFound.
func (r *PricingPolicyRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindPricingPolicy, id) })
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
