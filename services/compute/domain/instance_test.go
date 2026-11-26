package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// ---- spec builders and helpers ----

func presetSize(p string) *computepb.ResourcesSpec_Preset {
	return &computepb.ResourcesSpec_Preset{Preset: p}
}

func managedDisk(name string, sizeGiB int64) *computepb.AttachedDiskSpec {
	return &computepb.AttachedDiskSpec{
		AttachMode: computepb.AttachedDiskSpec_READ_WRITE,
		Type:       &computepb.AttachedDiskSpec_ManagedDisk{ManagedDisk: &computepb.ManagedDisk{Name: name, Spec: ssdSpec(sizeGiB)}},
	}
}

func existingDisk(id string, mode computepb.AttachedDiskSpec_AttachMode) *computepb.AttachedDiskSpec {
	return &computepb.AttachedDiskSpec{
		AttachMode: mode,
		Type:       &computepb.AttachedDiskSpec_ExistingDisk{ExistingDisk: &computepb.ExistingDisk{Id: id}},
	}
}

func nicSpec(name, subnet string) *computepb.NetworkInterfaceSpec {
	return &computepb.NetworkInterfaceSpec{Name: name, SubnetId: subnet, IpAddress: &computepb.IPAddress{}}
}

func fsSpec(id, tag string) *computepb.AttachedFilesystemSpec {
	return &computepb.AttachedFilesystemSpec{
		AttachMode: computepb.AttachedFilesystemSpec_READ_WRITE, MountTag: tag,
		Type: &computepb.AttachedFilesystemSpec_ExistingFilesystem{ExistingFilesystem: &computepb.ExistingFilesystem{Id: id}},
	}
}

// baseSpec is a valid CPU instance: managed boot disk "boot" (20 GiB) and one dynamic NIC.
func baseSpec() *computepb.InstanceSpec {
	return &computepb.InstanceSpec{
		Resources:         &computepb.ResourcesSpec{Platform: "cpu-e2", Size: presetSize("2vcpu-8gb")},
		BootDisk:          managedDisk("boot", 20),
		NetworkInterfaces: []*computepb.NetworkInterfaceSpec{nicSpec("eth0", "subnet-a")},
	}
}

// gpuSpec is baseSpec on a clusterable 8-GPU H100 preset.
func gpuSpec() *computepb.InstanceSpec {
	s := baseSpec()
	s.Resources = &computepb.ResourcesSpec{Platform: "gpu-h100-sxm", Size: presetSize("8gpu-128vcpu-1600gb")}
	return s
}

// edited returns a clone of s changed by fn.
func edited(s *computepb.InstanceSpec, fn func(*computepb.InstanceSpec)) *computepb.InstanceSpec {
	c := proto.CloneOf(s)
	fn(c)
	return c
}

func (f *instFixture) create(name string, spec *computepb.InstanceSpec) (*computepb.Instance, error) {
	op, err := f.svc.Create(f.ctx, CreateInstanceCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name}, Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	if op.Err != nil {
		return nil, op.Err
	}
	return f.repo.Get(f.ctx, op.ResourceID)
}

func (f *instFixture) mustCreate(t *testing.T, name string, spec *computepb.InstanceSpec) *computepb.Instance {
	t.Helper()
	inst, err := f.create(name, spec)
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	return inst
}

func (f *instFixture) get(t *testing.T, id string) *computepb.Instance {
	t.Helper()
	inst, err := f.repo.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return inst
}

func (f *instFixture) disk(t *testing.T, id string) *computepb.Disk {
	t.Helper()
	d, err := f.disks.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("disk Get(%s): %v", id, err)
	}
	return d
}

func (f *instFixture) diskCount() int {
	f.disks.repo.mu.Lock()
	defer f.disks.repo.mu.Unlock()
	return len(f.disks.repo.byID)
}

// mkDisk creates a standalone READY disk through the DiskService.
func (f *instFixture) mkDisk(t *testing.T, name string, sizeGiB int64) *computepb.Disk {
	t.Helper()
	op, err := f.ds.Create(t.Context(), CreateDiskCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name}, Spec: ssdSpec(sizeGiB),
	})
	if err != nil || op.Err != nil {
		t.Fatalf("create disk %s: %v / %v", name, err, op)
	}
	return f.disk(t, op.ResourceID)
}

// update sends a full-replace update: spec edited by edit (nil: unchanged),
// labels (nil: unchanged).
func (f *instFixture) update(t *testing.T, id string, edit func(*computepb.InstanceSpec), labels map[string]string) error {
	t.Helper()
	cur := f.get(t, id)
	cmd := UpdateInstanceCmd{Metadata: &commonpb.ResourceMetadata{Id: id, Name: cur.GetMetadata().GetName(), Labels: cur.GetMetadata().GetLabels()}}
	paths := []string{"metadata.name"}
	if labels != nil {
		cmd.Metadata.Labels = labels
		paths = append(paths, "metadata.labels")
	}
	if edit != nil {
		cmd.Spec = edited(cur.GetSpec(), edit)
		paths = append(paths, "spec.*")
	}
	rm, err := mask.Parse(strings.Join(paths, ","))
	if err != nil {
		t.Fatal(err)
	}
	cmd.ResetMask = rm
	op, err := f.svc.Update(t.Context(), cmd)
	if err != nil {
		return err
	}
	return op.Err
}

