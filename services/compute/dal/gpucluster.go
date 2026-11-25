package dal

import (
	"context"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// GpuClusterRepo stores GPU clusters.
type GpuClusterRepo struct {
	r protoRepo[*computepb.GpuCluster]
}

var _ domain.GpuClusterRepository = (*GpuClusterRepo)(nil)

// NewGpuClusterRepo returns a GpuClusterRepo over st.
func NewGpuClusterRepo(st store.Store) *GpuClusterRepo {
	return &GpuClusterRepo{r: protoRepo[*computepb.GpuCluster]{
		st: st, kind: domain.KindGpuCluster, noun: "gpu cluster",
		newT: func() *computepb.GpuCluster { return &computepb.GpuCluster{} },
	}}
}

// Get returns the cluster or errs.NotFound.
func (g *GpuClusterRepo) Get(ctx context.Context, id string) (*computepb.GpuCluster, error) {
	return g.r.get(ctx, id)
}

// GetByName returns the cluster named name in parentID or errs.NotFound.
func (g *GpuClusterRepo) GetByName(ctx context.Context, parentID, name string) (*computepb.GpuCluster, error) {
	return g.r.getByName(ctx, parentID, name)
}

// List returns one page of a parent's clusters in creation order.
func (g *GpuClusterRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.GpuClusterPage, error) {
	items, next, err := g.r.list(ctx, parentID, pageSize, pageToken, nil)
	return domain.GpuClusterPage{Items: items, NextPageToken: next}, err
}

// Create inserts a new cluster; the name must be unique in its parent.
func (g *GpuClusterRepo) Create(ctx context.Context, c *computepb.GpuCluster) error {
	return g.r.create(ctx, c)
}

// Mutate implements domain.GpuClusterRepository.
func (g *GpuClusterRepo) Mutate(ctx context.Context, id string, fn func(*computepb.GpuCluster) error) (*computepb.GpuCluster, error) {
	return g.r.mutate(ctx, id, fn)
}

// DeleteIf implements domain.GpuClusterRepository.
func (g *GpuClusterRepo) DeleteIf(ctx context.Context, id string, check func(*computepb.GpuCluster) error) error {
	err := g.r.st.Update(ctx, func(tx store.Tx) error {
		rec, err := tx.Get(g.r.kind, id)
		if err != nil {
			return err
		}
		c, err := g.r.decode(rec)
		if err != nil {
			return err
		}
		if err := check(c); err != nil {
			return err
		}
		return tx.Delete(g.r.kind, id)
	})
	return g.r.toDomain(err, id)
}
