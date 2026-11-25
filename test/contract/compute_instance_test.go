package contract

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func protoEqual(t *testing.T, what string, got, want proto.Message) {
	t.Helper()
	if !proto.Equal(got, want) {
		t.Errorf("%s:\n got  %v\n want %v", what, got, want)
	}
}

// TestInstanceCreateAndRead creates a CPU and a GPU instance and reads them
// back through every read RPC; the spec must echo exactly what was sent.
func TestInstanceCreateAndRead(t *testing.T) {
	t.Parallel()
	c := startInstances(t)

	cpuSpec, gpuSpec := c.cpuSpec("cpu1"), c.gpuSpec("gpu1")
	cpuSpec.Hostname = "cpu-host"
	cpuSpec.CloudInitUserData = "#cloud-config\n"
	cpu := c.mustCreate(t, "cpu1", cpuSpec)
	gpu := c.mustCreate(t, "gpu1", gpuSpec)

	protoEqual(t, "cpu spec echo", cpu.GetSpec(), cpuSpec)
	protoEqual(t, "gpu spec echo", gpu.GetSpec(), gpuSpec)
	for _, i := range []*computepb.Instance{cpu, gpu} {
		if i.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
			t.Errorf("%s state = %v", i.GetMetadata().GetName(), i.GetStatus().GetState())
		}
		if i.GetMetadata().GetParentId() != c.project || i.GetMetadata().GetId() == "" {
			t.Errorf("metadata = %v", i.GetMetadata())
		}
		n := i.GetStatus().GetNetworkInterfaces()
		if len(n) != 1 || n[0].GetName() != "eth0" || n[0].GetIpAddress().GetAddress() == "" || strings.Contains(n[0].GetIpAddress().GetAddress(), "/") ||
			!strings.HasPrefix(n[0].GetMacAddress(), "02:4e:42:") || n[0].GetPublicIpAddress() != nil {
			t.Errorf("%s nic status = %v", i.GetMetadata().GetName(), n)
		}
		if len(n) == 1 && len(n[0].GetSecurityGroups()) == 0 {
			t.Errorf("%s: no default security group in status", i.GetMetadata().GetName())
		}
		da := i.GetStatus().GetDiskAttachments()
		if len(da) != 1 || !da[0].GetIsManaged() || da[0].GetName() != i.GetMetadata().GetName()+"-boot" {
			t.Errorf("%s disk attachments = %v", i.GetMetadata().GetName(), da)
		}
	}
	if got := cpu.GetStatus().GetNetworkInterfaces()[0].GetFqdn(); !strings.HasPrefix(got, "cpu-host.") || !strings.HasSuffix(got, ".compute.internal.") {
		t.Errorf("fqdn = %q", got)
	}
	if got := gpu.GetStatus().GetNetworkInterfaces()[0].GetFqdn(); !strings.HasPrefix(got, gpu.GetMetadata().GetId()+".") {
		t.Errorf("fqdn without hostname = %q", got)
	}
	if gpu.GetStatus().GetInfinibandTopologyPath() != nil {
		t.Errorf("instance outside a GPU cluster has topology path %v", gpu.GetStatus().GetInfinibandTopologyPath())
	}

	byName, err := c.instances.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: c.project, Name: "gpu1"})
	if err != nil {
		t.Fatal(err)
	}
	protoEqual(t, "GetByName", byName, gpu)
	_, err = c.instances.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: c.project, Name: "nope"})
	wantCode(t, "GetByName missing", err, codes.NotFound, "")
	_, err = c.instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: "computeinstance-e00doesnotexist"})
	wantCode(t, "Get missing", err, codes.NotFound, "")
	_, err = c.instances.Get(t.Context(), &computepb.GetInstanceRequest{})
	wantCode(t, "Get without id", err, codes.InvalidArgument, "id")

	seen := map[string]bool{}
	token := ""
	for range 5 {
		page, err := c.instances.List(t.Context(), &computepb.ListInstancesRequest{ParentId: c.project, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.GetItems()) > 1 {
			t.Fatalf("page size 1 returned %d items", len(page.GetItems()))
		}
		for _, i := range page.GetItems() {
			seen[i.GetMetadata().GetName()] = true
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(seen) != 2 || !seen["cpu1"] || !seen["gpu1"] {
		t.Errorf("paged names = %v", seen)
	}
	_, err = c.instances.List(t.Context(), &computepb.ListInstancesRequest{})
	wantCode(t, "List without parent", err, codes.InvalidArgument, "parent_id")

	_, err = c.create(t.Context(), "cpu1", c.cpuSpec("cpu1b"))
	wantCode(t, "duplicate name", err, codes.AlreadyExists, "")
}

// TestInstanceNICs covers every address kind and what the VPC shows for it.
func TestInstanceNICs(t *testing.T) {
	t.Parallel()
	c := startInstances(t)

	privAlloc := c.newAllocation(t, "user-private", privAllocSpec(c.subnet, "", ""))
	aliasAlloc := c.newAllocation(t, "user-alias", privAllocSpec(c.subnet, "", ""))
	pubAlloc := c.newAllocation(t, "user-public", pubAllocSpec(c.subnet, "", ""))
	sgOp, err := c.groups.Create(t.Context(), &vpcpb.CreateSecurityGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: "vm-sg"}, Spec: &vpcpb.SecurityGroupSpec{NetworkId: c.network},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sgOp.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	sgID := sgOp.ResourceID()

	spec := c.cpuSpec("nics")
	spec.NetworkInterfaces = []*computepb.NetworkInterfaceSpec{
		{
			Name: "eth0", SubnetId: c.subnet,
			IpAddress:       &computepb.IPAddress{AllocationId: privAlloc},
			PublicIpAddress: &computepb.PublicIPAddress{Allocation: &computepb.PublicIPAddress_AllocationId{AllocationId: pubAlloc}},
			Aliases:         []*computepb.IPAlias{{AllocationId: aliasAlloc}},
			SecurityGroups:  []*computepb.SecurityGroup{{Id: sgID}},
		},
		{Name: "eth1", SubnetId: c.subnet, IpAddress: &computepb.IPAddress{}, PublicIpAddress: &computepb.PublicIPAddress{}},
		{Name: "eth2", SubnetId: c.subnet, IpAddress: &computepb.IPAddress{}, PublicIpAddress: &computepb.PublicIPAddress{Static: true}},
	}
	inst := c.mustCreate(t, "nics", spec)
	id := inst.GetMetadata().GetId()
	protoEqual(t, "spec echo", inst.GetSpec(), spec)

	n := inst.GetStatus().GetNetworkInterfaces()
	if len(n) != 3 {
		t.Fatalf("nic status = %v", n)
	}
	if n[0].GetIpAddress().GetAllocationId() != privAlloc || n[0].GetPublicIpAddress().GetAllocationId() != pubAlloc ||
		len(n[0].GetAliases().GetCidrs()) != 1 {
		t.Errorf("eth0 status = %v", n[0])
	}
	if len(n[0].GetSecurityGroups()) != 1 || n[0].GetSecurityGroups()[0].GetId() != sgID {
		t.Errorf("eth0 security groups = %v, want [%s]", n[0].GetSecurityGroups(), sgID)
	}
	if n[1].GetPublicIpAddress().GetAddress() == "" || n[1].GetPublicIpAddress().GetStatic() {
		t.Errorf("eth1 public = %v, want dynamic", n[1].GetPublicIpAddress())
	}
	if !n[2].GetPublicIpAddress().GetStatic() {
		t.Errorf("eth2 public = %v, want static", n[2].GetPublicIpAddress())
	}
	if len(n[1].GetSecurityGroups()) == 0 || n[1].GetSecurityGroups()[0].GetId() == sgID {
		t.Errorf("eth1 security groups = %v, want the network default", n[1].GetSecurityGroups())
	}

	assigned := func(allocID, nicName string, typ vpcpb.NetworkInterfaceAssignment_Type) {
		t.Helper()
		a := c.alloc(t, allocID)
		got := a.GetStatus().GetAssignment().GetNetworkInterface()
		if a.GetStatus().GetState() != vpcpb.AllocationStatus_ASSIGNED || got.GetInstanceId() != id || got.GetName() != nicName || got.GetType() != typ {
			t.Errorf("allocation %s: state %v assignment %v, want ASSIGNED %s/%s/%v", a.GetMetadata().GetName(), a.GetStatus().GetState(), got, id, nicName, typ)
		}
	}
	assigned(privAlloc, "eth0", vpcpb.NetworkInterfaceAssignment_PRIMARY)
	assigned(aliasAlloc, "eth0", vpcpb.NetworkInterfaceAssignment_ALIAS)
	assigned(pubAlloc, "eth0", vpcpb.NetworkInterfaceAssignment_PUBLIC)
	assigned(n[1].GetIpAddress().GetAllocationId(), "eth1", vpcpb.NetworkInterfaceAssignment_PRIMARY)
	assigned(n[1].GetPublicIpAddress().GetAllocationId(), "eth1", vpcpb.NetworkInterfaceAssignment_PUBLIC)
	if a := c.alloc(t, n[1].GetIpAddress().GetAllocationId()); a.GetStatus().GetStatic() {
		t.Errorf("dynamic allocation reports static")
	}

	delOp, err := c.allocs.Delete(t.Context(), &vpcpb.DeleteAllocationRequest{Id: privAlloc})
	if err == nil {
		_, err = delOp.Wait(t.Context())
	}
	wantCode(t, "delete assigned allocation", err, codes.FailedPrecondition, "")

	c.mustDelete(t, id)
	for _, a := range []string{privAlloc, aliasAlloc, pubAlloc} {
		got := c.alloc(t, a)
		if got.GetStatus().GetState() != vpcpb.AllocationStatus_ALLOCATED || got.GetStatus().GetAssignment() != nil {
			t.Errorf("user allocation %s after delete: state %v assignment %v", got.GetMetadata().GetName(), got.GetStatus().GetState(), got.GetStatus().GetAssignment())
		}
	}
	for _, a := range []string{
		n[1].GetIpAddress().GetAllocationId(), n[1].GetPublicIpAddress().GetAllocationId(),
		n[2].GetIpAddress().GetAllocationId(), n[2].GetPublicIpAddress().GetAllocationId(),
	} {
		_, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: a})
		wantCode(t, "dynamic allocation "+a+" after delete", err, codes.NotFound, "")
	}
}