func (f *instFixture) start(id string) error {
	op, err := f.svc.Start(f.ctx, id, nil)
	if err != nil {
		return err
	}
	return op.Err
}

func (f *instFixture) stop(id string) error {
	op, err := f.svc.Stop(f.ctx, id, nil)
	if err != nil {
		return err
	}
	return op.Err
}

func (f *instFixture) remove(id string) error {
	op, err := f.svc.Delete(f.ctx, id, nil)
	if err != nil {
		return err
	}
	return op.Err
}

// wantKind fails unless err has kind k and, for BadRequest with field != "",
// a violation on field.
func wantKind(t *testing.T, err error, k errs.Kind, field string) {
	t.Helper()
	if got := errs.KindOf(err); got != k {
		t.Fatalf("error kind = %v, want %v (err: %v)", got, k, err)
	}
	if field == "" {
		return
	}
	e, ok := errors.AsType[*errs.Error](err)
	if !ok {
		t.Fatalf("error %v is not a domain error", err)
	}
	for _, v := range e.Violations {
		if v.Field == field {
			return
		}
	}
	t.Fatalf("error %v has no violation on field %q (violations: %v)", err, field, e.Violations)
}

func instState(inst *computepb.Instance) computepb.InstanceStatus_InstanceState {
	return inst.GetStatus().GetState()
}

// ---- create ----

func TestInstanceCreateRunningEchoesSpecAndFillsStatus(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.Hostname = ""
	inst := f.mustCreate(t, "vm", spec)
	id := inst.GetMetadata().GetId()

	if instState(inst) != computepb.InstanceStatus_RUNNING || inst.GetStatus().GetReconciling() {
		t.Fatalf("status = %v, want RUNNING and not reconciling", inst.GetStatus())
	}
	if diff := cmp.Diff(baseSpec(), inst.GetSpec(), protocmp.Transform()); diff != "" {
		t.Fatalf("spec was not echoed as sent (-want +got):\n%s", diff)
	}
	if md := inst.GetMetadata(); md.GetResourceVersion() != 1 || md.GetParentId() != testProject || md.GetName() != "vm" {
		t.Fatalf("metadata = %v", md)
	}
	nics := inst.GetStatus().GetNetworkInterfaces()
	if len(nics) != 1 {
		t.Fatalf("nic statuses = %v", nics)
	}
	n := nics[0]
	if n.GetIndex() != 0 || n.GetName() != "eth0" || n.GetIpAddress().GetAddress() != "10.0.0.1" ||
		n.GetIpAddress().GetAllocationId() != id+"-eth0-private" || n.GetPublicIpAddress() != nil {
		t.Fatalf("nic status = %v", n)
	}
	if !strings.HasPrefix(n.GetMacAddress(), "02:4e:42:") || len(n.GetMacAddress()) != len("02:4e:42:00:00:00") {
		t.Fatalf("mac = %q, want 02:4e:42:xx:xx:xx", n.GetMacAddress())
	}
	if want := id + ".network-1.compute.internal."; n.GetFqdn() != want {
		t.Fatalf("fqdn = %q, want %q", n.GetFqdn(), want)
	}
	if got := n.GetSecurityGroups(); len(got) != 1 || got[0].GetId() != "securitygroup-default" {
		t.Fatalf("security groups = %v, want the network default", got)
	}
	atts := inst.GetStatus().GetDiskAttachments()
	if len(atts) != 1 || atts[0].GetName() != "boot" || !atts[0].GetIsManaged() {
		t.Fatalf("disk attachments = %v", atts)
	}
	d := f.disk(t, atts[0].GetId())
	if d.GetStatus().GetManagedBy() != id || d.GetStatus().GetReadWriteAttachment() != id ||
		d.GetStatus().GetState() != computepb.DiskStatus_READY || d.GetStatus().GetSizeBytes() != 20*gib {
		t.Fatalf("managed disk status = %v, want READY 20GiB managed_by and attached read-write to %s", d.GetStatus(), id)
	}
	if d.GetMetadata().GetName() != "boot" || d.GetMetadata().GetParentId() != testProject {
		t.Fatalf("managed disk metadata = %v", d.GetMetadata())
	}
}

