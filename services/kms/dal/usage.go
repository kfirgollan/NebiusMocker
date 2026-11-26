package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/kms/domain"
)

// NewQuotaUsage returns the KMS quota usage source: the number of symmetric and
// asymmetric keys per project. Every stored key counts whatever its state, so a
// key scheduled for deletion keeps counting until it is purged (Nebius counts a
// deleted key for 30 days).
func NewQuotaUsage(st store.Store) peer.QuotaUsageSource {
	kinds := []struct{ kind, quota string }{
		{domain.KindSymmetricKey, quota.KMSSymmetricKeyCount},
		{domain.KindAsymmetricKey, quota.KMSAsymmetricKeyCount},
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
