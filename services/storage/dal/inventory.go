package dal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// kindInventoryState holds the scheduler state of an inventory under the
// inventory's own ID.
const kindInventoryState = "storageinventorystate"

// InventoryRepo stores bucket inventories (children of their source bucket).
type InventoryRepo struct{ st store.Store }

var _ domain.InventoryRepository = (*InventoryRepo)(nil)

// NewInventoryRepo returns an InventoryRepo over st.
func NewInventoryRepo(st store.Store) *InventoryRepo { return &InventoryRepo{st: st} }

// stateJSON is the stored form of domain.InventoryState.
type stateJSON struct {
	NextRun   time.Time `json:"next_run"`
	LastRun   time.Time `json:"last_run"`
	CreatedBy string    `json:"created_by"`
}

// Get returns the inventory or errs.NotFound.
func (r *InventoryRepo) Get(ctx context.Context, id string) (*storagepb.Inventory, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		rec, err = tx.Get(domain.KindInventory, id)
		return err
	})
	if err != nil {
		return nil, inventoryErr(err, id)
	}
	return decodeInventory(rec)
}

// GetByName returns the inventory named name of bucket bucketID.
func (r *InventoryRepo) GetByName(ctx context.Context, bucketID, name string) (*storagepb.Inventory, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		rec, err = tx.ByName(domain.KindInventory, bucketID, name)
		return err
	})
	if err != nil {
		return nil, inventoryErr(err, name)
	}
	return decodeInventory(rec)
}

// List returns one page of a bucket's inventories in creation order.
func (r *InventoryRepo) List(ctx context.Context, bucketID string, pageSize int, pageToken string) (domain.InventoryPage, error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.InventoryPage{}, err
	}
	recs, err := r.records(ctx, store.ListQuery{Kind: domain.KindInventory, ParentID: bucketID, AfterSeq: after, Limit: pageSize + 1})
	if err != nil {
		return domain.InventoryPage{}, err
	}
	var page domain.InventoryPage
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	if page.Items, err = decodeInventories(recs); err != nil {
		return domain.InventoryPage{}, err
	}
	return page, nil
}

// ListAll returns every inventory in creation order.
func (r *InventoryRepo) ListAll(ctx context.Context) ([]*storagepb.Inventory, error) {
	recs, err := r.records(ctx, store.ListQuery{Kind: domain.KindInventory, AnyParent: true})
	if err != nil {
		return nil, err
	}
	return decodeInventories(recs)
}

// Count returns how many inventories a bucket has.
func (r *InventoryRepo) Count(ctx context.Context, bucketID string) (int, error) {
	recs, err := r.records(ctx, store.ListQuery{Kind: domain.KindInventory, ParentID: bucketID})
	return len(recs), err
}

func (r *InventoryRepo) records(ctx context.Context, q store.ListQuery) ([]store.Record, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		recs, err = tx.List(q)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list inventories: %w", err)
	}
	return recs, nil
}

// Create inserts an inventory and its scheduler state atomically.
func (r *InventoryRepo) Create(ctx context.Context, inv *storagepb.Inventory, st domain.InventoryState) error {
	md := inv.GetMetadata()
	data, err := marshal.Marshal(inv)
	if err != nil {
		return fmt.Errorf("encode inventory %s: %w", md.GetId(), err)
	}
	sdata, err := encodeState(st)
	if err != nil {
		return err
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		// The per-bucket limit is checked in the same transaction as the
		// insert, so concurrent creates cannot exceed it.
		siblings, err := tx.List(store.ListQuery{Kind: domain.KindInventory, ParentID: md.GetParentId()})
		if err != nil {
			return err
		}
		if len(siblings) >= domain.MaxInventoriesPerBucket {
			return errInventoryLimit(md.GetParentId())
		}
		if _, err := tx.Put(store.Record{Kind: domain.KindInventory, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data}); err != nil {
			return err
		}
		_, err = tx.Put(store.Record{Kind: kindInventoryState, ID: md.GetId(), ParentID: md.GetParentId(), Data: sdata})
		return err
	})
	return inventoryErr(err, md.GetId())
}

// errInventoryLimit reports a bucket that already has the maximum number of
// inventory configurations.
func errInventoryLimit(bucketID string) error {
	return &errs.Error{Kind: errs.KindQuotaExceeded, ResourceID: bucketID, Message: fmt.Sprintf(
		"a bucket can have at most %d inventory configurations", domain.MaxInventoriesPerBucket)}
}

// Update replaces an existing inventory (names never change).
func (r *InventoryRepo) Update(ctx context.Context, inv *storagepb.Inventory) error {
	md := inv.GetMetadata()
	data, err := marshal.Marshal(inv)
	if err != nil {
		return fmt.Errorf("encode inventory %s: %w", md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.Get(domain.KindInventory, md.GetId()); err != nil {
			return err
		}
		_, err := tx.Put(store.Record{Kind: domain.KindInventory, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data})
		return err
	})
	return inventoryErr(err, md.GetId())
}

// State returns the scheduler state of an inventory.
func (r *InventoryRepo) State(ctx context.Context, id string) (domain.InventoryState, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		rec, err = tx.Get(kindInventoryState, id)
		return err
	})
	if err != nil {
		return domain.InventoryState{}, inventoryErr(err, id)
	}
	var js stateJSON
	if err := json.Unmarshal(rec.Data, &js); err != nil {
		return domain.InventoryState{}, fmt.Errorf("decode inventory state %s: %w", id, err)
	}
	return domain.InventoryState(js), nil
}

// SetState replaces the scheduler state of an existing inventory.
func (r *InventoryRepo) SetState(ctx context.Context, id string, st domain.InventoryState) error {
	sdata, err := encodeState(st)
	if err != nil {
		return err
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		inv, err := tx.Get(domain.KindInventory, id)
		if err != nil {
			return err
		}
		_, err = tx.Put(store.Record{Kind: kindInventoryState, ID: id, ParentID: inv.ParentID, Data: sdata})
		return err
	})
	return inventoryErr(err, id)
}

// Delete removes the inventory and its state.
func (r *InventoryRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		if err := tx.Delete(domain.KindInventory, id); err != nil {
			return err
		}
		if err := tx.Delete(kindInventoryState, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	})
	return inventoryErr(err, id)
}

func encodeState(st domain.InventoryState) ([]byte, error) {
	data, err := json.Marshal(stateJSON(st))
	if err != nil {
		return nil, fmt.Errorf("encode inventory state: %w", err)
	}
	return data, nil
}

func inventoryErr(err error, id string) error {
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
		return errs.AlreadyExists(existing, "an inventory with this name already exists in the bucket")
	default:
		return err
	}
}

func decodeInventory(rec store.Record) (*storagepb.Inventory, error) {
	inv := &storagepb.Inventory{}
	if err := proto.Unmarshal(rec.Data, inv); err != nil {
		return nil, fmt.Errorf("decode inventory %s: %w", rec.ID, err)
	}
	return inv, nil
}

func decodeInventories(recs []store.Record) ([]*storagepb.Inventory, error) {
	out := make([]*storagepb.Inventory, 0, len(recs))
	for _, rec := range recs {
		inv, err := decodeInventory(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}
