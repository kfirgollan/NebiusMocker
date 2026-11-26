package domain

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// fakeQuotaGate records the claims it admits and refuses them all when refuse
// is set (apply then does not run, as in the quotas module).
type fakeQuotaGate struct {
	mu     sync.Mutex
	refuse bool
	claims []peer.QuotaClaim
}

var _ QuotaGate = (*fakeQuotaGate)(nil)

func (g *fakeQuotaGate) Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error {
	g.mu.Lock()
	g.claims = append(g.claims, c)
	refuse := g.refuse
	g.mu.Unlock()
	if refuse {
		return errs.QuotaExceeded("quota exhausted")
	}
	return apply(ctx)
}

// only returns the one recorded claim, failing unless there is exactly one.
func (g *fakeQuotaGate) only(t *testing.T) peer.QuotaClaim {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.claims) != 1 {
		t.Fatalf("claims = %+v, want exactly one", g.claims)
	}
	return g.claims[0]
}

func (g *fakeQuotaGate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.claims = nil
}

func wantClaim(t *testing.T, got peer.QuotaClaim, want map[string]int64) {
	t.Helper()
	if got.ContainerID != testProject || got.Region != "" {
		t.Fatalf("claim charged to %q/%q, want project %s with no region", got.ContainerID, got.Region, testProject)
	}
	if diff := cmp.Diff(want, got.Amounts); diff != "" {
		t.Fatalf("claim amounts (-want +got):\n%s", diff)
	}
}

func wantQuotaExceeded(t *testing.T, err error) {
	t.Helper()
	if !errs.IsKind(err, errs.KindQuotaExceeded) {
		t.Fatalf("err = %v, want QuotaExceeded", err)
	}
}

var (
	ssdQuota = quota.ComputeDiskSize("NETWORK_SSD")
	fsQuota  = quota.ComputeFilesystemSize("NETWORK_SSD")
)

func TestInstanceCreateClaims(t *testing.T) {
	t.Parallel()
	preemptibleGPU := func() *computepb.InstanceSpec {
		s := baseSpec()
		s.Resources = &computepb.ResourcesSpec{Platform: "gpu-l40s-a", Size: presetSize("1gpu-8vcpu-32gb")}
		s.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
		s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
		return s
	}
	withSecondary := baseSpec()
	withSecondary.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk("data", 50)}
	tests := []struct {
		name string
		spec *computepb.InstanceSpec
		want map[string]int64
	}{
		{
			name: "cpu vm charges count, vcpus and its boot disk",
			spec: baseSpec(),
			want: map[string]int64{
				quota.ComputeInstanceCount: 1, quota.ComputeInstanceNonGPUVCPU: 2,
				quota.ComputeDiskCount: 1, ssdQuota: 20 * gib,
			},
		},
		{
			name: "gpu vm charges its gpus and no vcpus",
			spec: gpuSpec(),
			want: map[string]int64{
				quota.ComputeInstanceCount: 1, "compute.instance.gpu.h100": 8,
				quota.ComputeDiskCount: 1, ssdQuota: 20 * gib,
			},
		},
		{
			name: "preemptible vm counts apart and takes no gpu quota",
			spec: preemptibleGPU(),
			want: map[string]int64{
				quota.ComputeInstancePreemptibleCount: 1,
				quota.ComputeDiskCount:                1, ssdQuota: 20 * gib,
			},
		},
		{
			name: "every managed disk is charged",
			spec: withSecondary,
			want: map[string]int64{
				quota.ComputeInstanceCount: 1, quota.ComputeInstanceNonGPUVCPU: 2,
				quota.ComputeDiskCount: 2, ssdQuota: 70 * gib,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInstFixture(t)
			f.mustCreate(t, "vm", tt.spec)
			wantClaim(t, f.gate.only(t), tt.want)

			r := newInstFixture(t)
			r.gate.refuse = true
			_, err := r.create("vm", tt.spec)
			wantQuotaExceeded(t, err)
			if _, gerr := r.repo.GetByName(t.Context(), testProject, "vm"); !errs.IsKind(gerr, errs.KindNotFound) {
				t.Fatalf("instance stored after a refused claim: %v", gerr)
			}
			if n := r.diskCount(); n != 0 {
				t.Fatalf("%d disks stored after a refused claim, want none", n)
			}
		})
	}
}