func TestInstanceCreateStoppedAndHostname(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.Stopped = true
	spec.Hostname = "web"
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{}
	inst := f.mustCreate(t, "vm", spec)
	if instState(inst) != computepb.InstanceStatus_STOPPED {
		t.Fatalf("state = %v, want STOPPED", instState(inst))
	}
	if diff := cmp.Diff(spec, inst.GetSpec(), protocmp.Transform()); diff != "" {
		t.Fatalf("spec not echoed (-want +got):\n%s", diff)
	}
	n := inst.GetStatus().GetNetworkInterfaces()[0]
	if n.GetFqdn() != "web.network-1.compute.internal." {
		t.Fatalf("fqdn = %q, want the hostname", n.GetFqdn())
	}
	if n.GetPublicIpAddress() != nil {
		t.Fatalf("a stopped instance must not hold a dynamic public address, got %v", n.GetPublicIpAddress())
	}
}

func TestInstanceCreatePublicAndAliasAndSecurityGroups(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	nic := spec.GetNetworkInterfaces()[0]
	nic.PublicIpAddress = &computepb.PublicIPAddress{}
	nic.Aliases = []*computepb.IPAlias{{AllocationId: "vpcallocation-alias1"}}
	nic.SecurityGroups = []*computepb.SecurityGroup{{Id: "securitygroup-x"}}
	inst := f.mustCreate(t, "vm", spec)
	n := inst.GetStatus().GetNetworkInterfaces()[0]
	if n.GetPublicIpAddress().GetAddress() != "203.0.113.1" || n.GetPublicIpAddress().GetStatic() {
		t.Fatalf("public ip = %v, want dynamic 203.0.113.1", n.GetPublicIpAddress())
	}
	if len(n.GetAliases().GetCidrs()) != 1 {
		t.Fatalf("aliases = %v", n.GetAliases())
	}
	if got := n.GetSecurityGroups(); len(got) != 1 || got[0].GetId() != "securitygroup-x" {
		t.Fatalf("security groups = %v", got)
	}
	b, ok := f.net.binding(inst.GetMetadata().GetId(), "eth0")
	if !ok || b.Public == nil || b.SubnetID != "subnet-a" || b.ProjectID != testProject ||
		!slices.Equal(b.AliasAllocationIDs, []string{"vpcallocation-alias1"}) || !slices.Equal(b.SecurityGroupIDs, []string{"securitygroup-x"}) {
		t.Fatalf("binding = %+v, %v", b, ok)
	}
}

func TestInstanceCreateMultipleNICsAndSecondaryDisks(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	ro := f.mkDisk(t, "shared", 10)
	spec := baseSpec()
	spec.NetworkInterfaces = append(spec.NetworkInterfaces, nicSpec("eth1", "subnet-b"))
	spec.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk("data", 30), existingDisk(ro.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_ONLY)}
	inst := f.mustCreate(t, "vm", spec)
	id := inst.GetMetadata().GetId()
	if got := len(inst.GetStatus().GetNetworkInterfaces()); got != 2 {
		t.Fatalf("%d nic statuses, want 2", got)
	}
	if n := inst.GetStatus().GetNetworkInterfaces()[1]; n.GetIndex() != 1 || n.GetIpAddress().GetAddress() != "10.0.0.2" {
		t.Fatalf("second nic = %v", n)
	}
	var names []string
	for _, a := range inst.GetStatus().GetDiskAttachments() {
		names = append(names, fmt.Sprintf("%s/%v", a.GetName(), a.GetIsManaged()))
	}
	if want := []string{"boot/true", "data/true", "shared/false"}; !slices.Equal(names, want) {
		t.Fatalf("attachments = %v, want boot first then spec order %v", names, want)
	}
	got := f.disk(t, ro.GetMetadata().GetId())
	if !slices.Equal(got.GetStatus().GetReadOnlyAttachments(), []string{id}) || got.GetStatus().GetReadWriteAttachment() != "" || got.GetStatus().GetManagedBy() != "" {
		t.Fatalf("existing disk status = %v, want read-only attachment only", got.GetStatus())
	}
	// A second instance can also attach the disk read-only.
	other := baseSpec()
	other.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk(ro.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_ONLY)}
	other.NetworkInterfaces[0].Name = "n"
	other.BootDisk = managedDisk("boot2", 20)
	f.mustCreate(t, "vm2", other)
	if got := f.disk(t, ro.GetMetadata().GetId()); len(got.GetStatus().GetReadOnlyAttachments()) != 2 {
		t.Fatalf("read-only attachments = %v, want 2", got.GetStatus().GetReadOnlyAttachments())
	}
}

func TestInstanceCreateExistingBootDiskReadWrite(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	d := f.mkDisk(t, "bootdisk", 20)
	spec := baseSpec()
	spec.BootDisk = existingDisk(d.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_WRITE)
	inst := f.mustCreate(t, "vm", spec)
	got := f.disk(t, d.GetMetadata().GetId())
	if got.GetStatus().GetReadWriteAttachment() != inst.GetMetadata().GetId() || got.GetStatus().GetManagedBy() != "" {
		t.Fatalf("disk status = %v, want attached read-write and not managed", got.GetStatus())
	}
	if a := inst.GetStatus().GetDiskAttachments(); len(a) != 1 || a[0].GetIsManaged() || a[0].GetName() != "bootdisk" {
		t.Fatalf("attachments = %v", a)
	}
}

