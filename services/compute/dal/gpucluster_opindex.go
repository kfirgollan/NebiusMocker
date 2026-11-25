package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// kindGpuClusterOp is the store kind of GPU cluster operation-index entries
// (not a NID type).
const kindGpuClusterOp = "computegpucluster.op"

// GpuClusterOpIndex records which operations ran on GPU clusters of each
// parent, so ListOperationsByParent still finds operations of deleted clusters.
type GpuClusterOpIndex struct{ st store.Store }

var _ domain.OperationIndex = (*GpuClusterOpIndex)(nil)

// NewGpuClusterOpIndex returns a GpuClusterOpIndex over st.
func NewGpuClusterOpIndex(st store.Store) *GpuClusterOpIndex { return &GpuClusterOpIndex{st: st} }

// Add implements domain.OperationIndex.
func (o *GpuClusterOpIndex) Add(ctx context.Context, parentID, operationID string) error {
	return o.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindGpuClusterOp, ID: operationID, ParentID: parentID})
		return err
	})
}

// IDs implements domain.OperationIndex.
func (o *GpuClusterOpIndex) IDs(ctx context.Context, parentID string) ([]string, error) {
	var recs []store.Record
	err := o.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: kindGpuClusterOp, ParentID: parentID})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list gpu cluster operations of %s: %w", parentID, err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids, nil
}
