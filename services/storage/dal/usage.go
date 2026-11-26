package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// NewQuotaUsage returns the Object Storage quota usage source: buckets (of
// every type, including soft-deleted ones until they are purged) and data
// transfers (both API versions share one record kind) per project.
func NewQuotaUsage(st store.Store) peer.QuotaUsageSource {
	kinds := []struct{ kind, quota string }{
		{domain.KindBucket, quota.StorageBucketCount},
		{domain.KindTransfer, quota.StorageTransferCount},
	}
	return peer.QuotaFunc(func(ctx context.Context) ([]peer.QuotaUsage, error) {
		var out []peer.QuotaUsage
		for _, k := range kinds {
			counts := map[string]uint64{}
			err := st.View(ctx, func(tx store.Tx) error {
				recs, err := tx.List(store.ListQuery{Kind: k.kind, AnyParent: true})
				for _, r := range recs {
					counts[r.ParentID]++
				}
				return err
			})
			if err != nil {
				return nil, fmt.Errorf("count %s: %w", k.kind, err)
			}
			for project, n := range counts {
				out = append(out, peer.QuotaUsage{ContainerID: project, Name: k.quota, Amount: n})
			}
		}
		return out, nil
	})
}