func TestInstanceCreatePreemptibleGPU(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.Resources = &computepb.ResourcesSpec{Platform: "gpu-l40s-a", Size: presetSize("1gpu-8vcpu-32gb")}
	spec.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
	spec.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
	inst := f.mustCreate(t, "vm", spec)
	if diff := cmp.Diff(spec, inst.GetSpec(), protocmp.Transform()); diff != "" {
		t.Fatalf("spec not echoed (-want +got):\n%s", diff)
	}
}

func TestInstanceCreateMetadata(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	f.mustCreate(t, "vm", baseSpec())
	tests := []struct {
		name  string
		md    *commonpb.ResourceMetadata
		spec  *computepb.InstanceSpec
		kind  errs.Kind
		field string
	}{
		{"id set", &commonpb.ResourceMetadata{Id: "x", ParentId: testProject, Name: "a"}, baseSpec(), errs.KindBadRequest, "metadata.id"},
		{"no parent", &commonpb.ResourceMetadata{Name: "a"}, baseSpec(), errs.KindBadRequest, "metadata.parent_id"},
		{"no name", &commonpb.ResourceMetadata{ParentId: testProject}, baseSpec(), errs.KindBadRequest, "metadata.name"},
		{"unknown project", &commonpb.ResourceMetadata{ParentId: "project-e00nope", Name: "a"}, baseSpec(), errs.KindNotFound, ""},
		{"no spec", &commonpb.ResourceMetadata{ParentId: testProject, Name: "a"}, nil, errs.KindBadRequest, "spec"},
		{"duplicate name", &commonpb.ResourceMetadata{ParentId: testProject, Name: "vm"}, edited(baseSpec(), func(s *computepb.InstanceSpec) {
			s.BootDisk = managedDisk("other-boot", 20)
		}), errs.KindAlreadyExists, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.svc.Create(t.Context(), CreateInstanceCmd{Metadata: tt.md, Spec: tt.spec})
			wantKind(t, err, tt.kind, tt.field)
		})
	}
}

