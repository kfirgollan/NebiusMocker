package dal

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

// ExportRepo stores audit event exports.
type ExportRepo struct{ st store.Store }

var _ domain.ExportRepository = (*ExportRepo)(nil)

// NewExportRepo returns an ExportRepo over st.
func NewExportRepo(st store.Store) *ExportRepo { return &ExportRepo{st: st} }

// envelope is the stored form of an export. AuditEventExport has no region
// field, so the region travels beside the protojson of the export.
type envelope struct {
	Region string          `json:"region"`
	Export json.RawMessage `json:"export"`
}

func encodeExport(e domain.Export) ([]byte, error) {
	inner, err := protojson.Marshal(e.Resource)
	if err != nil {
		return nil, fmt.Errorf("encode export %s: %w", e.Resource.GetMetadata().GetId(), err)
	}
	return json.Marshal(envelope{Region: e.Region, Export: inner})
}

func decodeExport(rec store.Record) (domain.Export, error) {
	var env envelope
	res := &auditv2pb.AuditEventExport{}
	if err := json.Unmarshal(rec.Data, &env); err != nil {
		return domain.Export{}, fmt.Errorf("decode export %s: %w", rec.ID, err)
	}
	if err := protojson.Unmarshal(env.Export, res); err != nil {
		return domain.Export{}, fmt.Errorf("decode export %s: %w", rec.ID, err)
	}
	return domain.Export{Region: env.Region, Resource: res}, nil
}

// Get returns the export or errs.NotFound.
func (r *ExportRepo) Get(ctx context.Context, id string) (domain.Export, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindExport, id)
		return gerr
	})
	if err != nil {
		return domain.Export{}, toDomain(err, id)
	}
	return decodeExport(rec)
}

type listedExport struct {
	seq uint64
	res *auditv2pb.AuditEventExport
}

// List returns one page of the exports of parentID started in region, in
// creation order.
func (r *ExportRepo) List(ctx context.Context, parentID, region string, pageSize int, pageToken string) (domain.ExportPage, error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.ExportPage{}, err
	}
	var items []listedExport
	err = r.st.View(ctx, func(tx store.Tx) error {
		items, err = scanRegion(tx, parentID, region, after, pageSize+1)
		return err
	})
	if err != nil {
		return domain.ExportPage{}, fmt.Errorf("list %s in %s: %w", domain.KindExport, parentID, err)
	}
	var page domain.ExportPage
	if len(items) > pageSize {
		items = items[:pageSize]
		page.NextPageToken = encodePageToken(items[pageSize-1].seq)
	}
	page.Items = make([]*auditv2pb.AuditEventExport, 0, len(items))
	for _, it := range items {
		page.Items = append(page.Items, it.res)
	}
	return page, nil
}

// scanRegion returns up to limit exports of parentID in region with Seq > after.
// The region is part of the stored envelope, so it filters after the read: the
// scan continues past exports of other regions until limit matches are found.
func scanRegion(tx store.Tx, parentID, region string, after uint64, limit int) ([]listedExport, error) {
	var items []listedExport
	for {
		recs, err := tx.List(store.ListQuery{Kind: domain.KindExport, ParentID: parentID, AfterSeq: after, Limit: limit})
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			after = rec.Seq
			e, err := decodeExport(rec)
			if err != nil {
				return nil, err
			}
			if e.Region == region {
				items = append(items, listedExport{rec.Seq, e.Resource})
			}
		}
		if len(items) >= limit || len(recs) < limit {
			return items, nil
		}
	}
}

// Create inserts a new export; a set name must be unique in its parent.
func (r *ExportRepo) Create(ctx context.Context, e domain.Export) error {
	md := e.Resource.GetMetadata()
	data, err := encodeExport(e)
	if err != nil {
		return err
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, gerr := tx.Get(domain.KindExport, md.GetId()); gerr == nil {
			return errs.AlreadyExists(md.GetId(), "export already exists")
		}
		_, perr := tx.Put(store.Record{
			Kind: domain.KindExport, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data,
		})
		return perr
	})
	return toDomain(err, md.GetId())
}

// Modify reads the export, applies fn and stores the result in one
// transaction. An error from fn discards the change and is returned as is.
// fn must not change the export's ID, parent or name.
func (r *ExportRepo) Modify(ctx context.Context, id string, fn func(*auditv2pb.AuditEventExport) error) (*auditv2pb.AuditEventExport, error) {
	var out *auditv2pb.AuditEventExport
	err := r.st.Update(ctx, func(tx store.Tx) error {
		rec, err := tx.Get(domain.KindExport, id)
		if err != nil {
			return err
		}
		e, err := decodeExport(rec)
		if err != nil {
			return err
		}
		if err := fn(e.Resource); err != nil {
			return err
		}
		if rec.Data, err = encodeExport(e); err != nil {
			return err
		}
		if _, err := tx.Put(rec); err != nil {
			return err
		}
		out = e.Resource
		return nil
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return out, nil
}
