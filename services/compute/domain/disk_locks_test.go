package domain

import (
	"slices"
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func TestDiskLocksForImage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newDiskFixture(t)
	f.seedDisk(t, "computedisk-ready", nil)
	f.seedDisk(t, "computedisk-busy", func(d *computepb.Disk) { d.Status.State = computepb.DiskStatus_UPDATING })
	locks, err := NewDiskLocks(f.disks)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, disk, parent string
		want               errs.Kind
	}{
		{"missing disk", "computedisk-missing", testProject, errs.KindNotFound},
		{"other project", "computedisk-ready", otherProject, errs.KindBadRequest},
		{"not ready", "computedisk-busy", testProject, errs.KindBadState},
	} {
		if _, err := locks.LockForImage(ctx, tc.disk, tc.parent, "computeimage-a"); !errs.IsKind(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}

	for range 2 { // idempotent
		if _, err := locks.LockForImage(ctx, "computedisk-ready", testProject, "computeimage-a"); err != nil {
			t.Fatal(err)
		}
	}
	d, err := locks.LockForImage(ctx, "computedisk-ready", testProject, "computeimage-b")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.GetStatus().GetLockState().GetImages(); !slices.Equal(got, []string{"computeimage-a", "computeimage-b"}) {
		t.Fatalf("lock_state.images = %v", got)
	}
	if err := CheckDiskDeletable(d); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("locked disk deletable: %v", err)
	}

	for _, img := range []string{"computeimage-a", "computeimage-b", "computeimage-b"} {
		if err := locks.UnlockForImage(ctx, "computedisk-ready", img); err != nil {
			t.Fatal(err)
		}
	}
	if err := locks.UnlockForImage(ctx, "computedisk-missing", "computeimage-a"); err != nil {
		t.Fatalf("unlock of a missing disk: %v", err)
	}
	stored, err := f.disks.Get(ctx, "computedisk-ready")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDiskDeletable(stored); err != nil {
		t.Fatalf("unlocked disk not deletable: %v", err)
	}
}