func TestInstanceCreateValidation(t *testing.T) {
	t.Parallel()
	noSize := func(s *computepb.InstanceSpec) { s.BootDisk.GetManagedDisk().Spec.Size = nil }
	tests := []struct {
		name  string
		edit  func(*computepb.InstanceSpec)
		kind  errs.Kind
		field string
	}{
		{"no platform", func(s *computepb.InstanceSpec) { s.Resources.Platform = "" }, errs.KindBadRequest, "spec.resources.platform"},
		{"no preset", func(s *computepb.InstanceSpec) { s.Resources.Size = nil }, errs.KindBadRequest, "spec.resources.preset"},
		{"unknown platform", func(s *computepb.InstanceSpec) { s.Resources.Platform = "nope" }, errs.KindBadRequest, "spec.resources.platform"},
		{"unknown preset", func(s *computepb.InstanceSpec) { s.Resources.Size = presetSize("nope") }, errs.KindBadRequest, "spec.resources.preset"},
		{"platform not in region", func(s *computepb.InstanceSpec) {
			s.Resources = &computepb.ResourcesSpec{Platform: "gpu-b200-sxm", Size: presetSize("1gpu-20vcpu-224gb")}
		}, errs.KindBadRequest, "spec.resources.platform"},
		{"unknown nvl group", func(s *computepb.InstanceSpec) { s.NvlInstanceGroupId = "computenvlinstancegroup-e00nope" }, errs.KindNotFound, ""},
		{"nvl group with a non-NVL platform", func(s *computepb.InstanceSpec) { s.NvlInstanceGroupId = testNVLGroup }, errs.KindBadRequest, "spec.nvl_instance_group_id"},
		{"spot pricing", func(s *computepb.InstanceSpec) {
			s.PricingModel = &computepb.InstanceSpec_SpotPricingPolicy{SpotPricingPolicy: &computepb.SpotPricingPolicySpec{}}
		}, errs.KindBadRequest, "spec.spot_pricing_policy"},
		{"local disk passthrough", func(s *computepb.InstanceSpec) {
			s.LocalDisks = &computepb.LocalDisksSpec{Request: &computepb.LocalDisksSpec_PassthroughGroup{PassthroughGroup: &computepb.PassthroughGroupRequest{Requested: true}}}
		}, errs.KindBadRequest, "spec.local_disks.passthrough_group"},
		{"recovery ALWAYS", func(s *computepb.InstanceSpec) { s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_ALWAYS }, errs.KindBadRequest, "spec.recovery_policy"},
		{"reservation ids", func(s *computepb.InstanceSpec) {
			s.ReservationPolicy = &computepb.ReservationPolicy{Policy: computepb.ReservationPolicy_AUTO, ReservationIds: []string{"r1"}}
		}, errs.KindBadRequest, "spec.reservation_policy.reservation_ids"},
		{"reservation ids with FORBID", func(s *computepb.InstanceSpec) {
			s.ReservationPolicy = &computepb.ReservationPolicy{Policy: computepb.ReservationPolicy_FORBID, ReservationIds: []string{"r1"}}
		}, errs.KindBadRequest, "spec.reservation_policy.reservation_ids"},
		{"reservation STRICT", func(s *computepb.InstanceSpec) {
			s.ReservationPolicy = &computepb.ReservationPolicy{Policy: computepb.ReservationPolicy_STRICT}
		}, errs.KindBadState, ""},
		{"preemptible on CPU platform", func(s *computepb.InstanceSpec) {
			s.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
			s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
		}, errs.KindBadRequest, "spec.preemptible"},
		{"preemptible without STOP", func(s *computepb.InstanceSpec) {
			s.Resources = &computepb.ResourcesSpec{Platform: "gpu-l40s-a", Size: presetSize("1gpu-8vcpu-32gb")}
			s.Preemptible = &computepb.PreemptibleSpec{}
			s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
		}, errs.KindBadRequest, "spec.preemptible.on_preemption"},
		{"preemptible needs FAIL", func(s *computepb.InstanceSpec) {
			s.Resources = &computepb.ResourcesSpec{Platform: "gpu-l40s-a", Size: presetSize("1gpu-8vcpu-32gb")}
			s.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
		}, errs.KindBadRequest, "spec.recovery_policy"},
		{"gpu cluster on a non-clustering preset", func(s *computepb.InstanceSpec) {
			s.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: "computegpucluster-e00c1"}
		}, errs.KindBadRequest, "spec.gpu_cluster.id"},
		{"unknown gpu cluster", func(s *computepb.InstanceSpec) {
			s.Resources = gpuSpec().GetResources()
			s.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: "computegpucluster-e00nope"}
		}, errs.KindNotFound, ""},
		{"filesystem without id", func(s *computepb.InstanceSpec) {
			s.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec("", "a")}
		}, errs.KindBadRequest, "spec.filesystems[0].existing_filesystem.id"},
		{"filesystem twice", func(s *computepb.InstanceSpec) {
			s.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec("computefilesystem-e00f1", "a"), fsSpec("computefilesystem-e00f1", "b")}
		}, errs.KindBadRequest, "spec.filesystems[1].existing_filesystem.id"},
		{"filesystem without mount_tag", func(s *computepb.InstanceSpec) {
			s.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec("computefilesystem-e00f1", "")}
		}, errs.KindBadRequest, "spec.filesystems[0].mount_tag"},
		{"duplicate mount_tag", func(s *computepb.InstanceSpec) {
			s.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec("computefilesystem-e00f1", "a"), fsSpec("computefilesystem-e00f2", "a")}
		}, errs.KindBadRequest, "spec.filesystems[1].mount_tag"},
		{"unknown filesystem", func(s *computepb.InstanceSpec) {
			s.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec("computefilesystem-e00nope", "a")}
		}, errs.KindNotFound, ""},
		{"service account not an SA id", func(s *computepb.InstanceSpec) { s.ServiceAccountId = "user-1" }, errs.KindBadRequest, "spec.service_account_id"},
		{"service account of another project", func(s *computepb.InstanceSpec) { s.ServiceAccountId = otherSA }, errs.KindBadRequest, "spec.service_account_id"},
		{"unknown service account", func(s *computepb.InstanceSpec) { s.ServiceAccountId = "serviceaccount-e00nope" }, errs.KindNotFound, ""},
		{"no boot disk", func(s *computepb.InstanceSpec) { s.BootDisk = nil }, errs.KindBadRequest, "spec.boot_disk"},
		{"boot disk without attach_mode", func(s *computepb.InstanceSpec) { s.BootDisk.AttachMode = computepb.AttachedDiskSpec_UNSPECIFIED }, errs.KindBadRequest, "spec.boot_disk.attach_mode"},
		{"secondary disk without attach_mode", func(s *computepb.InstanceSpec) {
			d := managedDisk("d", 10)
			d.AttachMode = computepb.AttachedDiskSpec_UNSPECIFIED
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{d}
		}, errs.KindBadRequest, "spec.secondary_disks[0].attach_mode"},
		{"no nic", func(s *computepb.InstanceSpec) { s.NetworkInterfaces = nil }, errs.KindBadRequest, "spec.network_interfaces"},
		{"nine nics", func(s *computepb.InstanceSpec) {
			s.NetworkInterfaces = nil
			for i := range 9 {
				s.NetworkInterfaces = append(s.NetworkInterfaces, nicSpec(fmt.Sprintf("n%d", i), "subnet-a"))
			}
		}, errs.KindBadRequest, "spec.network_interfaces"},
		{"nic without name", func(s *computepb.InstanceSpec) { s.NetworkInterfaces[0].Name = "" }, errs.KindBadRequest, "spec.network_interfaces[0].name"},
		{"nic name twice", func(s *computepb.InstanceSpec) {
			s.NetworkInterfaces = append(s.NetworkInterfaces, nicSpec("eth0", "subnet-b"))
		}, errs.KindBadRequest, "spec.network_interfaces[1].name"},
		{"nic without ip_address", func(s *computepb.InstanceSpec) { s.NetworkInterfaces[0].IpAddress = nil }, errs.KindBadRequest, "spec.network_interfaces[0].ip_address"},
		{"nic without subnet", func(s *computepb.InstanceSpec) { s.NetworkInterfaces[0].SubnetId = "" }, errs.KindBadRequest, "spec.network_interfaces[0].subnet_id"},
		{"nic subnet of another project", func(s *computepb.InstanceSpec) { s.NetworkInterfaces[0].SubnetId = "subnet-other" }, errs.KindBadRequest, "spec.network_interfaces[0].subnet_id"},
		{"nic unknown subnet", func(s *computepb.InstanceSpec) { s.NetworkInterfaces[0].SubnetId = "subnet-nope" }, errs.KindNotFound, ""},
		{"boot disk with neither type", func(s *computepb.InstanceSpec) {
			s.BootDisk = &computepb.AttachedDiskSpec{AttachMode: computepb.AttachedDiskSpec_READ_WRITE}
		}, errs.KindBadRequest, "spec.boot_disk"},
		{"existing disk without id", func(s *computepb.InstanceSpec) {
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk("", computepb.AttachedDiskSpec_READ_WRITE)}
		}, errs.KindBadRequest, "spec.secondary_disks[0].existing_disk.id"},
		{"managed disk without name", func(s *computepb.InstanceSpec) { s.BootDisk.GetManagedDisk().Name = "" }, errs.KindBadRequest, "spec.boot_disk.managed_disk.name"},
		{"managed disk name twice", func(s *computepb.InstanceSpec) {
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk("boot", 10)}
		}, errs.KindBadRequest, "spec.secondary_disks[0].managed_disk.name"},
		{"managed disk without spec", func(s *computepb.InstanceSpec) { s.BootDisk.GetManagedDisk().Spec = nil }, errs.KindBadRequest, "spec.boot_disk.managed_disk.spec"},
		{"managed disk without size", noSize, errs.KindBadRequest, "spec.boot_disk.managed_disk.spec.size"},
		{"device_id twice", func(s *computepb.InstanceSpec) {
			s.BootDisk.DeviceId = "dev"
			d := managedDisk("d", 10)
			d.DeviceId = "dev"
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{d}
		}, errs.KindBadRequest, "spec.secondary_disks[0].device_id"},
		{"unknown existing disk", func(s *computepb.InstanceSpec) {
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk("computedisk-e00nope", computepb.AttachedDiskSpec_READ_WRITE)}
		}, errs.KindNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInstFixture(t)
			_, err := f.create("vm", edited(baseSpec(), tt.edit))
			wantKind(t, err, tt.kind, tt.field)
			if n := f.repo.count(); n != 0 {
				t.Errorf("%d instances stored after a rejected create", n)
			}
			if b := f.net.boundNICs(); len(b) != 0 {
				t.Errorf("NICs still bound after a rejected create: %v", b)
			}
			if n := f.diskCount(); n != 0 {
				t.Errorf("%d disks left after a rejected create", n)
			}
			for _, c := range []string{"computegpucluster-e00c1", "computegpucluster-e00nope"} {
				if m := f.gpu.membersOf(c); len(m) != 0 {
					t.Errorf("GPU cluster %s still has members %v", c, m)
				}
			}
			for _, fs := range []string{"computefilesystem-e00f1", "computefilesystem-e00f2"} {
				if a := f.fs.attachedTo(fs); len(a) != 0 {
					t.Errorf("filesystem %s still attached to %v", fs, a)
				}
			}
		})
	}
}

