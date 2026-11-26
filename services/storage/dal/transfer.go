package dal

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// kindTransferIteration holds iteration history: ID = transferID/%020d seq,
// parent = the transfer.
const kindTransferIteration = "storagetransferiteration"

// TransferRepo stores transfers (children of their project; names unique per
// project across both API versions) and their iteration history.
type TransferRepo struct{ st store.Store }

var _ domain.TransferRepository = (*TransferRepo)(nil)

// NewTransferRepo returns a TransferRepo over st.
func NewTransferRepo(st store.Store) *TransferRepo { return &TransferRepo{st: st} }

// transferEnvelope is the stored form of a domain.TransferRecord: the
// resource as deterministic proto bytes plus the runner state.
type transferEnvelope struct {
	API         domain.TransferAPI    `json:"api"`
	Resource    []byte                `json:"resource"`
	Status      domain.TransferStatus `json:"status"`
	NextRun     time.Time             `json:"next_run"`
	EmptyStreak uint32                `json:"empty_streak"`
	Watermark   time.Time             `json:"watermark"`
}

func encodeTransfer(r *domain.TransferRecord) ([]byte, error) {
	var msg proto.Message
	switch r.API {
	case domain.TransferAPIV1:
		msg = r.V1
	case domain.TransferAPIV1Alpha1:
		msg = r.Alpha
	default:
		return nil, fmt.Errorf("transfer %s: unknown API %d", r.Metadata().GetId(), r.API)
	}
	res, err := marshal.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode transfer %s: %w", r.Metadata().GetId(), err)
	}
	data, err := json.Marshal(transferEnvelope{
		API: r.API, Resource: res, Status: r.Status, NextRun: r.NextRun, EmptyStreak: r.EmptyStreak, Watermark: r.Watermark,
	})
	if err != nil {
		return nil, fmt.Errorf("encode transfer %s: %w", r.Metadata().GetId(), err)
	}
	return data, nil
}

func decodeTransfer(rec store.Record) (*domain.TransferRecord, error) {
	var env transferEnvelope
	if err := json.Unmarshal(rec.Data, &env); err != nil {
		return nil, fmt.Errorf("decode transfer %s: %w", rec.ID, err)
	}
	out := &domain.TransferRecord{API: env.API, Status: env.Status, NextRun: env.NextRun, EmptyStreak: env.EmptyStreak, Watermark: env.Watermark}
	var msg proto.Message
	switch env.API {
	case domain.TransferAPIV1:
		out.V1 = &storagepb.Transfer{}
		msg = out.V1
	case domain.TransferAPIV1Alpha1:
		out.Alpha = &storagev1alpha1pb.Transfer{}
		msg = out.Alpha
	default:
		return nil, fmt.Errorf("decode transfer %s: unknown API %d", rec.ID, env.API)
	}
	if err := proto.Unmarshal(env.Resource, msg); err != nil {
		return nil, fmt.Errorf("decode transfer %s: %w", rec.ID, err)
	}
	return out, nil
}

func (r *TransferRepo) view(ctx context.Context, key string, fn func(tx store.Tx) (store.Record, error)) (*domain.TransferRecord, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		rec, err = fn(tx)
		return err
	})
	if err != nil {
		return nil, transferErr(err, key)
	}
	return decodeTransfer(rec)
}

// Get returns the transfer or errs.NotFound.
func (r *TransferRepo) Get(ctx context.Context, id string) (*domain.TransferRecord, error) {
	return r.view(ctx, id, func(tx store.Tx) (store.Record, error) { return tx.Get(domain.KindTransfer, id) })
}

// GetByName returns the transfer named name in project parentID.
func (r *TransferRepo) GetByName(ctx context.Context, parentID, name string) (*domain.TransferRecord, error) {
	return r.view(ctx, name, func(tx store.Tx) (store.Record, error) { return tx.ByName(domain.KindTransfer, parentID, name) })
}

// List returns one page of a project's transfers in creation order; a
// non-zero only keeps those created through that API version.
func (r *TransferRepo) List(ctx context.Context, parentID string, only domain.TransferAPI, pageSize int, pageToken string) (domain.TransferPage, error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.TransferPage{}, err
	}
	recs, err := r.records(ctx, store.ListQuery{Kind: domain.KindTransfer, ParentID: parentID, AfterSeq: after})
	if err != nil {
		return domain.TransferPage{}, err
	}
	var page domain.TransferPage
	var lastSeq uint64
	for _, rec := range recs {
		t, err := decodeTransfer(rec)
		if err != nil {
			return domain.TransferPage{}, err
		}
		if only != 0 && t.API != only {
			continue
		}
		if len(page.Items) == pageSize {
			page.NextPageToken = encodePageToken(lastSeq)
			break
		}
		page.Items = append(page.Items, t)
		lastSeq = rec.Seq
	}
	return page, nil
}

