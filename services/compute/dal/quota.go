package dal

import (
	"context"
	"fmt"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// QuotaRecords reads every stored compute resource of every project.
type QuotaRecords struct {
	instances   protoRepo[*computepb.Instance]
	disks       protoRepo[*computepb.Disk]
	snapshots   protoRepo[*computepb.DiskSnapshot]
	filesystems protoRepo[*computepb.Filesystem]
	clusters    protoRepo[*computepb.GpuCluster]
}

var _ domain.QuotaRecords = (*QuotaRecords)(nil)

// NewQuotaRecords returns QuotaRecords over st.
func NewQuotaRecords(st store.Store) *QuotaRecords {
	return &QuotaRecords{
		instances:   NewInstanceRepo(st).r,
		disks:       NewDiskRepo(st).r,
		snapshots:   NewSnapshotRepo(st).r,
		filesystems: NewFilesystemRepo(st).r,
		clusters:    NewGpuClusterRepo(st).r,
	}
}

// Instances implements domain.QuotaRecords.
func (q *QuotaRecords) Instances(ctx context.Context) ([]*computepb.Instance, error) {
	return q.instances.all(ctx)
}

// Disks implements domain.QuotaRecords.
func (q *QuotaRecords) Disks(ctx context.Context) ([]*computepb.Disk, error) { return q.disks.all(ctx) }

// Snapshots implements domain.QuotaRecords.
func (q *QuotaRecords) Snapshots(ctx context.Context) ([]*computepb.DiskSnapshot, error) {
	return q.snapshots.all(ctx)
}

// Filesystems implements domain.QuotaRecords.
func (q *QuotaRecords) Filesystems(ctx context.Context) ([]*computepb.Filesystem, error) {
	return q.filesystems.all(ctx)
}

// GpuClusters implements domain.QuotaRecords.
func (q *QuotaRecords) GpuClusters(ctx context.Context) ([]*computepb.GpuCluster, error) {
	return q.clusters.all(ctx)
}

// all returns every item of the kind in every parent.
func (r protoRepo[T]) all(ctx context.Context) ([]T, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: r.kind, AnyParent: true})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list all %ss: %w", r.noun, err)
	}
	out := make([]T, 0, len(recs))
	for _, rec := range recs {
		item, err := r.decode(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}
