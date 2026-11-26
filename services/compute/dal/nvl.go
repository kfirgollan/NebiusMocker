package dal

import (
	"context"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// NVLInstanceGroupRepo stores NVL instance groups.
type NVLInstanceGroupRepo struct {
	r protoRepo[*computepb.NVLInstanceGroup]
}

var _ domain.NVLInstanceGroupRepository = (*NVLInstanceGroupRepo)(nil)

// NewNVLInstanceGroupRepo returns a NVLInstanceGroupRepo over st.
func NewNVLInstanceGroupRepo(st store.Store) *NVLInstanceGroupRepo {
	return &NVLInstanceGroupRepo{r: protoRepo[*computepb.NVLInstanceGroup]{
		st: st, kind: domain.KindNVLInstanceGroup, noun: "nvl instance group",
		newT: func() *computepb.NVLInstanceGroup { return &computepb.NVLInstanceGroup{} },
	}}
}

// Get returns the group or errs.NotFound.
func (g *NVLInstanceGroupRepo) Get(ctx context.Context, id string) (*computepb.NVLInstanceGroup, error) {
	return g.r.get(ctx, id)
}

// GetByName returns the group named name in parentID or errs.NotFound.
func (g *NVLInstanceGroupRepo) GetByName(ctx context.Context, parentID, name string) (*computepb.NVLInstanceGroup, error) {
	return g.r.getByName(ctx, parentID, name)
}

// List returns one page of a parent's groups in creation order.
func (g *NVLInstanceGroupRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.NVLInstanceGroupPage, error) {
	items, next, err := g.r.list(ctx, parentID, pageSize, pageToken, nil)
	return domain.NVLInstanceGroupPage{Items: items, NextPageToken: next}, err
}

// Create inserts a new group; the name must be unique in its parent.
func (g *NVLInstanceGroupRepo) Create(ctx context.Context, c *computepb.NVLInstanceGroup) error {
	return g.r.create(ctx, c)
}

// Mutate implements domain.NVLInstanceGroupRepository.
func (g *NVLInstanceGroupRepo) Mutate(ctx context.Context, id string, fn func(*computepb.NVLInstanceGroup) error) (*computepb.NVLInstanceGroup, error) {
	return g.r.mutate(ctx, id, fn)
}

// DeleteIf implements domain.NVLInstanceGroupRepository.
func (g *NVLInstanceGroupRepo) DeleteIf(ctx context.Context, id string, check func(*computepb.NVLInstanceGroup) error) error {
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