func TestInstanceCreateExistingDiskRules(t *testing.T) {
	t.Parallel()
	t.Run("other project", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		op, err := f.ds.Create(t.Context(), CreateDiskCmd{Metadata: &commonpb.ResourceMetadata{ParentId: otherProject, Name: "far"}, Spec: ssdSpec(10)})
		if err != nil {
			t.Fatal(err)
		}
		spec := edited(baseSpec(), func(s *computepb.InstanceSpec) {
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk(op.ResourceID, computepb.AttachedDiskSpec_READ_WRITE)}
		})
		_, err = f.create("vm", spec)
		wantKind(t, err, errs.KindBadRequest, "spec.secondary_disks[0].existing_disk.id")
	})
	t.Run("not READY", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		f.ops.deferred = true // the disk stays CREATING
		op, err := f.ds.Create(t.Context(), CreateDiskCmd{Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "slow"}, Spec: ssdSpec(10)})
		if err != nil {
			t.Fatal(err)
		}
		f.ops.deferred = false
		_, err = f.create("vm", edited(baseSpec(), func(s *computepb.InstanceSpec) {
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk(op.ResourceID, computepb.AttachedDiskSpec_READ_WRITE)}
		}))
		wantKind(t, err, errs.KindBadState, "")
	})
	t.Run("read-write twice", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		d := f.mkDisk(t, "d", 10)
		with := func(name string, mode computepb.AttachedDiskSpec_AttachMode) error {
			spec := baseSpec()
			spec.BootDisk = managedDisk(name+"-boot", 20)
			spec.NetworkInterfaces[0].Name = name
			spec.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk(d.GetMetadata().GetId(), mode)}
			_, err := f.create(name, spec)
			return err
		}
		if err := with("a", computepb.AttachedDiskSpec_READ_WRITE); err != nil {
			t.Fatal(err)
		}
		wantKind(t, with("b", computepb.AttachedDiskSpec_READ_WRITE), errs.KindBadState, "")
		wantKind(t, with("c", computepb.AttachedDiskSpec_READ_ONLY), errs.KindBadState, "")
	})
	t.Run("read-only blocks read-write", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		d := f.mkDisk(t, "d", 10)
		spec := baseSpec()
		spec.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk(d.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_ONLY)}
		f.mustCreate(t, "a", spec)
		spec2 := edited(spec, func(s *computepb.InstanceSpec) {
			s.BootDisk = managedDisk("boot2", 20)
			s.NetworkInterfaces[0].Name = "n2"
			s.SecondaryDisks[0].AttachMode = computepb.AttachedDiskSpec_READ_WRITE
		})
		_, err := f.create("b", spec2)
		wantKind(t, err, errs.KindBadState, "")
	})
	t.Run("same disk twice", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		d := f.mkDisk(t, "d", 10)
		_, err := f.create("vm", edited(baseSpec(), func(s *computepb.InstanceSpec) {
			s.SecondaryDisks = []*computepb.AttachedDiskSpec{
				existingDisk(d.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_ONLY),
				existingDisk(d.GetMetadata().GetId(), computepb.AttachedDiskSpec_READ_ONLY),
			}
		}))
		wantKind(t, err, errs.KindBadRequest, "spec.secondary_disks[1]")
	})
	t.Run("managed by another instance", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		a := f.mustCreate(t, "a", baseSpec())
		bootID := a.GetStatus().GetDiskAttachments()[0].GetId()
		spec := baseSpec()
		spec.BootDisk = managedDisk("b-boot", 20)
		spec.NetworkInterfaces[0].Name = "n2"
		spec.SecondaryDisks = []*computepb.AttachedDiskSpec{existingDisk(bootID, computepb.AttachedDiskSpec_READ_ONLY)}
		_, err := f.create("b", spec)
		wantKind(t, err, errs.KindBadState, "")
	})
}