// ListAll returns every transfer in creation order.
func (r *TransferRepo) ListAll(ctx context.Context) ([]*domain.TransferRecord, error) {
	recs, err := r.records(ctx, store.ListQuery{Kind: domain.KindTransfer, AnyParent: true})
	if err != nil {
		return nil, err
	}
	out := make([]*domain.TransferRecord, 0, len(recs))
	for _, rec := range recs {
		t, err := decodeTransfer(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (r *TransferRepo) records(ctx context.Context, q store.ListQuery) ([]store.Record, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		recs, err = tx.List(q)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", q.Kind, err)
	}
	return recs, nil
}

func transferRecord(t *domain.TransferRecord) (store.Record, error) {
	data, err := encodeTransfer(t)
	if err != nil {
		return store.Record{}, err
	}
	md := t.Metadata()
	return store.Record{Kind: domain.KindTransfer, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data}, nil
}

// Create inserts a transfer; a taken name is errs.AlreadyExists.
func (r *TransferRepo) Create(ctx context.Context, t *domain.TransferRecord) error {
	rec, err := transferRecord(t)
	if err != nil {
		return err
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.Get(domain.KindTransfer, rec.ID); err == nil {
			return errs.AlreadyExists(rec.ID, "the transfer already exists")
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		_, err := tx.Put(rec)
		return err
	})
	return transferErr(err, rec.ID)
}

// Update replaces an existing transfer (a rename to a taken name is
// errs.AlreadyExists).
func (r *TransferRepo) Update(ctx context.Context, t *domain.TransferRecord) error {
	rec, err := transferRecord(t)
	if err != nil {
		return err
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.Get(domain.KindTransfer, rec.ID); err != nil {
			return err
		}
		_, err := tx.Put(rec)
		return err
	})
	return transferErr(err, rec.ID)
}

// Delete removes the transfer and its iteration history in one transaction.
func (r *TransferRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.Get(domain.KindTransfer, id); err != nil {
			return err
		}
		its, err := tx.List(store.ListQuery{Kind: kindTransferIteration, ParentID: id})
		if err != nil {
			return err
		}
		for _, it := range its {
			if err := tx.Delete(kindTransferIteration, it.ID); err != nil {
				return err
			}
		}
		return tx.Delete(domain.KindTransfer, id)
	})
	return transferErr(err, id)
}

func iterationID(transferID string, seq int64) string {
	return fmt.Sprintf("%s/%020d", transferID, seq)
}

// PutIteration inserts or replaces iteration it.Seq of a transfer.
func (r *TransferRepo) PutIteration(ctx context.Context, transferID string, it domain.Iteration) error {
	data, err := json.Marshal(it)
	if err != nil {
		return fmt.Errorf("encode iteration %d of %s: %w", it.Seq, transferID, err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.Get(domain.KindTransfer, transferID); err != nil {
			return err
		}
		_, err := tx.Put(store.Record{Kind: kindTransferIteration, ID: iterationID(transferID, it.Seq), ParentID: transferID, Data: data})
		return err
	})
	return transferErr(err, transferID)
}

// History returns one page of a transfer's iterations, newest first. The
// page token is the sequence number to continue below.
func (r *TransferRepo) History(ctx context.Context, transferID string, pageSize int, pageToken string) (domain.IterationPage, error) {
	before, err := decodeIterationToken(pageToken)
	if err != nil {
		return domain.IterationPage{}, err
	}
	recs, err := r.records(ctx, store.ListQuery{Kind: kindTransferIteration, ParentID: transferID})
	if err != nil {
		return domain.IterationPage{}, err
	}
	its := make([]domain.Iteration, 0, len(recs))
	for _, rec := range recs {
		var it domain.Iteration
		if err := json.Unmarshal(rec.Data, &it); err != nil {
			return domain.IterationPage{}, fmt.Errorf("decode iteration %s: %w", rec.ID, err)
		}
		if before == 0 || it.Seq < before {
			its = append(its, it)
		}
	}
	slices.SortFunc(its, func(a, b domain.Iteration) int { return cmp.Compare(b.Seq, a.Seq) })
	var page domain.IterationPage
	if len(its) > pageSize {
		its = its[:pageSize]
		page.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(its[len(its)-1].Seq, 10)))
	}
	page.Items = its
	return page, nil
}

func decodeIterationToken(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(token); err == nil {
		if seq, err := strconv.ParseInt(string(b), 10, 64); err == nil && seq > 0 {
			return seq, nil
		}
	}
	return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
}

func transferErr(err error, id string) error {
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
		return errs.AlreadyExists(existing, "a transfer with this name already exists in the project")
	default:
		return err
	}
}
