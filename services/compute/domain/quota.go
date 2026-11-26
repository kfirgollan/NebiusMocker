package domain

import (
	"context"
	"fmt"
	"sort"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// admit runs apply under the quota gate when the claim adds consumption, and
// directly otherwise (a shrink or an unchanged size needs no check).
func admit(ctx context.Context, gate QuotaGate, project string, amounts map[string]int64, apply func(ctx context.Context) error) error {
	for _, n := range amounts {
		if n > 0 {
			return gate.Admit(ctx, peer.QuotaClaim{ContainerID: project, Amounts: amounts}, apply)
		}
	}
	return apply(ctx)
}

// addAmounts adds src to dst.
func addAmounts(dst, src map[string]int64) {
	for k, v := range src {
		dst[k] += v
	}
}

// diffAmounts returns next - prev without the zero entries.
func diffAmounts(prev, next map[string]int64) map[string]int64 {
	out := map[string]int64{}
	addAmounts(out, next)
	for k, v := range prev {
		out[k] -= v
	}
	for k, v := range out {
		if v == 0 {
			delete(out, k)
		}
	}
	return out
}

// diskSize is the capacity a disk is charged: its status size, or the spec
// size while a resize has not reached the status yet.
func diskSize(d *computepb.Disk) int64 {
	size := d.GetStatus().GetSizeBytes()
	if n, err := SizeBytes(d.GetSpec()); err == nil {
		size = max(size, n)
	}
	return size
}

// diskAmounts is the consumption of one disk of the given type and size.
func diskAmounts(typ computepb.DiskSpec_DiskType, size int64) map[string]int64 {
	return map[string]int64{quota.ComputeDiskCount: 1, quota.ComputeDiskSize(typ.String()): size}
}

// filesystemSize is diskSize for filesystems.
func filesystemSize(f *computepb.Filesystem) int64 {
	size := f.GetStatus().GetSizeBytes()
	if n, err := FilesystemSizeBytes(f.GetSpec()); err == nil {
		size = max(size, n)
	}
	return size
}

func filesystemAmounts(typ computepb.FilesystemSpec_FilesystemType, size int64) map[string]int64 {
	return map[string]int64{quota.ComputeFilesystemCount: 1, quota.ComputeFilesystemSize(typ.String()): size}
}

// snapshotAmounts is the consumption of one snapshot.
func snapshotAmounts(s *computepb.DiskSnapshot) map[string]int64 {
	return map[string]int64{quota.ComputeDiskSnapshotCount: 1, quota.ComputeDiskSnapshotSize: s.GetStatus().GetStorageSizeBytes()}
}

// presetAmounts is the consumption of inst running on preset pp: a
// preemptible VM counts apart from a regular one; non-GPU platforms charge
// their vCPUs, and GPU platforms their GPUs (regular VMs only, and not while
// the VM draws from a capacity reservation, status.reservation_id: docs
// "GPUs allocated from reservations do not count towards quotas on the
// number of GPUs").
func presetAmounts(inst *computepb.Instance, pp PlatformPreset) map[string]int64 {
	spec, res := inst.GetSpec(), pp.Preset.GetResources()
	out := map[string]int64{quota.ComputeInstanceCount: 1}
	if spec.GetPreemptible() != nil {
		delete(out, quota.ComputeInstanceCount)
		out[quota.ComputeInstancePreemptibleCount] = 1
	}
	gpu := quota.ComputeInstanceGPU(pp.Platform.GetMetadata().GetName())
	switch {
	case gpu == "":
		out[quota.ComputeInstanceNonGPUVCPU] = int64(res.GetVcpuCount())
	case spec.GetPreemptible() == nil && inst.GetStatus().GetReservationId() == "":
		out[gpu] = int64(res.GetGpuCount())
	}
	return out
}

// instanceAmounts resolves the preset of inst in its project.
func (s *InstanceService) instanceAmounts(ctx context.Context, inst *computepb.Instance) (map[string]int64, error) {
	spec := inst.GetSpec()
	pp, err := s.d.Platforms.Resolve(ctx, inst.GetMetadata().GetParentId(), spec.GetResources().GetPlatform(), spec.GetResources().GetPreset())
	if err != nil {
		return nil, err
	}
	return presetAmounts(inst, pp), nil
}

// admitCreate claims the instance and the managed disks its plan creates,
// then runs apply, the transaction that stores them.
func (s *InstanceService) admitCreate(ctx context.Context, inst *computepb.Instance, plan diskPlan, apply func(ctx context.Context) error) error {
	project := inst.GetMetadata().GetParentId()
	amounts, err := s.instanceAmounts(ctx, inst)
	if err != nil {
		return err
	}
	for _, sl := range plan.slots {
		if sl.created != nil {
			addAmounts(amounts, diskAmounts(sl.created.GetSpec().GetType(), diskSize(sl.created)))
		}
	}
	return admit(ctx, s.d.Quota, project, amounts, apply)
}

// admitUpdate claims what an update adds to the instance (a larger preset),
// the managed disks it creates and the growth of those it resizes, then runs
// apply, the transaction that stores them.
func (s *InstanceService) admitUpdate(ctx context.Context, stored, updated *computepb.Instance, plan diskPlan, apply func(ctx context.Context) error) error {
	project := stored.GetMetadata().GetParentId()
	prev, err := s.instanceAmounts(ctx, stored)
	if err != nil {
		return err
	}
	next, err := s.instanceAmounts(ctx, updated)
	if err != nil {
		return err
	}
	amounts := diffAmounts(prev, next)
	for _, sl := range plan.slots {
		switch m := sl.spec.GetManagedDisk(); {
		case sl.created != nil:
			addAmounts(amounts, diskAmounts(sl.created.GetSpec().GetType(), diskSize(sl.created)))
		case sl.id != "" && m != nil:
			d, err := s.d.Disks.d.Disks.Get(ctx, sl.id)
			if err != nil {
				return fmt.Errorf("%s: %w", sl.field, err)
			}
			if size, err := SizeBytes(m.GetSpec()); err == nil && size > diskSize(d) {
				amounts[quota.ComputeDiskSize(d.GetSpec().GetType().String())] += size - diskSize(d)
			}
		}
	}
	return admit(ctx, s.d.Quota, project, amounts, apply)
}

// QuotaUsage implements peer.QuotaUsageSource: it counts every stored compute
// resource per project, whatever its state, including the managed disks of
// instances and mk8s nodes. Instances charge the vCPUs or GPUs of their
// preset; one whose preset the catalog no longer offers counts only itself.
type QuotaUsage struct {
	records   QuotaRecords
	platforms PlatformResolver
}

var _ peer.QuotaUsageSource = (*QuotaUsage)(nil)

// NewQuotaUsage returns the compute quota usage source.
func NewQuotaUsage(records QuotaRecords, platforms PlatformResolver) *QuotaUsage {
	return &QuotaUsage{records: records, platforms: platforms}
}

type usageKey struct{ project, name string }

// QuotaUsage implements peer.QuotaUsageSource.
func (u *QuotaUsage) QuotaUsage(ctx context.Context) ([]peer.QuotaUsage, error) {
	acc := map[usageKey]uint64{}
	add := func(project string, amounts map[string]int64) {
		for name, n := range amounts {
			if n > 0 {
				acc[usageKey{project, name}] += uint64(n)
			}
		}
	}
	if err := u.countInstances(ctx, add); err != nil {
		return nil, err
	}
	disks, err := u.records.Disks(ctx)
	if err != nil {
		return nil, fmt.Errorf("count disks: %w", err)
	}
	for _, d := range disks {
		add(d.GetMetadata().GetParentId(), diskAmounts(d.GetSpec().GetType(), diskSize(d)))
	}
	snaps, err := u.records.Snapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("count disk snapshots: %w", err)
	}
	for _, sn := range snaps {
		add(sn.GetMetadata().GetParentId(), snapshotAmounts(sn))
	}
	filesystems, err := u.records.Filesystems(ctx)
	if err != nil {
		return nil, fmt.Errorf("count filesystems: %w", err)
	}
	for _, f := range filesystems {
		add(f.GetMetadata().GetParentId(), filesystemAmounts(f.GetSpec().GetType(), filesystemSize(f)))
	}
	clusters, err := u.records.GpuClusters(ctx)
	if err != nil {
		return nil, fmt.Errorf("count GPU clusters: %w", err)
	}
	for _, c := range clusters {
		add(c.GetMetadata().GetParentId(), map[string]int64{quota.ComputeGPUClusterCount: 1})
	}
	out := make([]peer.QuotaUsage, 0, len(acc))
	for k, n := range acc {
		out = append(out, peer.QuotaUsage{ContainerID: k.project, Name: k.name, Amount: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContainerID != out[j].ContainerID {
			return out[i].ContainerID < out[j].ContainerID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// countInstances adds the consumption of every stored instance through add.
func (u *QuotaUsage) countInstances(ctx context.Context, add func(project string, amounts map[string]int64)) error {
	instances, err := u.records.Instances(ctx)
	if err != nil {
		return fmt.Errorf("count instances: %w", err)
	}
	for _, inst := range instances {
		project, spec := inst.GetMetadata().GetParentId(), inst.GetSpec()
		pp, err := u.platforms.Resolve(ctx, project, spec.GetResources().GetPlatform(), spec.GetResources().GetPreset())
		switch {
		case err == nil:
			add(project, presetAmounts(inst, pp))
		case errs.IsKind(err, errs.KindBadRequest) || errs.IsKind(err, errs.KindNotFound):
			add(project, map[string]int64{quota.ComputeInstanceCount: 1})
		default:
			return fmt.Errorf("count instance %s: %w", inst.GetMetadata().GetId(), err)
		}
	}
	return nil
}