func TestInstanceCreateNoVPC(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	f.svc.d.Network = noNetwork{}
	_, err := f.create("vm", baseSpec())
	wantKind(t, err, errs.KindBadRequest, "spec.network_interfaces")
}

// ---- create rollback ----

func TestInstanceCreateRollsBackOnNICBindFailure(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	f.net.failNIC["eth1"] = errs.BadState("vpcallocation-x", "allocation is assigned to another NIC")
	spec := gpuSpec()
	spec.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: "computegpucluster-e00c1"}
	spec.NetworkInterfaces = append(spec.NetworkInterfaces, nicSpec("eth1", "subnet-b"))
	_, err := f.create("vm", spec)
	wantKind(t, err, errs.KindBadState, "")
	if b := f.net.boundNICs(); len(b) != 0 {
		t.Errorf("eth0 was left bound: %v", b)
	}
	if f.repo.count() != 0 || f.diskCount() != 0 {
		t.Errorf("left %d instances and %d disks", f.repo.count(), f.diskCount())
	}
	if len(f.gpu.joins) != 0 {
		t.Errorf("the GPU cluster was joined despite the failed bind: %v", f.gpu.joins)
	}
	// The failure names the NIC.
	spec.NetworkInterfaces[1].Name = "eth1"
	if e, ok := errors.AsType[*errs.Error](err); ok && len(e.Violations) > 0 && !strings.HasPrefix(e.Violations[0].Field, "spec.network_interfaces[1].") {
		t.Errorf("violation field = %q, want prefix spec.network_interfaces[1].", e.Violations[0].Field)
	}
	// And the same name can be used once the failure is gone.
	delete(f.net.failNIC, "eth1")
	f.mustCreate(t, "vm", spec)
}