// TestInstanceStartStop covers the Start and Stop RPCs and dynamic public IPs.
func TestInstanceStartStop(t *testing.T) {
	t.Parallel()
	c := startInstances(t)
	spec := c.cpuSpec("ss")
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{}
	inst := c.mustCreate(t, "ss", spec)
	id := inst.GetMetadata().GetId()
	oldPublic := inst.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
	if oldPublic.GetAddress() == "" {
		t.Fatalf("no public ip: %v", inst.GetStatus())
	}

	wantCode(t, "Start running", c.start(t.Context(), id), codes.FailedPrecondition, "")
	if err := c.stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stopped := c.get(t, id)
	if stopped.GetStatus().GetState() != computepb.InstanceStatus_STOPPED || stopped.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress() != nil {
		t.Errorf("after stop: %v", stopped.GetStatus())
	}
	if stopped.GetSpec().GetStopped() {
		t.Errorf("the Stop RPC changed spec.stopped")
	}
	_, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: oldPublic.GetAllocationId()})
	wantCode(t, "dynamic public allocation after stop", err, codes.NotFound, "")
	wantCode(t, "Stop stopped", c.stop(t.Context(), id), codes.FailedPrecondition, "")

	if err := c.start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	started := c.get(t, id)
	if started.GetStatus().GetState() != computepb.InstanceStatus_RUNNING || started.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress().GetAddress() == "" {
		t.Errorf("after start: %v", started.GetStatus())
	}
	if started.GetStatus().GetNetworkInterfaces()[0].GetMacAddress() != inst.GetStatus().GetNetworkInterfaces()[0].GetMacAddress() {
		t.Errorf("mac address changed across stop/start")
	}

	_, err = c.instances.Start(t.Context(), &computepb.StartInstanceRequest{Id: "computeinstance-e00doesnotexist"})
	wantCode(t, "Start missing", err, codes.NotFound, "")
	_, err = c.instances.Stop(t.Context(), &computepb.StopInstanceRequest{Id: "computeinstance-e00doesnotexist"})
	wantCode(t, "Stop missing", err, codes.NotFound, "")

	// A static public address survives a stop.
	spec2 := c.cpuSpec("static")
	spec2.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{Static: true}
	st := c.mustCreate(t, "static", spec2)
	addr := st.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress().GetAddress()
	if err := c.stop(t.Context(), st.GetMetadata().GetId()); err != nil {
		t.Fatal(err)
	}
	if got := c.get(t, st.GetMetadata().GetId()).GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress().GetAddress(); got != addr || addr == "" {
		t.Errorf("static public address after stop = %q, want %q", got, addr)
	}
}

