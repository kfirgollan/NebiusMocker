package contract

import (
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// fsMask extends instResetMask with the filesystems the tests clear.
const fsMask = instResetMask + ",spec.filesystems"

func attachedFS(id, tag string, mode computepb.AttachedFilesystemSpec_AttachMode) *computepb.AttachedFilesystemSpec {
	return &computepb.AttachedFilesystemSpec{
		AttachMode: mode, MountTag: tag,
		Type: &computepb.AttachedFilesystemSpec_ExistingFilesystem{ExistingFilesystem: &computepb.ExistingFilesystem{Id: id}},
	}
}

func rwFS(id, tag string) *computepb.AttachedFilesystemSpec {
	return attachedFS(id, tag, computepb.AttachedFilesystemSpec_READ_WRITE)
}

func withFS(s *computepb.InstanceSpec, f ...*computepb.AttachedFilesystemSpec) *computepb.InstanceSpec {
	s.Filesystems = f
	return s
}

// updateFS resends the instance with change applied (filesystems reset).
func (c instClients) updateFS(t *testing.T, i *computepb.Instance, change func(*computepb.InstanceSpec)) error {
	t.Helper()
	next := proto.Clone(i).(*computepb.Instance)
	change(next.GetSpec())
	op, err := c.instances.Update(resetCtx(t.Context(), t, fsMask), &computepb.UpdateInstanceRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(t.Context())
	return err
}

func fsAttachments(t *testing.T, f fsClients, id string) []string {
	t.Helper()
	got, err := f.fs.Get(t.Context(), &computepb.GetFilesystemRequest{Id: id})
	if err != nil {
		t.Fatalf("get filesystem %s: %v", id, err)
	}
	if ro := got.GetStatus().GetReadOnlyAttachments(); len(ro) != 0 {
		t.Errorf("filesystem %s read-only attachments = %v, want none", id, ro)
	}
	return got.GetStatus().GetReadWriteAttachments()
}

// TestInstanceFilesystems attaches shared filesystems to instances through
// create and update, and checks the filesystem side of the bookkeeping.
func TestInstanceFilesystems(t *testing.T) {
	t.Parallel()
	c := startInstances(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired(),
		testkit.WithProjects(config.ProjectSeed{Name: "other"}))
	admin := instClientsFor(t, c.env, config.DefaultDevToken)
	f := newFsClients(t, c.env.Inst, config.DefaultDevToken)
	ctx := t.Context()
	other := newFsClients(t, c.env.Inst, config.DefaultDevToken)
	other.project = c.env.Inst.Projects[1].ID

	a := f.must(ctx, t, "shared-a", fsSpecGiB(10)).GetMetadata().GetId()
	b := f.must(ctx, t, "shared-b", fsSpecGiB(10)).GetMetadata().GetId()
	foreign := other.must(ctx, t, "foreign", fsSpecGiB(10)).GetMetadata().GetId()
	missing := "computefilesystem-e00doesnotexist"

	// Create with a filesystem: echoed on the instance, recorded on the filesystem.
	spec := withFS(admin.cpuSpec("vm"), rwFS(a, "data"))
	inst := admin.mustCreate(t, "vm", spec)
	vmID := inst.GetMetadata().GetId()
	protoEqual(t, "spec.filesystems echo", &computepb.InstanceSpec{Filesystems: inst.GetSpec().GetFilesystems()}, &computepb.InstanceSpec{Filesystems: spec.GetFilesystems()})
	if got := fsAttachments(t, f, a); !slices.Equal(got, []string{vmID}) {
		t.Fatalf("read_write_attachments after create = %v, want [%s]", got, vmID)
	}

	// Invalid creates leave nothing attached.
	_, err := admin.create(ctx, "bad-mode", withFS(admin.cpuSpec("bad-mode"), attachedFS(b, "m", computepb.AttachedFilesystemSpec_READ_ONLY)))
	wantCode(t, "READ_ONLY attach_mode", err, codes.InvalidArgument, "attach_mode")
	_, err = admin.create(ctx, "bad-tag", withFS(admin.cpuSpec("bad-tag"), rwFS(a, "same"), rwFS(b, "same")))
	wantCode(t, "duplicate mount_tag", err, codes.InvalidArgument, "mount_tag")
	_, err = admin.create(ctx, "bad-proj", withFS(admin.cpuSpec("bad-proj"), rwFS(foreign, "x")))
	wantCode(t, "filesystem of another project", err, codes.InvalidArgument, "existing_filesystem.id")
	_, err = admin.create(ctx, "bad-missing", withFS(admin.cpuSpec("bad-missing"), rwFS(missing, "x")))
	wantCode(t, "missing filesystem", err, codes.NotFound, "existing_filesystem.id")
	if got := fsAttachments(t, f, b); len(got) != 0 {
		t.Errorf("filesystem %s attachments after rejected creates = %v, want none", b, got)
	}
	if got := fsAttachments(t, other, foreign); len(got) != 0 {
		t.Errorf("foreign filesystem attachments after rejected create = %v, want none", got)
	}

	// Adding a filesystem to a running instance is refused.
	err = admin.updateFS(t, inst, func(s *computepb.InstanceSpec) { s.Filesystems = append(s.Filesystems, rwFS(b, "extra")) })
	wantCode(t, "attach while RUNNING", err, codes.FailedPrecondition, "")
	if got := fsAttachments(t, f, b); len(got) != 0 {
		t.Errorf("filesystem %s attachments after refused update = %v, want none", b, got)
	}

	// Stopped: rejected shapes stay rejected, a valid one attaches.
	if err := admin.stop(ctx, vmID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	inst = admin.get(t, vmID)
	err = admin.updateFS(t, inst, func(s *computepb.InstanceSpec) { s.Filesystems = append(s.Filesystems, rwFS(b, "data")) })
	wantCode(t, "update with duplicate mount_tag", err, codes.InvalidArgument, "mount_tag")
	err = admin.updateFS(t, inst, func(s *computepb.InstanceSpec) {
		s.Filesystems = append(s.Filesystems, attachedFS(b, "extra", computepb.AttachedFilesystemSpec_READ_ONLY))
	})
	wantCode(t, "update with READ_ONLY", err, codes.InvalidArgument, "attach_mode")
	err = admin.updateFS(t, inst, func(s *computepb.InstanceSpec) { s.Filesystems = append(s.Filesystems, rwFS(foreign, "extra")) })
	wantCode(t, "update with filesystem of another project", err, codes.InvalidArgument, "existing_filesystem.id")
	err = admin.updateFS(t, inst, func(s *computepb.InstanceSpec) { s.Filesystems = append(s.Filesystems, rwFS(missing, "extra")) })
	wantCode(t, "update with missing filesystem", err, codes.NotFound, "existing_filesystem.id")
	if got := fsAttachments(t, f, b); len(got) != 0 {
		t.Errorf("filesystem %s attachments after refused updates = %v, want none", b, got)
	}

	err = admin.updateFS(t, inst, func(s *computepb.InstanceSpec) { s.Filesystems = append(s.Filesystems, rwFS(b, "extra")) })
	if err != nil {
		t.Fatalf("attach b while STOPPED: %v", err)
	}
	inst = admin.get(t, vmID)
	if len(inst.GetSpec().GetFilesystems()) != 2 || inst.GetStatus().GetState() != computepb.InstanceStatus_STOPPED {
		t.Errorf("after attach: filesystems %v state %v", inst.GetSpec().GetFilesystems(), inst.GetStatus().GetState())
	}
	for _, id := range []string{a, b} {
		if got := fsAttachments(t, f, id); !slices.Equal(got, []string{vmID}) {
			t.Errorf("filesystem %s attachments = %v, want [%s]", id, got, vmID)
		}
	}

	// An attached filesystem cannot be deleted.
	wantCode(t, "delete attached filesystem", f.delete(ctx, b), codes.FailedPrecondition, "")

	// Detach b through the instance spec; the filesystem is then deletable.
	err = admin.updateFS(t, inst, func(s *computepb.InstanceSpec) { s.Filesystems = s.GetFilesystems()[:1] })
	if err != nil {
		t.Fatalf("detach b: %v", err)
	}
	if got := fsAttachments(t, f, b); len(got) != 0 {
		t.Errorf("filesystem %s attachments after detach = %v, want none", b, got)
	}
	if got := fsAttachments(t, f, a); !slices.Equal(got, []string{vmID}) {
		t.Errorf("filesystem %s attachments after detaching b = %v, want [%s]", a, got, vmID)
	}
	if err := f.delete(ctx, b); err != nil {
		t.Fatalf("delete detached filesystem: %v", err)
	}
	_, err = f.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: b})
	if status.Code(err) != codes.NotFound {
		t.Errorf("get deleted filesystem: %v, want NotFound", err)
	}

	// Instance delete detaches the remaining filesystem, which survives.
	admin.mustDelete(t, vmID)
	if got := fsAttachments(t, f, a); len(got) != 0 {
		t.Errorf("filesystem %s attachments after instance delete = %v, want none", a, got)
	}
	got, err := f.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: a})
	if err != nil || got.GetStatus().GetState() != computepb.FilesystemStatus_READY {
		t.Errorf("filesystem after instance delete = %v, %v, want READY", got.GetStatus(), err)
	}
	if err := f.delete(ctx, a); err != nil {
		t.Errorf("delete filesystem after instance delete: %v", err)
	}
}