func TestInstanceCreateRollsBackOnManagedDiskNameConflict(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	f.mkDisk(t, "data", 10) // a standalone disk already has the managed disk's name
	spec := gpuSpec()
	spec.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: "computegpucluster-e00c1"}
	spec.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec("computefilesystem-e00f1", "shared")}
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{}
	spec.SecondaryDisks = []*computepb.AttachedDiskSpec{managedDisk("data", 10)}
	_, err := f.create("vm", spec)
	wantKind(t, err, errs.KindAlreadyExists, "")
	if b := f.net.boundNICs(); len(b) != 0 {
		t.Errorf("NICs left bound: %v", b)
	}
	if f.repo.count() != 0 {
		t.Errorf("an instance was stored")
	}
	if got := f.diskCount(); got != 1 {
		t.Errorf("%d disks, want only the pre-existing standalone disk (the managed boot disk must be rolled back)", got)
	}
	if got := f.fs.attachedTo("computefilesystem-e00f1"); len(got) != 0 {
		t.Errorf("filesystem still attached to %v", got)
	}
	if got := f.gpu.membersOf("computegpucluster-e00c1"); len(got) != 0 {
		t.Errorf("GPU cluster still has members %v", got)
	}
	if len(f.gpu.joins) != 1 || len(f.gpu.leaves) != 1 {
		t.Errorf("joins %v, leaves %v, want one of each", f.gpu.joins, f.gpu.leaves)
	}
}

func TestInstanceCreateRollsBackOnFilesystemFailure(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec("computefilesystem-e00f1", "a"), fsSpec("computefilesystem-e00nope", "b")}
	_, err := f.create("vm", spec)
	wantKind(t, err, errs.KindNotFound, "")
	if got := f.fs.attachedTo("computefilesystem-e00f1"); len(got) != 0 {
		t.Errorf("first filesystem still attached to %v", got)
	}
	if len(f.net.boundNICs()) != 0 || f.repo.count() != 0 {
		t.Errorf("bound %v, instances %d", f.net.boundNICs(), f.repo.count())
	}
}

// ---- GPU clusters and filesystems ----

func TestInstanceGpuClusterJoinAndLeave(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := gpuSpec()
	spec.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: "computegpucluster-e00c1"}
	inst := f.mustCreate(t, "vm", spec)
	id := inst.GetMetadata().GetId()
	if want := []string{"computegpucluster-e00c1/" + id + "/gpu-h100-sxm"}; !slices.Equal(f.gpu.joins, want) {
		t.Fatalf("joins = %v, want %v", f.gpu.joins, want)
	}
	path := inst.GetStatus().GetInfinibandTopologyPath().GetPath()
	if !slices.Equal(path, []string{"spine-1", "leaf-computegpucluster-e00c1"}) {
		t.Fatalf("topology path = %v", path)
	}
	if err := f.remove(id); err != nil {
		t.Fatal(err)
	}
	if got := f.gpu.membersOf("computegpucluster-e00c1"); len(got) != 0 {
		t.Fatalf("members after delete = %v", got)
	}
	if want := []string{"computegpucluster-e00c1/" + id}; !slices.Equal(f.gpu.leaves, want) {
		t.Fatalf("leaves = %v, want %v", f.gpu.leaves, want)
	}
}

func TestInstanceWithoutGpuClusterNeverJoins(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", gpuSpec())
	if len(f.gpu.joins) != 0 || inst.GetStatus().GetInfinibandTopologyPath() != nil {
		t.Fatalf("joins %v, topology %v: an instance without spec.gpu_cluster must not join", f.gpu.joins, inst.GetStatus().GetGpuClusterTopology())
	}
	if err := f.remove(inst.GetMetadata().GetId()); err != nil || len(f.gpu.leaves) != 0 {
		t.Fatalf("delete: %v, leaves %v", err, f.gpu.leaves)
	}
}

func TestInstanceFilesystemAttachOnCreateAndDelete(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec("computefilesystem-e00f1", "a"), fsSpec("computefilesystem-e00f2", "b")}
	inst := f.mustCreate(t, "vm", spec)
	id := inst.GetMetadata().GetId()
	for _, fs := range []string{"computefilesystem-e00f1", "computefilesystem-e00f2"} {
		if got := f.fs.attachedTo(fs); !slices.Equal(got, []string{id}) {
			t.Fatalf("%s attached to %v, want [%s]", fs, got, id)
		}
	}
	if err := f.remove(id); err != nil {
		t.Fatal(err)
	}
	for _, fs := range []string{"computefilesystem-e00f1", "computefilesystem-e00f2"} {
		if got := f.fs.attachedTo(fs); len(got) != 0 {
			t.Fatalf("%s still attached to %v after delete", fs, got)
		}
	}
}
