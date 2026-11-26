package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

// NewQuotaUsage returns the registry quota usage source: the number of
// registries per project, whatever their state.
func NewQuotaUsage(st store.Store) peer.QuotaUsageSource {
	return peer.QuotaFunc(func(ctx context.Context) ([]peer.QuotaUsage, error) {
		counts := map[string]uint64{}
		err := st.View(ctx, func(tx store.Tx) error {
			recs, err := tx.List(store.ListQuery{Kind: domain.KindRegistry, AnyParent: true})
			for _, r := range recs {
				counts[r.ParentID]++
			}
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("count registries: %w", err)
		}
		out := make([]peer.QuotaUsage, 0, len(counts))
		for project, n := range counts {
			out = append(out, peer.QuotaUsage{ContainerID: project, Name: quota.RegistryCount, Amount: n})
		}
		return out, nil
	})
}
