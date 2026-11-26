package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// NewQuotaUsage returns the mk8s quota usage source: the number of clusters
// per project, whatever their state. Node groups are not counted here: their
// nodes are compute instances, which compute counts.
func NewQuotaUsage(st store.Store) peer.QuotaUsageSource {
	return peer.QuotaFunc(func(ctx context.Context) ([]peer.QuotaUsage, error) {
		counts := map[string]uint64{}
		err := st.View(ctx, func(tx store.Tx) error {
			recs, err := tx.List(store.ListQuery{Kind: domain.KindCluster, AnyParent: true})
			for _, r := range recs {
				counts[r.ParentID]++
			}
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("count clusters: %w", err)
		}
		out := make([]peer.QuotaUsage, 0, len(counts))
		for project, n := range counts {
			out = append(out, peer.QuotaUsage{ContainerID: project, Name: quota.MK8sClusterCount, Amount: n})
		}
		return out, nil
	})
}
