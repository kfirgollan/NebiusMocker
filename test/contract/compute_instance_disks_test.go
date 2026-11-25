package contract

import (
	"slices"
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

func existingDisk(id string, mode computepb.AttachedDiskSpec_AttachMode, device string) *computepb.AttachedDiskSpec {
	return &computepb.AttachedDiskSpec{
		AttachMode: mode, DeviceId: device,
		Type: &computepb.AttachedDiskSpec_ExistingDisk{ExistingDisk: &computepb.ExistingDisk{Id: id}},
	}
}

func managedDisk(name string, size int64) *computepb.AttachedDiskSpec {
	return &computepb.AttachedDiskSpec{
		AttachMode: computepb.AttachedDiskSpec_READ_WRITE,
		Type:       &computepb.AttachedDiskSpec_ManagedDisk{ManagedDisk: &computepb.ManagedDisk{Name: name, Spec: ssdSpec(size)}},
	}
}

func (c instClients) diskByName(t *testing.T, name string) (*computepb.Disk, error) {
	t.Helper()
	return c.disks.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: c.project, Name: name})
}

// TestInstanceExistingDisks attaches pre-created disks.
func TestInstanceExistingDisks(t *testing.T) {
	t.Parallel()
	c := startInstances(t)
	ctx := t.Context()
	data := c.newDisk(t, "data", 10)
	dataID := data.GetMetadata().GetId()

	spec := c.cpuSpec("a")
	spec.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk(dataID, computepb.AttachedDiskSpec_READ_WRITE, "data0")}
	a := c.mustCreate(t, "a", spec)
	protoEqual(t, "spec echo", a.GetSpec(), spec)
	if got := c.disk(t, dataID).GetStatus(); got.GetReadWriteAttachment() != a.GetMetadata().GetId() || got.GetManagedBy() != "" {
		t.Errorf("disk status after attach = %v", got)
	}
	if da := a.GetStatus().GetDiskAttachments(); len(da) != 2 || da[1].GetId() != dataID || da[1].GetIsManaged() {
		t.Errorf("disk attachments = %v", da)
	}

	wantCode(t, "delete attached disk", c.deleteDiskErr(t, dataID), codes.FailedPrecondition, "")
	_, err := c.create(ctx, "b", withSecondary(c.cpuSpec("b"), existingDisk(dataID, computepb.AttachedDiskSpec_READ_WRITE, "")))
	wantCode(t, "second read-write attach", err, codes.FailedPrecondition, "")
	_, err = c.create(ctx, "b2", withSecondary(c.cpuSpec("b2"), existingDisk(dataID, computepb.AttachedDiskSpec_READ_ONLY, "")))
	wantCode(t, "read-only next to a writer", err, codes.FailedPrecondition, "")
	_, err = c.create(ctx, "b3", withSecondary(c.cpuSpec("b3"), existingDisk("computedisk-e00doesnotexist", computepb.AttachedDiskSpec_READ_WRITE, "")))
	wantCode(t, "missing disk", err, codes.NotFound, "")
	_, err = c.create(ctx, "b4", withSecondary(c.cpuSpec("b4"),
		existingDisk(dataID, computepb.AttachedDiskSpec_READ_ONLY, "x"), existingDisk(dataID, computepb.AttachedDiskSpec_READ_ONLY, "y")))
	wantCode(t, "same disk twice", err, codes.InvalidArgument, "")

	// Detach through update, then two readers share it.
	a = c.mustUpdate(t, a, func(s *computepb.InstanceSpec) { s.SecondaryDisks = nil })
	if got := c.disk(t, dataID).GetStatus(); got.GetReadWriteAttachment() != "" || len(got.GetReadOnlyAttachments()) != 0 {
		t.Errorf("disk status after detach = %v", got)
	}
	if len(a.GetStatus().GetDiskAttachments()) != 1 {
		t.Errorf("attachments after detach = %v", a.GetStatus().GetDiskAttachments())
	}
	r1 := c.mustCreate(t, "r1", withSecondary(c.cpuSpec("r1"), existingDisk(dataID, computepb.AttachedDiskSpec_READ_ONLY, "")))
	r2 := c.mustCreate(t, "r2", withSecondary(c.cpuSpec("r2"), existingDisk(dataID, computepb.AttachedDiskSpec_READ_ONLY, "")))
	ro := c.disk(t, dataID).GetStatus().GetReadOnlyAttachments()
	if len(ro) != 2 || !slices.Contains(ro, r1.GetMetadata().GetId()) || !slices.Contains(ro, r2.GetMetadata().GetId()) {
		t.Errorf("read-only attachments = %v", ro)
	}
	_, err = c.create(ctx, "w", withSecondary(c.cpuSpec("w"), existingDisk(dataID, computepb.AttachedDiskSpec_READ_WRITE, "")))
	wantCode(t, "writer next to readers", err, codes.FailedPrecondition, "")

	// Instance delete keeps existing disks.
	c.mustDelete(t, r1.GetMetadata().GetId())
	c.mustDelete(t, r2.GetMetadata().GetId())
	if got := c.disk(t, dataID).GetStatus(); len(got.GetReadOnlyAttachments()) != 0 {
		t.Errorf("attachments after instance delete = %v", got)
	}
}

