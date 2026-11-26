package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// NewQuotaUsage returns the MysteryBox quota usage source: the number of
// secrets per project. Every stored secret counts whatever its state, so a
// secret scheduled for deletion keeps counting until it is purged (Nebius
// counts a deleted secret for 30 days).
func NewQuotaUsage(st store.Store) peer.QuotaUsageSource {
	return peer.QuotaFunc(func(ctx context.Context) ([]peer.QuotaUsage, error) {
		counts := map[string]uint64{}
		err := st.View(ctx, func(tx store.Tx) error {
			recs, err := tx.List(store.ListQuery{Kind: domain.KindSecret, AnyParent: true})
			for _, r := range recs {
				counts[r.ParentID]++
			}
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("count secrets: %w", err)
		}
		out := make([]peer.QuotaUsage, 0, len(counts))
		for project, n := range counts {
			out = append(out, peer.QuotaUsage{ContainerID: project, Name: quota.MysteryBoxSecretCount, Amount: n})
		}
		return out, nil
	})
}
