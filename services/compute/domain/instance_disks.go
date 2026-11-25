package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// diskSlot is one attached disk of an instance spec.
type diskSlot struct {
	spec  *computepb.AttachedDiskSpec
	field string // e.g. "spec.secondary_disks[1]"
	boot  bool
}

func diskSlots(spec *computepb.InstanceSpec) []diskSlot {
	if spec == nil {
		return nil
	}
	out := make([]diskSlot, 0, 1+len(spec.GetSecondaryDisks()))
	if spec.GetBootDisk() != nil {
		out = append(out, diskSlot{spec: spec.GetBootDisk(), field: "spec.boot_disk", boot: true})
	}
	for i, d := range spec.GetSecondaryDisks() {
		out = append(out, diskSlot{spec: d, field: fmt.Sprintf("spec.secondary_disks[%d]", i)})
	}
	return out
}

// resolvedSlot is a slot matched to a disk: an ID, or "" for a managed disk
// still to be created.
type resolvedSlot struct {
	diskSlot
	id string
	// switchToManaged: an attached existing disk becomes managed.
	switchToManaged bool
	// created is the new managed disk when id is "" (built by planDisks).
	created *computepb.Disk
}

// diskPlan is how an instance's disks change: the attachments before and
// the resolved slots after.
type diskPlan struct {
	current []*computepb.DiskAttachmentStatus
	slots   []resolvedSlot
}

// reconcileDisks makes the disks attached to inst match next, given the
// spec it had before (prev; nil on create). next nil detaches everything
// (instance delete). It returns the new status.disk_attachments, boot disk
// first. Managed disks are matched by name (instance.proto, ManagedDisk):
// a new name creates a disk, a name that disappears deletes it.
//
// planDisks runs outside the store transaction: it validates new managed
// disks with the DiskService rules (which read images and projects).
// applyDisks then runs inside it.
func (s *InstanceService) planDisks(ctx context.Context, inst *computepb.Instance, prev, next *computepb.InstanceSpec) (diskPlan, error) {
	current := inst.GetStatus().GetDiskAttachments()
	if prev == nil {
		current = nil
	}
	slots, err := s.resolveSlots(current, next)
	if err != nil {
		return diskPlan{}, err
	}
	if prev != nil && next != nil && len(current) > 0 && len(slots) > 0 && slots[0].id != current[0].GetId() {
		return diskPlan{}, errs.BadRequest(errs.Violation{
			Field: "spec.boot_disk", Message: "the boot disk cannot be detached or replaced; recreate the instance to change it",
		})
	}
	for i := range slots {
		if slots[i].id == "" {
			if slots[i].created, err = s.newManagedDisk(ctx, inst, slots[i]); err != nil {
				return diskPlan{}, err
			}
		}
	}
	return diskPlan{current: current, slots: slots}, nil
}

// applyDisks applies a plan inside the store transaction and returns the new
// status.disk_attachments.
func (s *InstanceService) applyDisks(tx InstanceTx, inst *computepb.Instance, plan diskPlan) ([]*computepb.DiskAttachmentStatus, error) {
	instID := inst.GetMetadata().GetId()
	current, slots := plan.current, plan.slots
	keep := map[string]bool{}
	for _, sl := range slots {
		if sl.id != "" {
			keep[sl.id] = true
		}
	}
	for _, a := range current {
		if keep[a.GetId()] {
			continue
		}
		if err := detachDisk(tx, a, instID); err != nil {
			return nil, err
		}
	}
	out := make([]*computepb.DiskAttachmentStatus, 0, len(slots))
	for _, sl := range slots {
		d, err := s.attachSlot(tx, inst, sl, current)
		if err != nil {
			return nil, err
		}
		out = append(out, &computepb.DiskAttachmentStatus{
			Id: d.GetMetadata().GetId(), Name: d.GetMetadata().GetName(), IsManaged: sl.spec.GetManagedDisk() != nil,
		})
	}
	return out, nil
}

