package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// kindDiskOp is the store kind of disk operation-index entries (not a NID type).
const kindDiskOp = "computedisk.op"

// DiskOpIndex records which operations ran on disks of each parent, so
// ListOperationsByParent still finds operations of deleted disks.
type DiskOpIndex struct{ st store.Store }

var _ domain.OperationIndex = (*DiskOpIndex)(nil)

// NewDiskOpIndex returns a DiskOpIndex over st.
func NewDiskOpIndex(st store.Store) *DiskOpIndex { return &DiskOpIndex{st: st} }

// Add implements domain.OperationIndex.
func (o *DiskOpIndex) Add(ctx context.Context, parentID, operationID string) error {
	return o.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindDiskOp, ID: operationID, ParentID: parentID})
		return err
	})
}

// IDs implements domain.OperationIndex.
func (o *DiskOpIndex) IDs(ctx context.Context, parentID string) ([]string, error) {
	var recs []store.Record
	err := o.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: kindDiskOp, ParentID: parentID})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list disk operations of %s: %w", parentID, err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids, nil
}
