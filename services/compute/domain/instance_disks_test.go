package domain

import (
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// withSecondary is baseSpec plus the given secondary disks.
func withSecondary(disks ...*computepb.AttachedDiskSpec) *computepb.InstanceSpec {
	s := baseSpec()
	s.SecondaryDisks = disks
	return s
}

func attachmentNames(inst *computepb.Instance) []string {
	var out []string
	for _, a := range inst.GetStatus().GetDiskAttachments() {
		out = append(out, a.GetName())
	}
	return out
}

func TestInstanceUpdateManagedDiskGrow(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	diskID := inst.GetStatus().GetDiskAttachments()[0].GetId()
	before := f.disk(t, diskID)
	err := f.update(t, id, func(s *computepb.InstanceSpec) { s.BootDisk.GetManagedDisk().Spec = ssdSpec(40) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := f.disk(t, diskID)
	if d.GetStatus().GetSizeBytes() != 40*gib || d.GetMetadata().GetResourceVersion() != before.GetMetadata().GetResourceVersion()+1 {
		t.Fatalf("disk size %d rv %d, want 40GiB and rv+1", d.GetStatus().GetSizeBytes(), d.GetMetadata().GetResourceVersion())
	}
	if diff := cmp.Diff(ssdSpec(40), d.GetSpec(), protocmp.Transform()); diff != "" {
		t.Fatalf("disk spec (-want +got):\n%s", diff)
	}
	if got := f.get(t, id).GetStatus().GetDiskAttachments()[0].GetId(); got != diskID {
		t.Fatalf("the managed disk was replaced by %s on a grow", got)
	}
}

func TestInstanceUpdateManagedDiskForbidDeletionAndLabels(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	diskID := inst.GetStatus().GetDiskAttachments()[0].GetId()
	err := f.update(t, id, func(s *computepb.InstanceSpec) {
		m := s.GetBootDisk().GetManagedDisk()
		m.Spec = proto.CloneOf(m.GetSpec())
		m.Spec.ForbidDeletion = true
		m.Labels = map[string]string{"role": "root"}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := f.disk(t, diskID)
	if !d.GetSpec().GetForbidDeletion() || d.GetMetadata().GetLabels()["role"] != "root" {
		t.Fatalf("disk = %v", d)
	}
	// forbid_deletion on a managed disk blocks the instance delete.
	wantKind(t, f.remove(id), errs.KindBadState, "")
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_RUNNING {
		t.Fatalf("state after a refused delete = %v, want RUNNING", instState(got))
	}
}

func TestInstanceUpdateManagedDiskRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		edit  func(*computepb.DiskSpec)
		field string
	}{
		{"shrink", func(d *computepb.DiskSpec) { d.Size = &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 10} }, "spec.boot_disk.managed_disk.spec.size"},
		{"type change", func(d *computepb.DiskSpec) { d.Type = computepb.DiskSpec_NETWORK_HDD }, "spec.boot_disk.managed_disk.spec.type"},
		{"block size change", func(d *computepb.DiskSpec) { d.BlockSizeBytes = 8192 }, "spec.boot_disk.managed_disk.spec.block_size_bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInstFixture(t)
			inst := f.mustCreate(t, "vm", baseSpec())
			id := inst.GetMetadata().GetId()
			diskID := inst.GetStatus().GetDiskAttachments()[0].GetId()
			before := f.disk(t, diskID)
			err := f.update(t, id, func(s *computepb.InstanceSpec) {
				m := s.GetBootDisk().GetManagedDisk()
				m.Spec = proto.CloneOf(m.GetSpec())
				tt.edit(m.GetSpec())
			}, nil)
			wantKind(t, err, errs.KindBadRequest, tt.field)
			if got := f.disk(t, diskID); !proto.Equal(got, before) {
				t.Errorf("disk changed by a refused update: %v", got)
			}
			if got := f.get(t, id); !proto.Equal(got, inst) {
				t.Errorf("instance changed by a refused update: %v", got)
			}
		})
	}
}

func TestInstanceUpdateManagedDiskRenameReplaces(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", withSecondary(managedDisk("data", 10)))
	id := inst.GetMetadata().GetId()
	oldID := inst.GetStatus().GetDiskAttachments()[1].GetId()
	err := f.update(t, id, func(s *computepb.InstanceSpec) { s.SecondaryDisks[0].GetManagedDisk().Name = "data2" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := f.get(t, id)
	if want := []string{"boot", "data2"}; !slices.Equal(attachmentNames(got), want) {
		t.Fatalf("attachments = %v, want %v", attachmentNames(got), want)
	}
	newID := got.GetStatus().GetDiskAttachments()[1].GetId()
	if newID == oldID {
		t.Fatalf("the disk keeps id %s after a rename: want a new disk", oldID)
	}
	if _, err := f.disks.Get(f.ctx, oldID); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("the old managed disk survived the rename: %v", err)
	}
	if d := f.disk(t, newID); d.GetStatus().GetManagedBy() != id || d.GetMetadata().GetName() != "data2" {
		t.Fatalf("new disk = %v", d)
	}
}

func TestInstanceUpdateBootDiskCannotBeReplaced(t *testing.T) {
	t.Parallel()
	tests := map[string]func(s *computepb.InstanceSpec, otherDiskID string){
		"rename managed boot": func(s *computepb.InstanceSpec, _ string) { s.BootDisk.GetManagedDisk().Name = "boot2" },
		"existing boot": func(s *computepb.InstanceSpec, id string) {
			s.BootDisk = existingDisk(id, computepb.AttachedDiskSpec_READ_WRITE)
		},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newInstFixture(t)
			other := f.mkDisk(t, "other", 20)
			inst := f.mustCreate(t, "vm", baseSpec())
			id := inst.GetMetadata().GetId()
			before := f.diskCount()
			err := f.update(t, id, func(s *computepb.InstanceSpec) { edit(s, other.GetMetadata().GetId()) }, nil)
			wantKind(t, err, errs.KindBadRequest, "spec.boot_disk")
			if f.diskCount() != before {
				t.Errorf("disk count %d -> %d after a refused boot disk change", before, f.diskCount())
			}
			if got := f.get(t, id); !proto.Equal(got, inst) {
				t.Errorf("instance changed: %v", got)
			}
		})
	}
}

