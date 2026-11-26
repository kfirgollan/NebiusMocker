// Package dal implements the quota allowance repository over pkg/store. It is
// the only quotas package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/quotas/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// Repo stores allowances. A record's name is "<quota>@<region>", unique per
// container, so ByKey is a name lookup.
type Repo struct{ st store.Store }

var _ domain.Repository = (*Repo)(nil)

// NewRepo returns a Repo over st.
func NewRepo(st store.Store) *Repo { return &Repo{st: st} }

func recordName(name, region string) string { return name + "@" + region }

func decode(rec store.Record) (domain.Allowance, error) {
	m := &quotaspb.QuotaAllowance{}
	if err := proto.Unmarshal(rec.Data, m); err != nil {
		return domain.Allowance{}, fmt.Errorf("decode quota allowance %s: %w", rec.ID, err)
	}
	_, region, _ := strings.Cut(rec.Name, "@")
	return domain.Allowance{Msg: m, Region: region}, nil
}

func notFound(err error, id string) error {
	if errors.Is(err, store.ErrNotFound) {
		return errs.NotFound(id)
	}
	return err
}

// Get returns the allowance or errs.NotFound.
func (r *Repo) Get(ctx context.Context, id string) (domain.Allowance, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindAllowance, id)
		return gerr
	})
	if err != nil {
		return domain.Allowance{}, notFound(err, id)
	}
	return decode(rec)
}

// ByKey returns the allowance of a quota in a region of a container.
func (r *Repo) ByKey(ctx context.Context, parentID, name, region string) (domain.Allowance, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(domain.KindAllowance, parentID, recordName(name, region))
		return gerr
	})
	if err != nil {
		return domain.Allowance{}, notFound(err, name)
	}
	return decode(rec)
}

// List returns one page of a container's allowances in creation order.
func (r *Repo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.Page, error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.Page{}, err
	}
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: domain.KindAllowance, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
		return lerr
	})
	if err != nil {
		return domain.Page{}, fmt.Errorf("list quota allowances of %s: %w", parentID, err)
	}
	var page domain.Page
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	for _, rec := range recs {
		a, derr := decode(rec)
		if derr != nil {
			return domain.Page{}, derr
		}
		page.Items = append(page.Items, a)
	}
	return page, nil
}

// Ensure inserts the allowances whose ID is not stored yet, in order.
func (r *Repo) Ensure(ctx context.Context, as []domain.Allowance) error {
	return r.st.Update(ctx, func(tx store.Tx) error {
		for _, a := range as {
			if _, err := tx.Get(domain.KindAllowance, a.Msg.GetMetadata().GetId()); err == nil {
				continue
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := put(tx, a); err != nil {
				return err
			}
		}
		return nil
	})
}

// Put replaces an existing allowance.
func (r *Repo) Put(ctx context.Context, a domain.Allowance) error {
	id := a.Msg.GetMetadata().GetId()
	err := r.st.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.Get(domain.KindAllowance, id); err != nil {
			return err
		}
		return put(tx, a)
	})
	return notFound(err, id)
}

func put(tx store.Tx, a domain.Allowance) error {
	md := a.Msg.GetMetadata()
	data, err := marshal.Marshal(a.Msg)
	if err != nil {
		return fmt.Errorf("encode quota allowance %s: %w", md.GetId(), err)
	}
	_, err = tx.Put(store.Record{
		Kind: domain.KindAllowance, ID: md.GetId(), ParentID: md.GetParentId(),
		Name: recordName(md.GetName(), a.Region), Data: data,
	})
	return err
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