// resolveSlots matches the slots of next to disk IDs and checks that no
// disk, managed disk name or device ID appears twice.
func (s *InstanceService) resolveSlots(current []*computepb.DiskAttachmentStatus, next *computepb.InstanceSpec) ([]resolvedSlot, error) {
	var (
		out     []resolvedSlot
		ids     = map[string]string{}
		names   = map[string]string{}
		devices = map[string]string{}
	)
	for _, sl := range diskSlots(next) {
		r := resolvedSlot{diskSlot: sl}
		if dev := sl.spec.GetDeviceId(); dev != "" {
			if other, dup := devices[dev]; dup {
				return nil, errs.BadRequest(errs.Violation{Field: sl.field + ".device_id", Message: fmt.Sprintf("device_id %q is also used by %s", dev, other)})
			}
			devices[dev] = sl.field
		}
		if err := matchSlot(&r, current, names); err != nil {
			return nil, err
		}
		if r.id != "" {
			if other, dup := ids[r.id]; dup {
				return nil, errs.BadRequest(errs.Violation{Field: sl.field, Message: fmt.Sprintf("disk %s is also attached by %s", r.id, other)})
			}
			ids[r.id] = sl.field
		}
		out = append(out, r)
	}
	return out, nil
}

// matchSlot finds the disk of one slot: the existing disk's ID, or the
// attached disk whose name a managed disk declares ("" when it is new).
// names collects the managed disk names seen so far.
func matchSlot(r *resolvedSlot, current []*computepb.DiskAttachmentStatus, names map[string]string) error {
	sl := r.diskSlot
	switch {
	case sl.spec.GetExistingDisk() != nil:
		r.id = sl.spec.GetExistingDisk().GetId()
		if r.id == "" {
			return errs.BadRequest(errs.Violation{Field: sl.field + ".existing_disk.id", Message: "required"})
		}
	case sl.spec.GetManagedDisk() != nil:
		name := sl.spec.GetManagedDisk().GetName()
		if name == "" {
			return errs.BadRequest(errs.Violation{Field: sl.field + ".managed_disk.name", Message: "required"})
		}
		if other, dup := names[name]; dup {
			return errs.BadRequest(errs.Violation{Field: sl.field + ".managed_disk.name", Message: fmt.Sprintf("managed disk %q is also declared by %s", name, other)})
		}
		names[name] = sl.field
		if i := slices.IndexFunc(current, func(a *computepb.DiskAttachmentStatus) bool { return a.GetName() == name }); i >= 0 {
			r.id = current[i].GetId()
			r.switchToManaged = !current[i].GetIsManaged()
		}
	default:
		return errs.BadRequest(errs.Violation{Field: sl.field, Message: "one of existing_disk or managed_disk is required"})
	}
	return nil
}

// detachDisk removes an attachment that the new spec no longer has: a
// managed disk is deleted (renaming replaces it), an existing disk is
// detached and kept.
func detachDisk(tx InstanceTx, a *computepb.DiskAttachmentStatus, instID string) error {
	d, err := tx.Disk(a.GetId())
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.GetIsManaged() && d.GetStatus().GetManagedBy() == instID {
		if d.GetSpec().GetForbidDeletion() {
			return errs.BadState(a.GetId(), fmt.Sprintf("managed disk %s has forbid_deletion set and cannot be deleted", a.GetName()))
		}
		return tx.DeleteDisk(a.GetId())
	}
	clearAttachment(d, instID)
	return tx.PutDisk(d)
}