func TestInstanceCreateExistingDiskClaimsNoDisk(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	d := f.mkDisk(t, "boot", 20)
	f.gate.reset()
	spec := baseSpec()
	spec.BootDisk = existingDisk(d.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_WRITE)
	f.mustCreate(t, "vm", spec)
	wantClaim(t, f.gate.only(t), map[string]int64{quota.ComputeInstanceCount: 1, quota.ComputeInstanceNonGPUVCPU: 2})
}

func TestInstanceUpdateClaimsOnlyTheGrowth(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}

	// A larger preset claims the extra vCPUs, not another instance.
	f.gate.reset()
	grow := func(s *computepb.InstanceSpec) {
		s.Resources = &computepb.ResourcesSpec{Platform: "cpu-e2", Size: presetSize("8vcpu-32gb")}
	}
	if err := f.update(t, id, grow, nil); err != nil {
		t.Fatalf("grow preset: %v", err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{quota.ComputeInstanceNonGPUVCPU: 6})

	// A smaller preset claims nothing.
	f.gate.reset()
	if err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.Resources = &computepb.ResourcesSpec{Platform: "cpu-e2", Size: presetSize("4vcpu-16gb")}
	}, nil); err != nil {
		t.Fatalf("shrink preset: %v", err)
	}
	if len(f.gate.claims) != 0 {
		t.Fatalf("shrinking the preset claimed %+v", f.gate.claims)
	}

	// A refused growth changes nothing.
	f.gate.refuse = true
	wantQuotaExceeded(t, f.update(t, id, grow, nil))
	if got := f.get(t, id).GetSpec().GetResources().GetPreset(); got != "4vcpu-16gb" {
		t.Fatalf("preset = %s after a refused update, want 4vcpu-16gb", got)
	}
}

func TestInstanceUpdateClaimsManagedDisks(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()

	f.gate.reset()
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.BootDisk = managedDisk("boot", 30) }, nil); err != nil {
		t.Fatalf("resize boot disk: %v", err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{ssdQuota: 10 * gib})

	f.gate.reset()
	if err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk("data", 5)}
	}, nil); err != nil {
		t.Fatalf("add disk: %v", err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{quota.ComputeDiskCount: 1, ssdQuota: 5 * gib})

	f.gate.refuse = true
	wantQuotaExceeded(t, f.update(t, id, func(s *computepb.InstanceSpec) { s.BootDisk = managedDisk("boot", 40) }, nil))
	if got := f.disk(t, f.get(t, id).GetStatus().GetDiskAttachments()[0].GetId()).GetStatus().GetSizeBytes(); got != 30*gib {
		t.Fatalf("boot disk = %d bytes after a refused resize, want 30 GiB", got)
	}
}

func TestCreateNodeClaimsInstanceAndBootDiskTogether(t *testing.T) {
	t.Parallel()
	f := nodeFixture(t)
	if _, err := f.svc.CreateNode(f.ctx, nodeSpecFor("node-0", cpuTemplate())); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{
		quota.ComputeInstanceCount: 1, quota.ComputeInstanceNonGPUVCPU: 2,
		quota.ComputeDiskCount: 1, ssdQuota: nodeBoot,
	})

	r := nodeFixture(t)
	r.gate.refuse = true
	_, err := r.svc.CreateNode(r.ctx, nodeSpecFor("node-0", cpuTemplate()))
	wantQuotaExceeded(t, err)
	if n := r.diskCount(); n != 0 {
		t.Fatalf("%d disks stored after a refused node, want none", n)
	}
	if nodes, lerr := r.svc.ListNodes(r.ctx, nodeGroup); lerr != nil || len(nodes) != 0 {
		t.Fatalf("nodes = %v / %v after a refused node, want none", nodes, lerr)
	}
	if recs, lerr := r.nodes.List(r.ctx, nodeGroup); lerr != nil || len(recs) != 0 {
		t.Fatalf("node records = %v / %v after a refused node, want none", recs, lerr)
	}
}

