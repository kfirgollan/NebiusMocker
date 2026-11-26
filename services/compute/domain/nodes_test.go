package domain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// fakeNodes is an in-memory NodeRepository.
type fakeNodes struct {
	mu    sync.Mutex
	order []NodeRecord
}

var _ NodeRepository = (*fakeNodes)(nil)

func (f *fakeNodes) find(match func(NodeRecord) bool) (NodeRecord, bool) {
	i := slices.IndexFunc(f.order, match)
	if i < 0 {
		return NodeRecord{}, false
	}
	return f.order[i], true
}

func (f *fakeNodes) Owner(_ context.Context, id string) (NodeRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.find(func(r NodeRecord) bool { return r.InstanceID == id })
	return rec, ok, nil
}

func (f *fakeNodes) ByName(_ context.Context, group, name string) (NodeRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.find(func(r NodeRecord) bool { return r.NodeGroupID == group && r.Name == name })
	return rec, ok, nil
}

func (f *fakeNodes) List(_ context.Context, group string) ([]NodeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []NodeRecord
	for _, r := range f.order {
		if r.NodeGroupID == group {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeNodes) Put(_ context.Context, n NodeRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.find(func(r NodeRecord) bool { return r.NodeGroupID == n.NodeGroupID && r.Name == n.Name }); dup {
		return errs.AlreadyExists(n.InstanceID, "node name taken")
	}
	f.order = append(f.order, n)
	return nil
}

func (f *fakeNodes) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = slices.DeleteFunc(f.order, func(r NodeRecord) bool { return r.InstanceID == id })
	return nil
}

const (
	nodeFamily = "ubuntu24.04-driverless"
	nodeGroup  = "mk8snodegroup-e00ng1"
	nodeBoot   = int64(64) << 30
)

func nodeFixture(t *testing.T) *instFixture {
	t.Helper()
	f := newInstFixture(t)
	f.images.add(&computepb.Image{
		Metadata: &commonpb.ResourceMetadata{Id: "computeimage-node", ParentId: PublicProjectID("e00"), Name: "node"},
		Spec:     &computepb.ImageSpec{ImageFamily: nodeFamily},
		Status:   &computepb.ImageStatus{State: computepb.ImageStatus_READY},
	})
	return f
}

func cpuTemplate() peer.NodeTemplate {
	return peer.NodeTemplate{
		Platform: "cpu-e2", Preset: "2vcpu-8gb", ImageFamily: nodeFamily, SubnetID: "subnet-a",
		BootDisk: peer.NodeDisk{SizeBytes: nodeBoot, Type: "NETWORK_SSD"},
	}
}

func nodeSpecFor(name string, t peer.NodeTemplate) peer.NodeSpec {
	return peer.NodeSpec{
		ProjectID: testProject, Name: name, ClusterID: "mk8scluster-e00c1", NodeGroupID: nodeGroup,
		Labels: map[string]string{"mk8s-node-group": nodeGroup}, Template: t,
	}
}

func TestCreateNodeIsARunningInstance(t *testing.T) {
	f := nodeFixture(t)
	tpl := cpuTemplate()
	tpl.PublicIP = true
	tpl.ServiceAccountID = testSA
	tpl.CloudInitUserData = "#cloud-config"
	tpl.SecurityGroupIDs = []string{"securitygroup-a"}
	tpl.BootDisk.Encrypted = false

	n, err := f.svc.CreateNode(f.ctx, nodeSpecFor("node-0", tpl))
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if n.Name != "node-0" || n.PrivateIP == "" || n.PublicIP == "" || strings.Contains(n.PrivateIP, "/") {
		t.Fatalf("node = %+v, want name, bare private and public IPs", n)
	}
	if want := (peer.NodeResources{VCPUs: 2, Memory: 8}); n.Resources != want {
		t.Fatalf("resources = %+v, want %+v", n.Resources, want)
	}

	inst := f.get(t, n.ID)
	if instState(inst) != computepb.InstanceStatus_RUNNING || inst.GetStatus().GetReconciling() {
		t.Fatalf("state = %v, want RUNNING", instState(inst))
	}
	if inst.GetMetadata().GetParentId() != testProject || inst.GetMetadata().GetLabels()["mk8s-node-group"] != nodeGroup {
		t.Fatalf("metadata = %+v", inst.GetMetadata())
	}
	spec := inst.GetSpec()
	if spec.GetResources().GetPlatform() != "cpu-e2" || spec.GetResources().GetPreset() != "2vcpu-8gb" ||
		spec.GetServiceAccountId() != testSA || spec.GetCloudInitUserData() != "#cloud-config" {
		t.Fatalf("spec = %v", spec)
	}
	nic := spec.GetNetworkInterfaces()[0]
	if nic.GetName() != "eth0" || nic.GetSubnetId() != "subnet-a" || nic.GetPublicIpAddress() == nil || nic.GetSecurityGroups()[0].GetId() != "securitygroup-a" {
		t.Fatalf("nic = %v", nic)
	}
	md := spec.GetBootDisk().GetManagedDisk()
	if md.GetName() != "node-0-boot" || md.GetSpec().GetSizeBytes() != nodeBoot ||
		md.GetSpec().GetSourceImageFamily().GetImageFamily() != nodeFamily {
		t.Fatalf("boot disk = %v", spec.GetBootDisk())
	}
	d := f.disk(t, inst.GetStatus().GetDiskAttachments()[0].GetId())
	if d.GetStatus().GetManagedBy() != n.ID || d.GetStatus().GetState() != computepb.DiskStatus_READY {
		t.Fatalf("boot disk status = %v", d.GetStatus())
	}
	if got := f.net.boundNICs(); !slices.Equal(got, []string{n.ID + "/eth0"}) {
		t.Fatalf("bound NICs = %v", got)
	}
}

func TestCreateNodeGPUClusterFilesystemPreemptible(t *testing.T) {
	f := nodeFixture(t)
	tpl := cpuTemplate()
	tpl.Platform, tpl.Preset = "gpu-h100-sxm", "8gpu-128vcpu-1600gb"
	tpl.GpuClusterID = "computegpucluster-e00c1"
	tpl.Preemptible = true
	tpl.Filesystems = []peer.NodeFilesystem{{ID: "computefilesystem-e00f1", MountTag: "data"}}

	n, err := f.svc.CreateNode(f.ctx, nodeSpecFor("gpu-0", tpl))
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if n.Resources.GPUs != 8 {
		t.Fatalf("GPUs = %d, want 8", n.Resources.GPUs)
	}
	if got := f.gpu.membersOf("computegpucluster-e00c1"); !slices.Equal(got, []string{n.ID}) {
		t.Fatalf("gpu cluster members = %v", got)
	}
	if got := f.fs.attachedTo("computefilesystem-e00f1"); !slices.Equal(got, []string{n.ID}) {
		t.Fatalf("filesystem attachments = %v", got)
	}
	if f.get(t, n.ID).GetSpec().GetPreemptible() == nil {
		t.Fatal("node is not preemptible")
	}

	if err := f.svc.DeleteNode(f.ctx, n.ID); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if len(f.gpu.membersOf("computegpucluster-e00c1")) != 0 || len(f.fs.attachedTo("computefilesystem-e00f1")) != 0 {
		t.Fatal("DeleteNode left the GPU cluster or filesystem attached")
	}
}

func TestCreateNodeIsIdempotentAndListsInOrder(t *testing.T) {
	f := nodeFixture(t)
	spec := nodeSpecFor("node-b", cpuTemplate())
	first, err := f.svc.CreateNode(f.ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.CreateNode(f.ctx, spec)
	if err != nil {
		t.Fatalf("second CreateNode: %v", err)
	}
	if diff := cmp.Diff(first, again); diff != "" {
		t.Fatalf("second CreateNode differs (-first +again):\n%s", diff)
	}
	if got := f.repo.count(); got != 1 {
		t.Fatalf("%d instances, want 1", got)
	}
	second, err := f.svc.CreateNode(f.ctx, nodeSpecFor("node-a", cpuTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	other := nodeSpecFor("node-x", cpuTemplate())
	other.NodeGroupID = "mk8snodegroup-e00ng2"
	if _, err := f.svc.CreateNode(f.ctx, other); err != nil {
		t.Fatal(err)
	}

	got, err := f.svc.ListNodes(f.ctx, nodeGroup)
	if err != nil {
		t.Fatal(err)
	}
	if want := []peer.NodeInstance{first, second}; cmp.Diff(want, got) != "" {
		t.Fatalf("ListNodes = %+v, want creation order %+v", got, want)
	}
}

func TestCreateNodeConcurrentSameName(t *testing.T) {
	f := nodeFixture(t)
	spec := nodeSpecFor("node-c", cpuTemplate())
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			n, err := f.svc.CreateNode(f.ctx, spec)
			if err != nil {
				t.Errorf("CreateNode: %v", err)
			}
			ids[i] = n.ID
		})
	}
	wg.Wait()
	if f.repo.count() != 1 || len(slices.Compact(ids)) != 1 {
		t.Fatalf("instances = %d, ids = %v; want one node", f.repo.count(), ids)
	}
}

func TestCreateNodeFailureLeavesNothing(t *testing.T) {
	f := nodeFixture(t)
	tpl := cpuTemplate()
	tpl.ImageFamily = "no-such-family"
	if _, err := f.svc.CreateNode(f.ctx, nodeSpecFor("node-0", tpl)); err == nil {
		t.Fatal("CreateNode with a missing image family succeeded")
	}
	if f.repo.count() != 0 || f.diskCount() != 0 || len(f.net.boundNICs()) != 0 {
		t.Fatalf("leftovers: %d instances, %d disks, NICs %v", f.repo.count(), f.diskCount(), f.net.boundNICs())
	}
	if list, _ := f.svc.ListNodes(f.ctx, nodeGroup); len(list) != 0 {
		t.Fatalf("ListNodes = %+v, want none", list)
	}
}

func TestDeleteNode(t *testing.T) {
	f := nodeFixture(t)
	tpl := cpuTemplate()
	tpl.PublicIP = true
	n, err := f.svc.CreateNode(f.ctx, nodeSpecFor("node-0", tpl))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteNode(f.ctx, n.ID); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if f.repo.count() != 0 || f.diskCount() != 0 || len(f.net.boundNICs()) != 0 {
		t.Fatalf("leftovers: %d instances, %d disks, NICs %v", f.repo.count(), f.diskCount(), f.net.boundNICs())
	}
	if list, _ := f.svc.ListNodes(f.ctx, nodeGroup); len(list) != 0 {
		t.Fatalf("ListNodes = %+v, want none", list)
	}
	if err := f.svc.DeleteNode(f.ctx, n.ID); err != nil {
		t.Fatalf("DeleteNode of a missing node: %v", err)
	}
	// The name is free again for the node group.
	if _, err := f.svc.CreateNode(f.ctx, nodeSpecFor("node-0", tpl)); err != nil {
		t.Fatalf("recreate: %v", err)
	}
}

func TestCheckTemplate(t *testing.T) {
	f := nodeFixture(t)
	gpu := cpuTemplate()
	gpu.Platform, gpu.Preset = "gpu-h100-sxm", "8gpu-128vcpu-1600gb"
	got, err := f.svc.CheckTemplate(f.ctx, testProject, gpu)
	if err != nil {
		t.Fatalf("CheckTemplate: %v", err)
	}
	if want := (peer.NodeResources{GPUs: 8, VCPUs: 128, Memory: 1600}); got != want {
		t.Fatalf("resources = %+v, want %+v", got, want)
	}
	if f.repo.count() != 0 || f.diskCount() != 0 || len(f.net.boundNICs()) != 0 || len(f.nodes.order) != 0 {
		t.Fatal("CheckTemplate stored something")
	}

	edit := func(fn func(*peer.NodeTemplate)) peer.NodeTemplate {
		tpl := cpuTemplate()
		fn(&tpl)
		return tpl
	}
	tests := []struct {
		name  string
		tpl   peer.NodeTemplate
		kind  errs.Kind
		field string
	}{
		{"unknown preset", edit(func(t *peer.NodeTemplate) { t.Preset = "9vcpu-1gb" }), errs.KindBadRequest, "resources.preset"},
		{"missing preset", edit(func(t *peer.NodeTemplate) { t.Preset = "" }), errs.KindBadRequest, "resources.preset"},
		{"missing platform", edit(func(t *peer.NodeTemplate) { t.Platform = "" }), errs.KindBadRequest, "resources.platform"},
		{"missing subnet", edit(func(t *peer.NodeTemplate) { t.SubnetID = "" }), errs.KindBadRequest, "network_interfaces[0].subnet_id"},
		{"subnet of another project", edit(func(t *peer.NodeTemplate) { t.SubnetID = "subnet-other" }), errs.KindBadRequest, "network_interfaces[0].subnet_id"},
		{"service account of another project", edit(func(t *peer.NodeTemplate) { t.ServiceAccountID = otherSA }), errs.KindBadRequest, "service_account_id"},
		{"gpu cluster on a cpu preset", edit(func(t *peer.NodeTemplate) { t.GpuClusterID = "computegpucluster-e00c1" }), errs.KindBadRequest, "gpu_cluster.id"},
		{"filesystem without a mount tag", edit(func(t *peer.NodeTemplate) {
			t.Filesystems = []peer.NodeFilesystem{{ID: "computefilesystem-e00f1", MountTag: "a"}, {ID: "computefilesystem-e00f2"}}
		}), errs.KindBadRequest, "filesystems[1].mount_tag"},
		{"filesystem without an id", edit(func(t *peer.NodeTemplate) {
			t.Filesystems = []peer.NodeFilesystem{{MountTag: "a"}}
		}), errs.KindBadRequest, "filesystems[0].existing_filesystem.id"},
		{"read-only filesystem", edit(func(t *peer.NodeTemplate) {
			t.Filesystems = []peer.NodeFilesystem{{ID: "computefilesystem-e00f1", MountTag: "a", ReadOnly: true}}
		}), errs.KindBadRequest, "filesystems[0].attach_mode"},
		{"boot disk too small", edit(func(t *peer.NodeTemplate) { t.BootDisk.SizeBytes = 0 }), errs.KindBadRequest, "boot_disk.size_bytes"},
		{"unknown boot disk type", edit(func(t *peer.NodeTemplate) { t.BootDisk.Type = "NETWORK_TAPE" }), errs.KindBadRequest, "boot_disk.type"},
		{"boot disk type missing", edit(func(t *peer.NodeTemplate) { t.BootDisk.Type = "" }), errs.KindBadRequest, "boot_disk.type"},
		{"unknown image family", edit(func(t *peer.NodeTemplate) { t.ImageFamily = "nope" }), errs.KindNotFound, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.CheckTemplate(f.ctx, testProject, tc.tpl)
			if err == nil {
				t.Fatal("CheckTemplate succeeded")
			}
			wantKind(t, err, tc.kind, tc.field)
			if strings.Contains(fmt.Sprint(err), "spec.") {
				t.Fatalf("error %q leaks instance-spec field names", err)
			}
		})
	}
}