// attachSlot creates or updates the disk of one slot and records the
// attachment on it. current are the attachments before the change.
func (s *InstanceService) attachSlot(tx InstanceTx, inst *computepb.Instance, sl resolvedSlot, current []*computepb.DiskAttachmentStatus) (*computepb.Disk, error) {
	instID, project := inst.GetMetadata().GetId(), inst.GetMetadata().GetParentId()
	if sl.id == "" {
		return createManagedDisk(tx, inst, sl)
	}
	d, err := tx.Disk(sl.id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sl.field, err)
	}
	id := d.GetMetadata().GetId()
	if d.GetMetadata().GetParentId() != project {
		return nil, errs.BadRequest(errs.Violation{Field: sl.field + ".existing_disk.id", Message: fmt.Sprintf("disk %s is not in project %s", id, project)})
	}
	if by := d.GetStatus().GetManagedBy(); by != "" && by != instID {
		return nil, errs.BadState(id, fmt.Sprintf("disk %s is managed by instance %s", id, by))
	}
	attached := slices.ContainsFunc(current, func(a *computepb.DiskAttachmentStatus) bool { return a.GetId() == sl.id })
	if !attached && d.GetStatus().GetState() != computepb.DiskStatus_READY {
		return nil, errs.BadState(id, fmt.Sprintf("disk %s is %s: only READY disks can be attached", id, d.GetStatus().GetState()))
	}
	switch m := sl.spec.GetManagedDisk(); {
	case m == nil:
		d.Status.ManagedBy = "" // a managed disk switched to existing_disk is kept on delete
	case sl.switchToManaged:
		if d.GetSpec().GetForbidDeletion() {
			return nil, errs.BadState(id, "a disk with forbid_deletion set cannot become a managed disk")
		}
		if !proto.Equal(d.GetSpec(), m.GetSpec()) {
			return nil, errs.BadRequest(errs.Violation{
				Field:   sl.field + ".managed_disk.spec",
				Message: fmt.Sprintf("to switch disk %s to a managed disk, the spec must match its current spec exactly (see DiskService.Get)", id),
			})
		}
		d.Status.ManagedBy = instID
		d.Metadata.Labels = maps.Clone(m.GetLabels())
	default:
		if err := s.updateManagedDisk(d, m, sl.field+".managed_disk"); err != nil {
			return nil, err
		}
	}
	if err := setAttachment(d, instID, sl.spec.GetAttachMode(), sl.field); err != nil {
		return nil, err
	}
	if err := tx.PutDisk(d); err != nil {
		return nil, err
	}
	return d, nil
}

// newManagedDisk builds a managed disk with the DiskService rules. It is
// READY at once: the mocker keeps no disk data, so there is nothing to copy.
func (s *InstanceService) newManagedDisk(ctx context.Context, inst *computepb.Instance, sl resolvedSlot) (*computepb.Disk, error) {
	m := sl.spec.GetManagedDisk()
	field := sl.field + ".managed_disk"
	if m.GetSpec() == nil {
		return nil, errs.BadRequest(errs.Violation{Field: field + ".spec", Message: "required"})
	}
	d, _, err := s.d.Disks.newDisk(ctx, CreateDiskCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.GetMetadata().GetParentId(), Name: m.GetName(), Labels: m.GetLabels()},
		Spec:     m.GetSpec(),
	})
	if err != nil {
		return nil, rebaseFields(err, "spec.", field+".spec.")
	}
	setDiskState(d, computepb.DiskStatus_READY)
	d.Status.ManagedBy = inst.GetMetadata().GetId()
	return d, nil
}

// createManagedDisk stores a planned managed disk with its attachment.
func createManagedDisk(tx InstanceTx, inst *computepb.Instance, sl resolvedSlot) (*computepb.Disk, error) {
	m, field, d := sl.spec.GetManagedDisk(), sl.field+".managed_disk", sl.created
	if err := setAttachment(d, inst.GetMetadata().GetId(), sl.spec.GetAttachMode(), sl.field); err != nil {
		return nil, err
	}
	if err := tx.CreateDisk(d); err != nil {
		if e, ok := errors.AsType[*errs.Error](err); ok && e.Kind == errs.KindAlreadyExists {
			return nil, errs.AlreadyExists(e.ResourceID, fmt.Sprintf(
				"%s.name: a disk named %q already exists in project %s", field, m.GetName(), inst.GetMetadata().GetParentId()))
		}
		return nil, err
	}
	return d, nil
}