func TestDiskClaims(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	if _, err := f.createDisk(t, "d", ssdSpec(10)); err != nil {
		t.Fatal(err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{quota.ComputeDiskCount: 1, ssdQuota: 10 * gib})

	r := newDiskFixture(t)
	r.gate.refuse = true
	_, err := r.createDisk(t, "d", ssdSpec(10))
	wantQuotaExceeded(t, err)
}

func TestDiskCreateRefusedStoresNothing(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.gate.refuse = true
	_, err := f.ds.Create(t.Context(), CreateDiskCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "d"}, Spec: ssdSpec(10),
	})
	wantQuotaExceeded(t, err)
	if _, gerr := f.disks.GetByName(t.Context(), testProject, "d"); !errs.IsKind(gerr, errs.KindNotFound) {
		t.Fatalf("disk stored after a refused claim: %v", gerr)
	}
}

func TestDiskResizeClaimsTheDelta(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	f.ops.deferred = true
	md := &commonpb.ResourceMetadata{Id: "disk-1"}

	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: md, Spec: ssdSpec(25)}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{ssdQuota: 15 * gib})
	f.ops.finish()

	// Labels only: no claim.
	f.gate.reset()
	if err := f.updateDisk(t, UpdateDiskCmd{Metadata: &commonpb.ResourceMetadata{Id: "disk-1", Labels: map[string]string{"a": "b"}}}); err != nil {
		t.Fatalf("labels: %v", err)
	}
	if len(f.gate.claims) != 0 {
		t.Fatalf("a metadata update claimed %+v", f.gate.claims)
	}

	// A refused growth leaves the spec alone.
	f.gate.refuse = true
	f.ops.finish()
	wantQuotaExceeded(t, f.updateDisk(t, UpdateDiskCmd{Metadata: md, Spec: ssdSpec(40)}))
	if got := f.mustGetDisk(t, "disk-1").GetStatus().GetSizeBytes(); got != 25*gib {
		t.Fatalf("size = %d after a refused resize, want 25 GiB", got)
	}
}

