package dal

import (
	"context"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// SnapshotRepo stores compute disk snapshots.
type SnapshotRepo struct {
	r protoRepo[*computepb.DiskSnapshot]
}

var _ domain.SnapshotRepository = (*SnapshotRepo)(nil)

// NewSnapshotRepo returns a SnapshotRepo over st.
func NewSnapshotRepo(st store.Store) *SnapshotRepo {
	return &SnapshotRepo{r: protoRepo[*computepb.DiskSnapshot]{
		st: st, kind: domain.KindDiskSnapshot, noun: "disk snapshot",
		newT: func() *computepb.DiskSnapshot { return &computepb.DiskSnapshot{} },
	}}
}

// Get returns the snapshot or errs.NotFound.
func (s *SnapshotRepo) Get(ctx context.Context, id string) (*computepb.DiskSnapshot, error) {
	return s.r.get(ctx, id)
}

// GetByName returns the snapshot named name in parentID or errs.NotFound.
func (s *SnapshotRepo) GetByName(ctx context.Context, parentID, name string) (*computepb.DiskSnapshot, error) {
	return s.r.getByName(ctx, parentID, name)
}

// List returns one page of a parent's snapshots in creation order.
func (s *SnapshotRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.SnapshotPage, error) {
	items, next, err := s.r.list(ctx, parentID, pageSize, pageToken, nil)
	return domain.SnapshotPage{Items: items, NextPageToken: next}, err
}

// ListByDisk returns one page of the snapshots of diskID in parentID.
func (s *SnapshotRepo) ListByDisk(ctx context.Context, parentID, diskID string, pageSize int, pageToken string) (domain.SnapshotPage, error) {
	items, next, err := s.r.list(ctx, parentID, pageSize, pageToken, func(snap *computepb.DiskSnapshot) bool {
		return snap.GetSpec().GetSourceDiskId() == diskID
	})
	return domain.SnapshotPage{Items: items, NextPageToken: next}, err
}

// Create inserts a new snapshot; the name must be unique in its parent.
func (s *SnapshotRepo) Create(ctx context.Context, snap *computepb.DiskSnapshot) error {
	return s.r.create(ctx, snap)
}

// Mutate implements domain.SnapshotRepository.
func (s *SnapshotRepo) Mutate(ctx context.Context, id string, fn func(*computepb.DiskSnapshot) error) (*computepb.DiskSnapshot, error) {
	return s.r.mutate(ctx, id, fn)
}

// Delete removes the snapshot or returns errs.NotFound.
func (s *SnapshotRepo) Delete(ctx context.Context, id string) error { return s.r.delete(ctx, id) }
