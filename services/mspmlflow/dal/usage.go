package dal

import (
	"context"
	"fmt"

	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

// NewQuotaUsage returns the Managed MLflow quota usage source: what every
// stored cluster consumes (amounts, the domain's QuotaAmounts), per project,
// whatever its state. A cluster counts until its record is removed when its
// deletion finishes.
func NewQuotaUsage(st store.Store, amounts func(*mlflowpb.Cluster) map[string]int64) peer.QuotaUsageSource {
	return peer.QuotaFunc(func(ctx context.Context) ([]peer.QuotaUsage, error) {
		var recs []store.Record
		err := st.View(ctx, func(tx store.Tx) error {
			var lerr error
			recs, lerr = tx.List(store.ListQuery{Kind: domain.KindCluster, AnyParent: true})
			return lerr
		})
		if err != nil {
			return nil, fmt.Errorf("count mlflow clusters: %w", err)
		}
		type key struct{ project, name string }
		acc := map[key]uint64{}
		for _, rec := range recs {
			c, err := decode(rec)
			if err != nil {
				return nil, err
			}
			for name, n := range amounts(c) {
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