func TestSnapshotClaims(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	if _, err := f.createSnapshot(t, testProject, "s", "disk-1", ""); err != nil {
		t.Fatal(err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{quota.ComputeDiskSnapshotCount: 1, quota.ComputeDiskSnapshotSize: 10 * gib})

	r := newDiskFixture(t)
	r.seedDisk(t, "disk-1", nil)
	r.gate.refuse = true
	_, err := r.createSnapshot(t, testProject, "s", "disk-1", "")
	wantQuotaExceeded(t, err)
	if r.mustGetDisk(t, "disk-1").GetStatus().GetLockState().GetSnapshots() != nil {
		t.Fatal("source disk stays locked by a refused snapshot")
	}
}

func TestFilesystemClaims(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	if _, err := f.create(t, "fs", filesystemSpecGiB(10)); err != nil {
		t.Fatal(err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{quota.ComputeFilesystemCount: 1, fsQuota: 10 * gib})

	r := newFsFixture(t)
	r.gate.refuse = true
	_, err := r.create(t, "fs", filesystemSpecGiB(10))
	wantQuotaExceeded(t, err)
	if _, gerr := r.repo.GetByName(t.Context(), testProject, "fs"); !errs.IsKind(gerr, errs.KindNotFound) {
		t.Fatalf("filesystem stored after a refused claim: %v", gerr)
	}
}

func TestFilesystemResizeClaimsTheDelta(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	f.seed(t, "fs-1", nil)
	f.ops.deferred = true
	if err := f.update(t, UpdateFilesystemCmd{Metadata: &commonpb.ResourceMetadata{Id: "fs-1"}, Spec: filesystemSpecGiB(16)}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	wantClaim(t, f.gate.only(t), map[string]int64{fsQuota: 6 * gib})
	f.ops.finish()

	f.gate.refuse = true
	wantQuotaExceeded(t, f.update(t, UpdateFilesystemCmd{Metadata: &commonpb.ResourceMetadata{Id: "fs-1"}, Spec: filesystemSpecGiB(30)}))
	if got := f.get(t, "fs-1").GetStatus().GetSizeBytes(); got != 16*gib {
		t.Fatalf("size = %d after a refused resize, want 16 GiB", got)
	}
}

func TestGpuClusterClaims(t *testing.T) {
	t.Parallel()
	f := newGpuFixture(t)
	f.create(t, "c")
	wantClaim(t, f.gate.only(t), map[string]int64{quota.ComputeGPUClusterCount: 1})

	r := newGpuFixture(t)
	r.gate.refuse = true
	_, err := r.svc.Create(t.Context(), CreateGpuClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "c"},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-3"},
	})
	wantQuotaExceeded(t, err)
	if _, gerr := r.clusters.GetByName(t.Context(), testProject, "c"); !errs.IsKind(gerr, errs.KindNotFound) {
		t.Fatalf("cluster stored after a refused claim: %v", gerr)
	}
}

// fakeRecords lists the stored resources of the fixtures for QuotaUsage.
type fakeRecords struct {
	instances   *memRepo[*computepb.Instance]
	disks       *memRepo[*computepb.Disk]
	snapshots   *memRepo[*computepb.DiskSnapshot]
	filesystems *memRepo[*computepb.Filesystem]
	clusters    *memRepo[*computepb.GpuCluster]
}

var _ QuotaRecords = fakeRecords{}

func allOf[T interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}](r *memRepo[T]) []T {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []T
	for _, id := range r.order {
		if v, ok := r.byID[id]; ok {
			out = append(out, v)
		}
	}
	return out
}

func (f fakeRecords) Instances(context.Context) ([]*computepb.Instance, error) {
	return allOf(f.instances), nil
}

func (f fakeRecords) Disks(context.Context) ([]*computepb.Disk, error) { return allOf(f.disks), nil }

func (f fakeRecords) Snapshots(context.Context) ([]*computepb.DiskSnapshot, error) {
	return allOf(f.snapshots), nil
}

func (f fakeRecords) Filesystems(context.Context) ([]*computepb.Filesystem, error) {
	return allOf(f.filesystems), nil
}

func (f fakeRecords) GpuClusters(context.Context) ([]*computepb.GpuCluster, error) {
	return allOf(f.clusters), nil
}

func usageMap(t *testing.T, u *QuotaUsage) map[string]uint64 {
	t.Helper()
	got, err := u.QuotaUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	for _, g := range got {
		if g.ContainerID != testProject {
			t.Fatalf("usage of %s, want only %s: %+v", g.ContainerID, testProject, g)
		}
		out[g.Name] = g.Amount
	}
	return out
}