// updateManagedDisk applies a managed disk's spec and labels from the
// instance spec: size may grow and forbid_deletion may change; type, block
// size, source and encryption are immutable (Terraform replaces the
// instance for those).
func (s *InstanceService) updateManagedDisk(d *computepb.Disk, m *computepb.ManagedDisk, field string) error {
	next := proto.CloneOf(m.GetSpec())
	if next == nil {
		return errs.BadRequest(errs.Violation{Field: field + ".spec", Message: "required"})
	}
	checked := proto.CloneOf(next)
	if err := keepDiskImmutable(d.GetSpec(), checked); err != nil { // fills unset immutable fields for the size checks
		return rebaseFields(err, "spec.", field+".spec.")
	}
	size, err := SizeBytes(checked)
	if err != nil {
		return rebaseFields(err, "spec.", field+".spec.")
	}
	current := d.GetStatus().GetSizeBytes()
	if size < current {
		return errs.BadRequest(errs.Violation{
			Field: field + ".spec.size", Message: fmt.Sprintf("disk size cannot be reduced: %d bytes is less than the current %d bytes", size, current),
		})
	}
	if err := checkSize(checked.GetType(), d.GetStatus().GetBlockSizeBytes(), size, d.GetStatus().GetSourceImageId() != ""); err != nil {
		return rebaseFields(err, "spec.", field+".spec.")
	}
	labels := m.GetLabels()
	if proto.Equal(d.GetSpec(), next) && maps.Equal(d.GetMetadata().GetLabels(), labels) {
		return nil
	}
	d.Spec = next // echoed exactly as the instance spec declares it
	d.Status.SizeBytes = size
	d.Metadata.Labels = maps.Clone(labels)
	d.Metadata.ResourceVersion++
	d.Metadata.UpdatedAt = timestamppb.New(s.d.Clock.Now())
	return nil
}

// setAttachment records instID's attachment on d in the given mode. A
// read-write attachment excludes every other attachment; a read-only one
// excludes a read-write attachment elsewhere.
func setAttachment(d *computepb.Disk, instID string, mode computepb.AttachedDiskSpec_AttachMode, field string) error {
	clearAttachment(d, instID)
	st, id := d.GetStatus(), d.GetMetadata().GetId()
	switch mode {
	case computepb.AttachedDiskSpec_READ_WRITE:
		if st.GetReadWriteAttachment() != "" || len(st.GetReadOnlyAttachments()) > 0 {
			return errs.BadState(id, fmt.Sprintf("disk %s is already attached to %s", id, attachedTo(st)))
		}
		st.ReadWriteAttachment = instID
	case computepb.AttachedDiskSpec_READ_ONLY:
		if rw := st.GetReadWriteAttachment(); rw != "" {
			return errs.BadState(id, fmt.Sprintf("disk %s is attached read-write to %s", id, rw))
		}
		st.ReadOnlyAttachments = append(st.ReadOnlyAttachments, instID)
	case computepb.AttachedDiskSpec_UNSPECIFIED:
		return errs.BadRequest(errs.Violation{Field: field + ".attach_mode", Message: "must be READ_ONLY or READ_WRITE"})
	default:
		return errs.BadRequest(errs.Violation{Field: field + ".attach_mode", Message: "must be READ_ONLY or READ_WRITE"})
	}
	return nil
}

func clearAttachment(d *computepb.Disk, instID string) {
	if d.GetStatus() == nil {
		d.Status = &computepb.DiskStatus{}
	}
	st := d.GetStatus()
	if st.GetReadWriteAttachment() == instID {
		st.ReadWriteAttachment = ""
	}
	st.ReadOnlyAttachments = slices.DeleteFunc(st.GetReadOnlyAttachments(), func(s string) bool { return s == instID })
}

func attachedTo(st *computepb.DiskStatus) string {
	all := slices.Clone(st.GetReadOnlyAttachments())
	if rw := st.GetReadWriteAttachment(); rw != "" {
		all = append([]string{rw}, all...)
	}
	return strings.Join(all, ", ")
}

// rebaseFields rewrites violation fields that start with from (DiskService
// rules report "spec.size") to start with to.
func rebaseFields(err error, from, to string) error {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || len(e.Violations) == 0 {
		return err
	}
	out := *e
	out.Violations = make([]errs.Violation, len(e.Violations))
	for i, v := range e.Violations {
		f := v.Field
		if rest, cut := strings.CutPrefix(f, from); cut {
			f = to + rest
		}
		out.Violations[i] = errs.Violation{Field: f, Message: v.Message}
	}
	return &out
}
