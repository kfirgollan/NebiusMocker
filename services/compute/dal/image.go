// Package dal implements the Compute domain repositories over pkg/store.
// It is the only Compute package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// kindImageOp is the store kind of operation-index entries (not a NID type).
const kindImageOp = "computeimage.op"

var marshal = proto.MarshalOptions{Deterministic: true}

// ImageRepo stores compute images.
type ImageRepo struct{ st store.Store }

var _ domain.ImageRepository = (*ImageRepo)(nil)

// NewImageRepo returns an ImageRepo over st.
func NewImageRepo(st store.Store) *ImageRepo { return &ImageRepo{st: st} }

// Get returns the image or errs.NotFound.
func (r *ImageRepo) Get(ctx context.Context, id string) (*computepb.Image, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindImage, id)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, id)
	}
	return decode(rec)
}

// GetByName returns the image named name in parentID or errs.NotFound.
func (r *ImageRepo) GetByName(ctx context.Context, parentID, name string) (*computepb.Image, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(domain.KindImage, parentID, name)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, name)
	}
	return decode(rec)
}

// List returns one page of a parent's images in creation order.
func (r *ImageRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.ImagePage, error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.ImagePage{}, err
	}
	recs, err := r.list(ctx, store.ListQuery{Kind: domain.KindImage, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
	if err != nil {
		return domain.ImagePage{}, err
	}
	var page domain.ImagePage
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	page.Items, err = decodeAll(recs)
	return page, err
}

// All returns every image of parentID in creation order.
func (r *ImageRepo) All(ctx context.Context, parentID string) ([]*computepb.Image, error) {
	recs, err := r.list(ctx, store.ListQuery{Kind: domain.KindImage, ParentID: parentID})
	if err != nil {
		return nil, err
	}
	return decodeAll(recs)
}

func (r *ImageRepo) list(ctx context.Context, q store.ListQuery) ([]store.Record, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(q)
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list images in %s: %w", q.ParentID, err)
	}
	return recs, nil
}

// Create inserts a new image; the name must be unique in its parent.
func (r *ImageRepo) Create(ctx context.Context, img *computepb.Image) error {
	return r.write(ctx, img, true)
}

// Update replaces an existing image.
func (r *ImageRepo) Update(ctx context.Context, img *computepb.Image) error {
	return r.write(ctx, img, false)
}

// Delete removes the image or returns errs.NotFound.
func (r *ImageRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindImage, id) })
	return toDomain(err, id)
}

func (r *ImageRepo) write(ctx context.Context, img *computepb.Image, create bool) error {
	md := img.GetMetadata()
	data, err := marshal.Marshal(img)
	if err != nil {
		return fmt.Errorf("encode image %s: %w", md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		_, getErr := tx.Get(domain.KindImage, md.GetId())
		switch {
		case create && getErr == nil:
			return errs.AlreadyExists(md.GetId(), "image already exists")
		case !create && getErr != nil:
			return getErr
		}
		_, putErr := tx.Put(store.Record{Kind: domain.KindImage, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data})
		return putErr
	})
	return toDomain(err, md.GetId())
}

// OpIndex records which operations ran on images of each parent.
type OpIndex struct{ st store.Store }

var _ domain.OperationIndex = (*OpIndex)(nil)

// NewOpIndex returns an OpIndex over st.
func NewOpIndex(st store.Store) *OpIndex { return &OpIndex{st: st} }

// Add implements domain.OperationIndex.
func (o *OpIndex) Add(ctx context.Context, parentID, operationID string) error {
	return o.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindImageOp, ID: operationID, ParentID: parentID})
		return err
	})
}

// IDs implements domain.OperationIndex.
func (o *OpIndex) IDs(ctx context.Context, parentID string) ([]string, error) {
	var recs []store.Record
	err := o.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: kindImageOp, ParentID: parentID})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list operations of %s: %w", parentID, err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids, nil
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
		return errs.AlreadyExists(existing, "an image with this name already exists in the parent")
	default:
		return err
	}
}

func decode(rec store.Record) (*computepb.Image, error) {
	img := &computepb.Image{}
	if err := proto.Unmarshal(rec.Data, img); err != nil {
		return nil, fmt.Errorf("decode image %s: %w", rec.ID, err)
	}
	return img, nil
}

func decodeAll(recs []store.Record) ([]*computepb.Image, error) {
	out := make([]*computepb.Image, 0, len(recs))
	for _, rec := range recs {
		img, err := decode(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
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