func TestInstanceUpdateSecondaryDiskAddAndRemove(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	keep := f.mkDisk(t, "keep", 10)
	keepID := keep.GetMetadata().GetId()
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()

	err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk("data", 10), existingDisk(keepID, computepb.AttachedDiskSpec_READ_WRITE)}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := f.get(t, id)
	if want := []string{"boot", "data", "keep"}; !slices.Equal(attachmentNames(got), want) {
		t.Fatalf("attachments = %v, want %v", attachmentNames(got), want)
	}
	dataID := got.GetStatus().GetDiskAttachments()[1].GetId()
	if d := f.disk(t, dataID); d.GetStatus().GetManagedBy() != id || d.GetStatus().GetReadWriteAttachment() != id {
		t.Fatalf("new managed disk = %v", d.GetStatus())
	}
	if d := f.disk(t, keepID); d.GetStatus().GetReadWriteAttachment() != id || d.GetStatus().GetManagedBy() != "" {
		t.Fatalf("existing disk = %v", d.GetStatus())
	}

	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.SecondaryDisks = nil }, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"boot"}; !slices.Equal(attachmentNames(f.get(t, id)), want) {
		t.Fatalf("attachments = %v, want %v", attachmentNames(f.get(t, id)), want)
	}
	if _, err := f.disks.Get(f.ctx, dataID); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("removed managed disk was kept: %v", err)
	}
	d := f.disk(t, keepID)
	if d.GetStatus().GetReadWriteAttachment() != "" || len(d.GetStatus().GetReadOnlyAttachments()) != 0 {
		t.Fatalf("removed existing disk is still attached: %v", d.GetStatus())
	}
}

