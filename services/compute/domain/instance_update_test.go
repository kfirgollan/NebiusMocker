package domain

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func TestInstanceUpdateLabelsOnly(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	f.clk.Advance(time.Minute)
	if err := f.update(t, id, nil, map[string]string{"env": "dev"}); err != nil {
		t.Fatal(err)
	}
	got := f.get(t, id)
	if got.GetMetadata().GetResourceVersion() != 2 || got.GetMetadata().GetLabels()["env"] != "dev" {
		t.Fatalf("metadata = %v, want resource_version 2 and the label", got.GetMetadata())
	}
	if instState(got) != computepb.InstanceStatus_RUNNING {
		t.Fatalf("state = %v, want RUNNING: a labels-only update has no state change", instState(got))
	}
	if !proto.Equal(got.GetSpec(), inst.GetSpec()) {
		t.Fatalf("spec changed: %v", got.GetSpec())
	}
	if !got.GetMetadata().GetUpdatedAt().AsTime().After(inst.GetMetadata().GetUpdatedAt().AsTime()) {
		t.Fatalf("updated_at did not advance: %v -> %v", inst.GetMetadata().GetUpdatedAt(), got.GetMetadata().GetUpdatedAt())
	}
	if got.GetMetadata().GetCreatedAt().AsTime() != inst.GetMetadata().GetCreatedAt().AsTime() {
		t.Fatalf("created_at changed")
	}
}

func TestInstanceUpdateUnchangedKeepsVersion(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	f.clk.Advance(time.Minute)
	if err := f.update(t, id, func(*computepb.InstanceSpec) {}, nil); err != nil {
		t.Fatal(err)
	}
	got := f.get(t, id)
	if !proto.Equal(got, inst) {
		t.Fatalf("an unchanged update modified the instance:\n got  %v\n want %v", got, inst)
	}
}

func TestInstanceUpdateStoppedToggleTransitions(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	f.ops.deferred = true

	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Stopped = true }, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_STOPPING || !got.GetStatus().GetReconciling() {
		t.Fatalf("after stopped=true: %v, want STOPPING and reconciling", got.GetStatus())
	}
	f.ops.finish()
	got := f.get(t, id)
	if instState(got) != computepb.InstanceStatus_STOPPED || got.GetStatus().GetReconciling() || got.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("after settle: state %v rv %d, want STOPPED rv 2", instState(got), got.GetMetadata().GetResourceVersion())
	}

	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Stopped = false }, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_STARTING {
		t.Fatalf("after stopped=false: %v, want STARTING", instState(got))
	}
	f.ops.finish()
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_RUNNING || got.GetMetadata().GetResourceVersion() != 3 {
		t.Fatalf("after settle: state %v rv %d, want RUNNING rv 3", instState(got), got.GetMetadata().GetResourceVersion())
	}
}

func TestInstanceUpdateOtherChangeRunsUpdating(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.Stopped = true
	inst := f.mustCreate(t, "vm", spec)
	id := inst.GetMetadata().GetId()
	f.ops.deferred = true
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.CloudInitUserData = "#cloud-config" }, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_UPDATING {
		t.Fatalf("state = %v, want UPDATING", instState(got))
	}
	// An update during UPDATING is refused.
	err := f.update(t, id, func(s *computepb.InstanceSpec) { s.CloudInitUserData = "x" }, nil)
	wantKind(t, err, errs.KindBadState, "")
	f.ops.finish()
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_STOPPED {
		t.Fatalf("state after settle = %v, want back to STOPPED", instState(got))
	}
}

func TestInstanceUpdateHostnameChangesFQDN(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Hostname = "api" }, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, id).GetStatus().GetNetworkInterfaces()[0].GetFqdn(); got != "api.network-1.compute.internal." {
		t.Fatalf("fqdn = %q", got)
	}
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Hostname = "" }, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, id).GetStatus().GetNetworkInterfaces()[0].GetFqdn(); got != id+".network-1.compute.internal." {
		t.Fatalf("fqdn = %q, want the instance id again", got)
	}
}

