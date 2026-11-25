package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// kindFilesystemOp is the store kind of filesystem operation-index entries (not a NID type).
const kindFilesystemOp = "computefilesystem.op"

// FilesystemOpIndex records which operations ran on filesystems of each
// parent, so ListOperationsByParent still finds operations of deleted ones.
type FilesystemOpIndex struct{ st store.Store }

var _ domain.OperationIndex = (*FilesystemOpIndex)(nil)

// NewFilesystemOpIndex returns a FilesystemOpIndex over st.
func NewFilesystemOpIndex(st store.Store) *FilesystemOpIndex { return &FilesystemOpIndex{st: st} }

// Add implements domain.OperationIndex.
func (o *FilesystemOpIndex) Add(ctx context.Context, parentID, operationID string) error {
	return o.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindFilesystemOp, ID: operationID, ParentID: parentID})
		return err
	})
}

// IDs implements domain.OperationIndex.
func (o *FilesystemOpIndex) IDs(ctx context.Context, parentID string) ([]string, error) {
	var recs []store.Record
	err := o.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: kindFilesystemOp, ParentID: parentID})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list filesystem operations of %s: %w", parentID, err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids, nil
}