func TestInstanceUpdateSwitchExistingToManaged(t *testing.T) {
	t.Parallel()
	attach := func(t *testing.T, f *instFixture, forbid bool) (string, string) {
		t.Helper()
		spec := ssdSpec(10)
		spec.ForbidDeletion = forbid
		op, err := f.ds.Create(f.ctx, CreateDiskCmd{Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "d"}, Spec: spec})
		if err != nil {
			t.Fatal(err)
		}
		inst := f.mustCreate(t, "vm", withSecondary(existingDisk(op.ResourceID, computepb.AttachedDiskSpec_READ_WRITE)))
		return inst.GetMetadata().GetId(), op.ResourceID
	}
	toManaged := func(spec *computepb.DiskSpec, name string) func(*computepb.InstanceSpec) {
		return func(s *computepb.InstanceSpec) {
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk(name, 10)}
			s.SecondaryDisks[0].GetManagedDisk().Spec = spec
		}
	}
	t.Run("matching spec", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		id, diskID := attach(t, f, false)
		if err := f.update(t, id, toManaged(ssdSpec(10), "d"), nil); err != nil {
			t.Fatal(err)
		}
		d := f.disk(t, diskID)
		if d.GetStatus().GetManagedBy() != id || d.GetStatus().GetReadWriteAttachment() != id {
			t.Fatalf("disk status = %v, want managed by the instance", d.GetStatus())
		}
		if a := f.get(t, id).GetStatus().GetDiskAttachments()[1]; a.GetId() != diskID || !a.GetIsManaged() {
			t.Fatalf("attachment = %v", a)
		}
		// The disk is now deleted with the instance.
		if err := f.remove(id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.disks.Get(f.ctx, diskID); !errs.IsKind(err, errs.KindNotFound) {
			t.Fatalf("switched disk survived the instance delete: %v", err)
		}
	})
	t.Run("spec mismatch", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		id, diskID := attach(t, f, false)
		err := f.update(t, id, toManaged(ssdSpec(20), "d"), nil)
		wantKind(t, err, errs.KindBadRequest, "spec.secondary_disks[0].managed_disk.spec")
		if f.disk(t, diskID).GetStatus().GetManagedBy() != "" {
			t.Fatal("disk became managed by a refused update")
		}
	})
	t.Run("forbid_deletion", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		id, diskID := attach(t, f, true)
		spec := ssdSpec(10)
		spec.ForbidDeletion = true
		wantKind(t, f.update(t, id, toManaged(spec, "d"), nil), errs.KindBadState, "")
		if f.disk(t, diskID).GetStatus().GetManagedBy() != "" {
			t.Fatal("disk became managed by a refused update")
		}
	})
	t.Run("name differs from the disk", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		id, diskID := attach(t, f, false)
		// A managed disk with another name is a new disk; the existing one is detached and kept.
		if err := f.update(t, id, toManaged(ssdSpec(10), "other"), nil); err != nil {
			t.Fatal(err)
		}
		if d := f.disk(t, diskID); d.GetStatus().GetReadWriteAttachment() != "" || d.GetStatus().GetManagedBy() != "" {
			t.Fatalf("old disk = %v, want detached and unmanaged", d.GetStatus())
		}
	})
}

func TestInstanceUpdateSwitchManagedToExisting(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", withSecondary(managedDisk("data", 10)))
	id := inst.GetMetadata().GetId()
	dataID := inst.GetStatus().GetDiskAttachments()[1].GetId()
	err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk(dataID, computepb.AttachedDiskSpec_READ_WRITE)}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := f.disk(t, dataID); d.GetStatus().GetManagedBy() != "" || d.GetStatus().GetReadWriteAttachment() != id {
		t.Fatalf("disk status = %v, want unmanaged but attached", d.GetStatus())
	}
	if a := f.get(t, id).GetStatus().GetDiskAttachments()[1]; a.GetIsManaged() || a.GetId() != dataID {
		t.Fatalf("attachment = %v", a)
	}
	// An unmanaged disk survives the instance delete, detached.
	if err := f.remove(id); err != nil {
		t.Fatal(err)
	}
	d := f.disk(t, dataID)
	if d.GetStatus().GetReadWriteAttachment() != "" {
		t.Fatalf("disk still attached after the instance delete: %v", d.GetStatus())
	}
}

// ---- delete ----