func TestQuotaUsageCountsCreatesAndDeletes(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	snaps, fss, clusters := &fakeSnapshots{}, &fakeFilesystems{}, &fakeGpuClusters{}
	u := NewQuotaUsage(fakeRecords{
		instances: &f.repo.instances, disks: &f.disks.repo, snapshots: &snaps.repo, filesystems: &fss.repo, clusters: &clusters.repo,
	}, f.svc.d.Platforms)

	if got := usageMap(t, u); len(got) != 0 {
		t.Fatalf("usage of an empty store = %v", got)
	}
	cpu := f.mustCreate(t, "cpu", baseSpec())
	gpu := gpuSpec()
	gpu.BootDisk = managedDisk("gpu-boot", 20)
	f.mustCreate(t, "gpu", gpu)
	pre := baseSpec()
	pre.Resources = &computepb.ResourcesSpec{Platform: "gpu-l40s-a", Size: presetSize("1gpu-8vcpu-32gb")}
	pre.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
	pre.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
	pre.BootDisk = managedDisk("pre-boot", 20)
	f.mustCreate(t, "pre", pre)
	f.mkDisk(t, "extra", 7)
	// Resources compute stores outside these services' creates.
	if err := snaps.Create(t.Context(), &computepb.DiskSnapshot{
		Metadata: &commonpb.ResourceMetadata{Id: "snap-1", ParentId: testProject, Name: "snap"},
		Status:   &computepb.DiskSnapshotStatus{StorageSizeBytes: 3 * gib},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fss.Create(t.Context(), &computepb.Filesystem{
		Metadata: &commonpb.ResourceMetadata{Id: "fs-1", ParentId: testProject, Name: "fs"},
		Spec:     filesystemSpecGiB(12), Status: &computepb.FilesystemStatus{SizeBytes: 12 * gib},
	}); err != nil {
		t.Fatal(err)
	}
	if err := clusters.Create(t.Context(), &computepb.GpuCluster{Metadata: &commonpb.ResourceMetadata{Id: "gc-1", ParentId: testProject, Name: "c"}}); err != nil {
		t.Fatal(err)
	}

	want := map[string]uint64{
		quota.ComputeInstanceCount: 2, quota.ComputeInstancePreemptibleCount: 1,
		quota.ComputeInstanceNonGPUVCPU: 2, "compute.instance.gpu.h100": 8,
		quota.ComputeDiskCount: 4, ssdQuota: uint64(67 * gib),
		quota.ComputeDiskSnapshotCount: 1, quota.ComputeDiskSnapshotSize: uint64(3 * gib),
		quota.ComputeFilesystemCount: 1, fsQuota: uint64(12 * gib),
		quota.ComputeGPUClusterCount: 1,
	}
	if diff := cmp.Diff(want, usageMap(t, u)); diff != "" {
		t.Fatalf("usage after creates (-want +got):\n%s", diff)
	}

	// Deleting an instance frees its slot, vCPUs and managed disk.
	if err := f.remove(cpu.GetMetadata().GetId()); err != nil {
		t.Fatal(err)
	}
	want[quota.ComputeInstanceCount] = 1
	delete(want, quota.ComputeInstanceNonGPUVCPU)
	want[quota.ComputeDiskCount] = 3
	want[ssdQuota] = uint64(47 * gib)
	if diff := cmp.Diff(want, usageMap(t, u)); diff != "" {
		t.Fatalf("usage after a delete (-want +got):\n%s", diff)
	}
}

// TestGPUPlatformsHaveQuotas checks that every GPU platform of the default
// platform catalog is enforced: its GPU quota is one the quota catalog knows.
func TestGPUPlatformsHaveQuotas(t *testing.T) {
	t.Parallel()
	catalog, err := DefaultPlatformCatalog()
	if err != nil {
		t.Fatal(err)
	}
	quotas, err := quota.DefaultCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range catalog.Platforms {
		gpus := slices.ContainsFunc(p.Presets, func(pr PresetDef) bool { return pr.GPUCount > 0 })
		name := quota.ComputeInstanceGPU(p.Name)
		if gpus != (name != "") {
			t.Errorf("platform %s: has GPUs = %v but GPU quota = %q", p.Name, gpus, name)
			continue
		}
		if !gpus {
			continue
		}
		if _, ok := quotas.Lookup(name); !ok {
			t.Errorf("platform %s: the quota catalog does not know %q", p.Name, name)
		}
		family := name[len("compute.instance.gpu."):]
		if !slices.Contains(quota.GPUFamilies, family) {
			t.Errorf("platform %s: GPU family %q is missing from quota.GPUFamilies %v", p.Name, family, quota.GPUFamilies)
		}
	}
}