func TestInstanceUpdatePresetChange(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	bigger := func(s *computepb.InstanceSpec) { s.Resources.Size = presetSize("4vcpu-16gb") }
	wantKind(t, f.update(t, id, bigger, nil), errs.KindBadState, "")
	if got := f.get(t, id).GetSpec().GetResources().GetPreset(); got != "2vcpu-8gb" {
		t.Fatalf("preset = %q after a refused change", got)
	}
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	if err := f.update(t, id, bigger, nil); err != nil {
		t.Fatal(err)
	}
	got := f.get(t, id)
	if got.GetSpec().GetResources().GetPreset() != "4vcpu-16gb" || instState(got) != computepb.InstanceStatus_STOPPED {
		t.Fatalf("preset %q state %v, want 4vcpu-16gb and STOPPED", got.GetSpec().GetResources().GetPreset(), instState(got))
	}
	wantKind(t, f.update(t, id, func(s *computepb.InstanceSpec) { s.Resources.Size = presetSize("nope") }, nil), errs.KindBadRequest, "spec.resources.preset")
}

func TestInstanceUpdateImmutableFields(t *testing.T) {
	t.Parallel()
	withSA := func() *computepb.InstanceSpec { s := baseSpec(); s.ServiceAccountId = testSA; return s }
	withGPUCluster := func() *computepb.InstanceSpec {
		s := gpuSpec()
		s.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: "computegpucluster-e00c1"}
		return s
	}
	preemptible := func() *computepb.InstanceSpec {
		s := baseSpec()
		s.Resources = &computepb.ResourcesSpec{Platform: "gpu-l40s-a", Size: presetSize("1gpu-8vcpu-32gb")}
		s.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
		s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
		return s
	}
	gpuL40 := func() *computepb.InstanceSpec {
		s := baseSpec()
		s.Resources = &computepb.ResourcesSpec{Platform: "gpu-l40s-a", Size: presetSize("1gpu-8vcpu-32gb")}
		return s
	}
	tests := []struct {
		name  string
		base  func() *computepb.InstanceSpec
		edit  func(*computepb.InstanceSpec)
		field string
	}{
		{"service_account_id removed", withSA, func(s *computepb.InstanceSpec) { s.ServiceAccountId = "" }, "spec.service_account_id"},
		{"service_account_id added", baseSpec, func(s *computepb.InstanceSpec) { s.ServiceAccountId = testSA }, "spec.service_account_id"},
		{"platform", baseSpec, func(s *computepb.InstanceSpec) { s.Resources.Platform = "cpu-d3" }, "spec.resources.platform"},
		{"gpu_cluster removed", withGPUCluster, func(s *computepb.InstanceSpec) { s.GpuCluster = nil }, "spec.gpu_cluster"},
		{"gpu_cluster added", gpuSpec, func(s *computepb.InstanceSpec) {
			s.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: "computegpucluster-e00c1"}
		}, "spec.gpu_cluster"},
		{"recovery_policy", baseSpec, func(s *computepb.InstanceSpec) { s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL }, "spec.recovery_policy"},
		{"preemptible removed", preemptible, func(s *computepb.InstanceSpec) { s.Preemptible = nil }, "spec.preemptible"},
		{"preemptible added", gpuL40, func(s *computepb.InstanceSpec) {
			s.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
			s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
		}, "spec.preemptible"},
		{"nic subnet_id", baseSpec, func(s *computepb.InstanceSpec) { s.NetworkInterfaces[0].SubnetId = "subnet-b" }, "spec.network_interfaces[0].subnet_id"},
		{"nic ip_address", baseSpec, func(s *computepb.InstanceSpec) {
			s.NetworkInterfaces[0].IpAddress = &computepb.IPAddress{AllocationId: "vpcallocation-x"}
		}, "spec.network_interfaces[0].ip_address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInstFixture(t)
			inst := f.mustCreate(t, "vm", tt.base())
			id := inst.GetMetadata().GetId()
			before := f.net.boundNICs()
			wantKind(t, f.update(t, id, tt.edit, nil), errs.KindBadRequest, tt.field)
			if got := f.get(t, id); !proto.Equal(got, inst) {
				t.Errorf("a refused update changed the instance:\n got  %v\n want %v", got, inst)
			}
			if got := f.net.boundNICs(); !slices.Equal(got, before) {
				t.Errorf("bound NICs = %v, want %v", got, before)
			}
		})
	}
}