func TestInstanceDeleteDisksAndNICs(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	keep := f.mkDisk(t, "keep", 10)
	spec := withSecondary(managedDisk("data", 10), existingDisk(keep.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_WRITE))
	spec.NetworkInterfaces = append(spec.NetworkInterfaces, nicSpec("eth1", "subnet-b"))
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{}
	inst := f.mustCreate(t, "vm", spec)
	id := inst.GetMetadata().GetId()
	var managed []string
	for _, a := range inst.GetStatus().GetDiskAttachments() {
		if a.GetIsManaged() {
			managed = append(managed, a.GetId())
		}
	}
	if len(managed) != 2 {
		t.Fatalf("managed disks = %v", managed)
	}
	if err := f.remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Get(f.ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("instance after delete: %v", err)
	}
	for _, d := range managed {
		if _, err := f.disks.Get(f.ctx, d); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("managed disk %s survived: %v", d, err)
		}
	}
	got := f.disk(t, keep.GetMetadata().GetId())
	if got.GetStatus().GetReadWriteAttachment() != "" || got.GetStatus().GetManagedBy() != "" || got.GetStatus().GetState() != computepb.DiskStatus_READY {
		t.Errorf("existing disk after delete = %v, want READY, detached and kept", got.GetStatus())
	}
	if b := f.net.boundNICs(); len(b) != 0 {
		t.Errorf("NICs still bound: %v", b)
	}
	for _, nic := range []string{"eth0", "eth1"} {
		if !slices.Contains(f.net.freed, id+"/"+nic) {
			t.Errorf("%s was not released: %v", nic, f.net.freed)
		}
	}
	// The kept disk can be attached again and deleted on its own.
	f.mustCreate(t, "vm2", edited(baseSpec(), func(s *computepb.InstanceSpec) {
		s.BootDisk = existingDisk(keep.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_WRITE)
	}))
}

func TestInstanceDeleteStates(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	running := f.mustCreate(t, "running", baseSpec())
	stopped := f.mustCreate(t, "stopped", edited(baseSpec(), func(s *computepb.InstanceSpec) {
		s.BootDisk = managedDisk("boot-s", 20)
		s.NetworkInterfaces[0].Name = "n2"
		s.Stopped = true
	}))
	for _, inst := range []*computepb.Instance{running, stopped} {
		if err := f.remove(inst.GetMetadata().GetId()); err != nil {
			t.Fatalf("delete %v instance: %v", instState(inst), err)
		}
	}
	wantKind(t, f.remove(running.GetMetadata().GetId()), errs.KindNotFound, "")
	_, err := f.svc.Delete(f.ctx, "", nil)
	wantKind(t, err, errs.KindBadRequest, "id")
}

func TestInstanceDeleteTransitionalIsRefused(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id := f.mustCreate(t, "vm", baseSpec()).GetMetadata().GetId()
	f.ops.deferred = true
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	wantKind(t, f.remove(id), errs.KindBadState, "")
	f.ops.finish()
	if err := f.remove(id); err != nil {
		t.Fatalf("delete once STOPPED: %v", err)
	}
}

func TestInstanceDeleteInProgress(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	f.ops.deferred = true
	if err := f.remove(id); err != nil {
		t.Fatal(err)
	}
	got := f.get(t, id)
	if instState(got) != computepb.InstanceStatus_DELETING {
		t.Fatalf("state = %v, want DELETING", instState(got))
	}
	wantKind(t, f.remove(id), errs.KindBadState, "")
	wantKind(t, f.stop(id), errs.KindBadState, "")
	f.ops.finish()
	if _, err := f.repo.Get(f.ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("instance after settle: %v", err)
	}
}

// ---- DiskService rules for instance disks ----

func TestDiskServiceRulesForInstanceDisks(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	free := f.mkDisk(t, "free", 10)
	inst := f.mustCreate(t, "vm", withSecondary(existingDisk(free.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_WRITE)))
	managedID := inst.GetStatus().GetDiskAttachments()[0].GetId()

	// Update of a managed disk is refused: it goes through the instance.
	_, err := f.ds.Update(f.ctx, UpdateDiskCmd{Metadata: &commonpb.ResourceMetadata{Id: managedID, Name: "renamed"}})
	wantKind(t, err, errs.KindBadState, "")
	// Delete of a managed or attached disk is refused.
	_, err = f.ds.Delete(f.ctx, managedID, nil)
	wantKind(t, err, errs.KindBadState, "")
	_, err = f.ds.Delete(f.ctx, free.GetMetadata().GetId(), nil)
	wantKind(t, err, errs.KindBadState, "")
	if d := f.disk(t, managedID); d.GetStatus().GetState() != computepb.DiskStatus_READY {
		t.Fatalf("managed disk state = %v after refused calls", d.GetStatus().GetState())
	}
}