func withSecondary(s *computepb.InstanceSpec, d ...*computepb.AttachedDiskSpec) *computepb.InstanceSpec {
	s.SecondaryDisks = d
	return s
}

func (c instClients) deleteDiskErr(t *testing.T, id string) error {
	t.Helper()
	op, err := c.disks.Delete(t.Context(), &computepb.DeleteDiskRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(t.Context())
	return err
}

// TestInstanceManagedDisks covers disks owned by an instance.
func TestInstanceManagedDisks(t *testing.T) {
	t.Parallel()
	c := startInstances(t)
	ctx := t.Context()
	inst := c.mustCreate(t, "m", c.cpuSpec("m"))
	id := inst.GetMetadata().GetId()

	inst = c.mustUpdate(t, inst, func(s *computepb.InstanceSpec) {
		s.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk("m-data", 10)}
	})
	d, err := c.diskByName(t, "m-data")
	if err != nil {
		t.Fatalf("managed disk was not created: %v", err)
	}
	if d.GetStatus().GetManagedBy() != id || d.GetStatus().GetReadWriteAttachment() != id || d.GetStatus().GetState() != computepb.DiskStatus_READY {
		t.Errorf("managed disk status = %v", d.GetStatus())
	}
	if da := inst.GetStatus().GetDiskAttachments(); len(da) != 2 || !da[1].GetIsManaged() || da[1].GetName() != "m-data" {
		t.Errorf("attachments = %v", da)
	}
	protoEqual(t, "spec echo", inst.GetSpec().GetSecondaryDisks()[0], managedDisk("m-data", 10))

	next := d.GetSpec()
	next.Size = &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 20}
	_, err = c.disks.Update(ctx, &computepb.UpdateDiskRequest{Metadata: d.GetMetadata(), Spec: next})
	wantCode(t, "DiskService.Update on managed disk", err, codes.FailedPrecondition, "")
	wantCode(t, "DiskService.Delete on managed disk", c.deleteDiskErr(t, d.GetMetadata().GetId()), codes.FailedPrecondition, "")

	// Growing through the instance works.
	inst = c.mustUpdate(t, inst, func(s *computepb.InstanceSpec) { s.SecondaryDisks[0] = managedDisk("m-data", 20) })
	if got := c.disk(t, d.GetMetadata().GetId()).GetSpec().GetSizeGibibytes(); got != 20 {
		t.Errorf("managed disk size after grow = %d", got)
	}

	// Removal deletes it.
	inst = c.mustUpdate(t, inst, func(s *computepb.InstanceSpec) { s.SecondaryDisks = nil })
	if _, err := c.diskByName(t, "m-data"); err == nil {
		t.Errorf("managed disk survived removal from the spec")
	}

	// A managed disk may not take an existing disk's name.
	c.newDisk(t, "taken", 5)
	_, err = c.create(ctx, "m2", withSecondary(c.cpuSpec("m2"), managedDisk("taken", 5)))
	wantCode(t, "managed disk name conflict", err, codes.AlreadyExists, "")
	err = c.update(ctx, t, inst, func(s *computepb.InstanceSpec) {
		s.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk("taken", 5)}
	})
	wantCode(t, "managed disk name conflict on update", err, codes.AlreadyExists, "")

	// Instance delete removes managed disks and keeps existing ones.
	keep := c.newDisk(t, "keep", 5)
	spec := withSecondary(c.cpuSpec("m3"), managedDisk("m3-data", 5), existingDisk(keep.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_WRITE, ""))
	m3 := c.mustCreate(t, "m3", spec)
	c.mustDelete(t, m3.GetMetadata().GetId())
	for _, name := range []string{"m3-data", "m3-boot"} {
		if _, err := c.diskByName(t, name); err == nil {
			t.Errorf("managed disk %s survived instance delete", name)
		}
	}
	if got := c.disk(t, keep.GetMetadata().GetId()); got.GetStatus().GetReadWriteAttachment() != "" {
		t.Errorf("existing disk still attached after instance delete: %v", got.GetStatus())
	}
}