func TestInstanceUpdateRequestChecks(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	tests := []struct {
		name  string
		md    *commonpb.ResourceMetadata
		kind  errs.Kind
		field string
	}{
		{"no id", &commonpb.ResourceMetadata{}, errs.KindBadRequest, "metadata.id"},
		{"unknown id", &commonpb.ResourceMetadata{Id: "computeinstance-e00nope"}, errs.KindNotFound, ""},
		{"parent change", &commonpb.ResourceMetadata{Id: id, ParentId: otherProject}, errs.KindBadRequest, "metadata.parent_id"},
		{"stale version", &commonpb.ResourceMetadata{Id: id, ResourceVersion: 7}, errs.KindConflict, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.svc.Update(t.Context(), UpdateInstanceCmd{Metadata: tt.md})
			wantKind(t, err, tt.kind, tt.field)
		})
	}
}

func TestInstanceUpdateNICAddAndRemove(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	if err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.NetworkInterfaces = append(s.NetworkInterfaces, nicSpec("eth1", "subnet-b"))
	}, nil); err != nil {
		t.Fatal(err)
	}
	got := f.get(t, id)
	if len(got.GetStatus().GetNetworkInterfaces()) != 2 || got.GetStatus().GetNetworkInterfaces()[1].GetName() != "eth1" {
		t.Fatalf("nic statuses = %v", got.GetStatus().GetNetworkInterfaces())
	}
	if want := []string{id + "/eth0", id + "/eth1"}; !slices.Equal(f.net.boundNICs(), want) {
		t.Fatalf("bound = %v, want %v", f.net.boundNICs(), want)
	}
	// eth0 keeps its address across the update.
	if a := got.GetStatus().GetNetworkInterfaces()[0].GetIpAddress().GetAddress(); a != "10.0.0.1" {
		t.Fatalf("eth0 address = %s, want it kept", a)
	}
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.NetworkInterfaces = s.GetNetworkInterfaces()[:1] }, nil); err != nil {
		t.Fatal(err)
	}
	got = f.get(t, id)
	if len(got.GetStatus().GetNetworkInterfaces()) != 1 {
		t.Fatalf("nic statuses = %v", got.GetStatus().GetNetworkInterfaces())
	}
	if want := []string{id + "/eth0"}; !slices.Equal(f.net.boundNICs(), want) {
		t.Fatalf("bound = %v, want only %v (eth1 released)", f.net.boundNICs(), want)
	}
	if !slices.Contains(f.net.freed, id+"/eth1") {
		t.Fatalf("released = %v, want eth1", f.net.freed)
	}
}

func TestInstanceUpdateRollsBackWhenNICBindFails(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	f.net.failNIC["eth1"] = errs.BadState("vpcallocation-x", "in use")
	err := f.update(t, id, func(s *computepb.InstanceSpec) {
		s.Hostname = "changed"
		s.NetworkInterfaces = append(s.NetworkInterfaces, nicSpec("eth1", "subnet-b"))
	}, nil)
	wantKind(t, err, errs.KindBadState, "")
	if got := f.get(t, id); !proto.Equal(got, inst) {
		t.Fatalf("a failed update changed the instance: %v", got)
	}
	if want := []string{id + "/eth0"}; !slices.Equal(f.net.boundNICs(), want) {
		t.Fatalf("bound = %v, want %v", f.net.boundNICs(), want)
	}
}

