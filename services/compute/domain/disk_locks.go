package domain

import (
	"context"
	"errors"
	"fmt"
	"slices"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// DiskLocks implements DiskLocker over a DiskRepository.
type DiskLocks struct{ disks DiskRepository }

var _ DiskLocker = (*DiskLocks)(nil)

// NewDiskLocks returns a DiskLocks over disks.
func NewDiskLocks(disks DiskRepository) (*DiskLocks, error) {
	if disks == nil {
		return nil, errors.New("compute: disk locks need a disk repository")
	}
	return &DiskLocks{disks: disks}, nil
}

// LockForImage implements DiskLocker.
func (l *DiskLocks) LockForImage(ctx context.Context, diskID, parentID, imageID string) (*computepb.Disk, error) {
	if diskID == "" || imageID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.source_disk_id", Message: "required"})
	}
	return l.disks.Mutate(ctx, diskID, func(d *computepb.Disk) error {
		if d.GetMetadata().GetParentId() != parentID {
			return errs.BadRequest(errs.Violation{
				Field: "spec.source_disk_id", Message: fmt.Sprintf("disk %s is not in project %s", diskID, parentID),
			})
		}
		if st := d.GetStatus().GetState(); st != computepb.DiskStatus_READY {
			return errs.BadState(diskID, fmt.Sprintf("disk %s is %s, not READY", diskID, st))
		}
		if d.GetStatus().GetLockState() == nil {
			d.Status.LockState = &computepb.DiskStatus_LockState{}
		}
		if !slices.Contains(d.GetStatus().GetLockState().GetImages(), imageID) {
			d.Status.LockState.Images = append(d.Status.LockState.Images, imageID)
		}
		return nil
	})
}

// UnlockForImage implements DiskLocker.
func (l *DiskLocks) UnlockForImage(ctx context.Context, diskID, imageID string) error {
	_, err := l.disks.Mutate(ctx, diskID, func(d *computepb.Disk) error {
		if ls := d.GetStatus().GetLockState(); ls != nil {
			ls.Images = slices.DeleteFunc(ls.GetImages(), func(x string) bool { return x == imageID })
		}
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}
