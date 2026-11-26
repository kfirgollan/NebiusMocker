package dal

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// NewQuotaUsage returns the Managed PostgreSQL quota usage source: what every
// stored cluster consumes (domain.QuotaAmounts), per project, whatever its
// state. A cluster counts until its record is removed when its deletion
// finishes.
func NewQuotaUsage(st store.Store) peer.QuotaUsageSource {
	clusters := NewClusterRepo(st)
	return peer.QuotaFunc(func(ctx context.Context) ([]peer.QuotaUsage, error) {
		all, err := clusters.All(ctx)
		if err != nil {
			return nil, fmt.Errorf("count postgresql clusters: %w", err)
		}
		type key struct{ project, name string }
		acc := map[key]uint64{}
		for _, c := range all {
			for name, n := range domain.QuotaAmounts(c.GetSpec()) {
				acc[key{c.GetMetadata().GetParentId(), name}] += uint64(max(n, 0))
			}
		}
		out := make([]peer.QuotaUsage, 0, len(acc))
		for k, n := range acc {
			out = append(out, peer.QuotaUsage{ContainerID: k.project, Name: k.name, Amount: n})
		}
		return out, nil
	})
}