func TestInstanceUpdateFilesystems(t *testing.T) {
	t.Parallel()
	const fs1, fs2 = "computefilesystem-e00f1", "computefilesystem-e00f2"
	add := func(s *computepb.InstanceSpec) {
		s.Filesystems = append(s.Filesystems, fsSpec(fs1, "data"))
	}
	t.Run("add while RUNNING is refused", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		id := f.mustCreate(t, "vm", baseSpec()).GetMetadata().GetId()
		wantKind(t, f.update(t, id, add, nil), errs.KindBadState, "")
		if got := f.fs.attachedTo(fs1); len(got) != 0 {
			t.Fatalf("filesystem attached to %v by a refused update", got)
		}
		if len(f.get(t, id).GetSpec().GetFilesystems()) != 0 {
			t.Fatal("a refused update changed the spec")
		}
	})
	t.Run("add while STOPPED, then remove", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		spec := baseSpec()
		spec.Stopped = true
		id := f.mustCreate(t, "vm", spec).GetMetadata().GetId()
		if err := f.update(t, id, add, nil); err != nil {
			t.Fatal(err)
		}
		if got := f.fs.attachedTo(fs1); !slices.Equal(got, []string{id}) {
			t.Fatalf("attached to %v, want [%s]", got, id)
		}
		if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Filesystems = nil }, nil); err != nil {
			t.Fatal(err)
		}
		if got := f.fs.attachedTo(fs1); len(got) != 0 {
			t.Fatalf("still attached to %v after removal", got)
		}
	})
	t.Run("remove while RUNNING", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		spec := baseSpec()
		spec.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec(fs1, "a"), fsSpec(fs2, "b")}
		id := f.mustCreate(t, "vm", spec).GetMetadata().GetId()
		if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Filesystems = s.GetFilesystems()[:1] }, nil); err != nil {
			t.Fatal(err)
		}
		if f.fs.attachedTo(fs2) != nil && len(f.fs.attachedTo(fs2)) != 0 || len(f.fs.attachedTo(fs1)) != 1 {
			t.Fatalf("attachments fs1 %v fs2 %v, want only fs1", f.fs.attachedTo(fs1), f.fs.attachedTo(fs2))
		}
	})
	t.Run("duplicate mount_tag", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		spec := baseSpec()
		spec.Stopped = true
		id := f.mustCreate(t, "vm", spec).GetMetadata().GetId()
		err := f.update(t, id, func(s *computepb.InstanceSpec) {
			s.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec(fs1, "same"), fsSpec(fs2, "same")}
		}, nil)
		wantKind(t, err, errs.KindBadRequest, "spec.filesystems[1].mount_tag")
	})
	t.Run("unknown filesystem rolls the update back", func(t *testing.T) {
		t.Parallel()
		f := newInstFixture(t)
		spec := baseSpec()
		spec.Stopped = true
		inst := f.mustCreate(t, "vm", spec)
		id := inst.GetMetadata().GetId()
		err := f.update(t, id, func(s *computepb.InstanceSpec) {
			s.Filesystems = []*computepb.AttachedFilesystemSpec{fsSpec(fs1, "a"), fsSpec("computefilesystem-e00nope", "b")}
		}, nil)
		wantKind(t, err, errs.KindNotFound, "")
		if got := f.fs.attachedTo(fs1); len(got) != 0 {
			t.Fatalf("fs1 attached to %v after the failed update", got)
		}
		if got := f.get(t, id); !proto.Equal(got, inst) {
			t.Fatalf("a failed update changed the instance: %v", got)
		}
	})
}

// ---- Start / Stop ----