func TestManagedNodeRefusesUserMutations(t *testing.T) {
	f := nodeFixture(t)
	n, err := f.svc.CreateNode(f.ctx, nodeSpecFor("node-0", cpuTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	wantMsg := fmt.Sprintf("instance %s is a node of mk8s node group %s (cluster mk8scluster-e00c1) and is managed by it: change the node group instead", n.ID, nodeGroup)
	checks := map[string]error{
		"update": f.update(t, n.ID, func(s *computepb.InstanceSpec) { s.Hostname = "x" }, nil),
		"delete": f.remove(n.ID),
		"stop":   f.stop(n.ID),
		"start":  f.start(n.ID),
	}
	for name, err := range checks {
		t.Run(name, func(t *testing.T) {
			wantKind(t, err, errs.KindBadState, "")
			if e, ok := errors.AsType[*errs.Error](err); !ok || e.Message != wantMsg {
				t.Fatalf("error = %v, want message %q", err, wantMsg)
			}
		})
	}
	if got := f.get(t, n.ID); instState(got) != computepb.InstanceStatus_RUNNING || got.GetSpec().GetHostname() != "" {
		t.Fatalf("a refused mutation changed the node: %v", got)
	}
	// Reads are unchanged, and a user's own instance is not affected.
	if _, err := f.svc.Get(f.ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	own := f.mustCreate(t, "mine", baseSpec())
	if err := f.stop(own.GetMetadata().GetId()); err != nil {
		t.Fatalf("Stop of an ordinary instance: %v", err)
	}
	if err := f.svc.DeleteNode(f.ctx, n.ID); err != nil {
		t.Fatalf("DeleteNode after refusals: %v", err)
	}
}