// TestInstanceUpdate covers labels, the stopped toggle, preset change,
// immutable fields and hostname changes.
func TestInstanceUpdate(t *testing.T) {
	t.Parallel()
	c := startInstances(t, testkit.WithOperationDelay(500*time.Millisecond))
	inst := c.mustCreate(t, "upd", c.cpuSpec("upd"))
	id := inst.GetMetadata().GetId()

	func() {
		next := proto.Clone(inst).(*computepb.Instance)
		next.GetMetadata().Labels = map[string]string{"team": "a"}
		op, err := c.instances.Update(t.Context(), &computepb.UpdateInstanceRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := op.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		got := c.get(t, id)
		if got.GetMetadata().GetLabels()["team"] != "a" || got.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
			t.Errorf("after labels update: %v %v", got.GetMetadata(), got.GetStatus().GetState())
		}
		if got.GetMetadata().GetResourceVersion() <= inst.GetMetadata().GetResourceVersion() {
			t.Errorf("resource_version %d did not grow from %d", got.GetMetadata().GetResourceVersion(), inst.GetMetadata().GetResourceVersion())
		}
		protoEqual(t, "spec unchanged", got.GetSpec(), inst.GetSpec())
	}()

	func() {
		cur := c.get(t, id)
		next := proto.Clone(cur).(*computepb.Instance)
		next.GetSpec().Stopped = true
		op, err := c.instances.Update(resetCtx(t.Context(), t, instResetMask), &computepb.UpdateInstanceRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.get(t, id).GetStatus().GetState(); got != computepb.InstanceStatus_STOPPING {
			t.Errorf("state while the operation runs = %v, want STOPPING", got)
		}
		if _, err := op.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := c.get(t, id); got.GetStatus().GetState() != computepb.InstanceStatus_STOPPED || !got.GetSpec().GetStopped() {
			t.Errorf("after stopped=true: %v stopped=%v", got.GetStatus().GetState(), got.GetSpec().GetStopped())
		}
	}()

	func() {
		stopped := c.get(t, id)
		if err := c.update(t.Context(), t, stopped, func(s *computepb.InstanceSpec) { s.Resources = preset("cpu-e2", "4vcpu-16gb") }); err != nil {
			t.Fatalf("preset change while STOPPED: %v", err)
		}
		got := c.get(t, id)
		if got.GetSpec().GetResources().GetPreset() != "4vcpu-16gb" || got.GetStatus().GetState() != computepb.InstanceStatus_STOPPED {
			t.Errorf("after preset change: %v %v", got.GetSpec().GetResources(), got.GetStatus().GetState())
		}
	}()

	func() {
		cur := c.get(t, id)
		next := proto.Clone(cur).(*computepb.Instance)
		next.GetSpec().Stopped = false
		op, err := c.instances.Update(resetCtx(t.Context(), t, instResetMask), &computepb.UpdateInstanceRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.get(t, id).GetStatus().GetState(); got != computepb.InstanceStatus_STARTING {
			t.Errorf("state while the operation runs = %v, want STARTING", got)
		}
		if _, err := op.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		running := c.get(t, id)
		if running.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
			t.Fatalf("state = %v", running.GetStatus().GetState())
		}
		err = c.update(t.Context(), t, running, func(s *computepb.InstanceSpec) { s.Resources = preset("cpu-e2", "8vcpu-32gb") })
		wantCode(t, "preset change while RUNNING", err, codes.FailedPrecondition, "")
	}()

	func() {
		err := c.update(t.Context(), t, c.get(t, id), func(s *computepb.InstanceSpec) { s.Resources = preset("cpu-d3", "4vcpu-16gb") })
		wantCode(t, "platform change", err, codes.InvalidArgument, "platform")
	}()

	func() {
		got := c.mustUpdate(t, c.get(t, id), func(s *computepb.InstanceSpec) { s.Hostname = "renamed" })
		if f := got.GetStatus().GetNetworkInterfaces()[0].GetFqdn(); !strings.HasPrefix(f, "renamed.") {
			t.Errorf("fqdn after hostname change = %q", f)
		}
	}()

	func() {
		_, err := c.instances.Update(t.Context(), &computepb.UpdateInstanceRequest{
			Metadata: &commonpb.ResourceMetadata{Id: "computeinstance-e00doesnotexist", ParentId: c.project, Name: "x"}, Spec: c.cpuSpec("x"),
		})
		wantCode(t, "update missing", err, codes.NotFound, "")
	}()
}

// TestInstanceBatchGet reads existing and missing ids in one call.
func TestInstanceBatchGet(t *testing.T) {
	t.Parallel()
	c := startInstances(t)
	inst := c.mustCreate(t, "b1", c.cpuSpec("b1"))
	res, err := c.instances.BatchGet(t.Context(), &computepb.BatchGetRequest{InstanceIds: []string{inst.GetMetadata().GetId(), "computeinstance-e00doesnotexist"}})
	if err != nil {
		t.Fatal(err)
	}
	items := res.GetItems()
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	protoEqual(t, "found", items[inst.GetMetadata().GetId()].GetInstance(), inst)
	if e := items["computeinstance-e00doesnotexist"].GetError(); e == nil || codes.Code(e.GetCode()) != codes.NotFound {
		t.Errorf("missing entry = %v, want NotFound status", items["computeinstance-e00doesnotexist"])
	}
	_, err = c.instances.BatchGet(t.Context(), &computepb.BatchGetRequest{})
	wantCode(t, "empty BatchGet", err, codes.InvalidArgument, "")
}

// TestInstanceNVLAndOperations covers ListInstancesByNVLInstanceGroup and
// ListOperationsByParent.
func TestInstanceNVLAndOperations(t *testing.T) {
	t.Parallel()
	c := startInstances(t)
	_, err := c.instances.ListInstancesByNVLInstanceGroup(t.Context(), &computepb.ListInstancesByNVLInstanceGroupRequest{NvlInstanceGroupId: "computenvlinstancegroup-e00none"})
	wantCode(t, "ListInstancesByNVLInstanceGroup", err, codes.NotFound, "")

	a := c.mustCreate(t, "ops-a", c.cpuSpec("ops-a"))
	b := c.mustCreate(t, "ops-b", c.cpuSpec("ops-b"))
	if err := c.stop(t.Context(), a.GetMetadata().GetId()); err != nil {
		t.Fatal(err)
	}
	c.mustDelete(t, a.GetMetadata().GetId())

	ops, err := c.instances.ListOperationsByParent(t.Context(), &computepb.ListOperationsByParentRequest{ParentId: c.project})
	if err != nil {
		t.Fatal(err)
	}
	var forA []string
	var all []string
	for _, o := range ops.GetOperations() {
		all = append(all, o.GetResourceId()+":"+strings.ToLower(o.GetDescription()))
		if o.GetResourceId() == a.GetMetadata().GetId() {
			forA = append(forA, strings.ToLower(o.GetDescription()))
		}
	}
	if len(forA) != 3 || !strings.HasPrefix(forA[0], "delete") || !strings.HasPrefix(forA[1], "stop") || !strings.HasPrefix(forA[2], "create") {
		t.Errorf("deleted instance operations (newest first) = %v, want delete, stop, create; all = %v", forA, all)
	}
	if len(all) != 4 {
		t.Errorf("operations = %v, want 4 (create a, create b, stop a, delete a)", all)
	}
	if !strings.HasPrefix(all[0], a.GetMetadata().GetId()) || !strings.HasSuffix(all[3], ":"+all[3][strings.Index(all[3], ":")+1:]) {
		t.Errorf("order = %v", all)
	}
	_ = b
	_, err = c.instances.ListOperationsByParent(t.Context(), &computepb.ListOperationsByParentRequest{})
	wantCode(t, "ListOperationsByParent without parent", err, codes.InvalidArgument, "parent_id")
}