func TestInstanceStopStartTransitions(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.Stopped = false
	id := f.mustCreate(t, "vm", spec).GetMetadata().GetId()
	f.ops.deferred = true
	if _, err := f.svc.Stop(f.ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_STOPPING || !got.GetStatus().GetReconciling() {
		t.Fatalf("state = %v, want STOPPING", got.GetStatus())
	}
	f.ops.finish()
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_STOPPED || got.GetSpec().GetStopped() {
		t.Fatalf("state %v stopped=%v, want STOPPED and spec.stopped untouched", instState(got), got.GetSpec().GetStopped())
	}
	if _, err := f.svc.Start(f.ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_STARTING {
		t.Fatalf("state = %v, want STARTING", instState(got))
	}
	f.ops.finish()
	if got := f.get(t, id); instState(got) != computepb.InstanceStatus_RUNNING {
		t.Fatalf("state = %v, want RUNNING", instState(got))
	}
}

func TestInstanceStartStopWrongState(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id := f.mustCreate(t, "vm", baseSpec()).GetMetadata().GetId()
	wantKind(t, f.start(id), errs.KindBadState, "")
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	wantKind(t, f.stop(id), errs.KindBadState, "")
	if err := f.start(id); err != nil {
		t.Fatal(err)
	}
	wantKind(t, f.start("computeinstance-e00nope"), errs.KindNotFound, "")
	_, err := f.svc.Stop(f.ctx, "", nil)
	wantKind(t, err, errs.KindBadRequest, "id")
	// An instance in a transitional state cannot be started or stopped.
	f.ops.deferred = true
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	wantKind(t, f.stop(id), errs.KindBadState, "")
	wantKind(t, f.start(id), errs.KindBadState, "")
}

func TestInstanceStopReleasesDynamicPublicIPAndStartRebinds(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{}
	inst := f.mustCreate(t, "vm", spec)
	id := inst.GetMetadata().GetId()
	first := inst.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
	if first.GetAddress() == "" || first.GetStatic() {
		t.Fatalf("public ip = %v", first)
	}
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	stopped := f.get(t, id).GetStatus().GetNetworkInterfaces()[0]
	if stopped.GetPublicIpAddress() != nil {
		t.Fatalf("a stopped instance still has public address %v", stopped.GetPublicIpAddress())
	}
	if b, ok := f.net.binding(id, "eth0"); !ok || b.Public != nil {
		t.Fatalf("binding while stopped = %+v (bound %v), want no public binding", b, ok)
	}
	if stopped.GetIpAddress().GetAddress() != "10.0.0.1" {
		t.Fatalf("private address = %s, want it kept", stopped.GetIpAddress().GetAddress())
	}
	if err := f.start(id); err != nil {
		t.Fatal(err)
	}
	again := f.get(t, id).GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
	if again.GetAddress() == "" || again.GetStatic() {
		t.Fatalf("public ip after start = %v, want a new dynamic address", again)
	}
}

func TestInstanceStopKeepsStaticAndExplicitPublicIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pub  *computepb.PublicIPAddress
	}{
		{"static", &computepb.PublicIPAddress{Static: true}},
		{"explicit allocation", &computepb.PublicIPAddress{Allocation: &computepb.PublicIPAddress_AllocationId{AllocationId: "vpcallocation-pub"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInstFixture(t)
			spec := baseSpec()
			spec.NetworkInterfaces[0].PublicIpAddress = tt.pub
			inst := f.mustCreate(t, "vm", spec)
			id := inst.GetMetadata().GetId()
			before := inst.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
			if before.GetAddress() == "" {
				t.Fatalf("no public address at create: %v", inst.GetStatus())
			}
			if err := f.stop(id); err != nil {
				t.Fatal(err)
			}
			if got := f.get(t, id).GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress(); got.GetAddress() != before.GetAddress() {
				t.Fatalf("public ip while stopped = %v, want %v kept", got, before)
			}
			if err := f.start(id); err != nil {
				t.Fatal(err)
			}
			if got := f.get(t, id).GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress(); got.GetAddress() != before.GetAddress() {
				t.Fatalf("public ip after start = %v, want %v", got, before)
			}
		})
	}
}
