package contract

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// TestInstanceSmoke walks one instance through its lifecycle with the
// official SDK: create (managed boot disk, dynamic private and public
// address), stop, start, delete.
func TestInstanceSmoke(t *testing.T) {
	t.Parallel()
	env := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{vpc.New()})
	ctx := t.Context()
	sdk := env.Inst.SDKAs(t, config.DefaultDevToken)
	instances, disks := computesvc.NewInstanceService(sdk), computesvc.NewDiskService(sdk)
	allocs := vpcsvc.NewAllocationService(sdk)
	project := env.Inst.ProjectID
	subnet, err := vpcsvc.NewSubnetService(sdk).GetByName(ctx, &vpcpb.GetSubnetByNameRequest{ParentId: project, Name: "default-subnet"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := instances.Create(ctx, &computepb.CreateInstanceRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: "vm1"},
		Spec: &computepb.InstanceSpec{
			Resources: &computepb.ResourcesSpec{Platform: "cpu-e2", Size: &computepb.ResourcesSpec_Preset{Preset: "2vcpu-8gb"}},
			BootDisk: &computepb.AttachedDiskSpec{
				AttachMode: computepb.AttachedDiskSpec_READ_WRITE,
				Type: &computepb.AttachedDiskSpec_ManagedDisk{ManagedDisk: &computepb.ManagedDisk{
					Name: "vm1-boot",
					Spec: &computepb.DiskSpec{
						Type: computepb.DiskSpec_NETWORK_SSD, Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 64},
						Source: &computepb.DiskSpec_SourceImageFamily{SourceImageFamily: &computepb.SourceImageFamily{ImageFamily: "ubuntu24.04-driverless"}},
					},
				}},
			},
			NetworkInterfaces: []*computepb.NetworkInterfaceSpec{{
				Name: "eth0", SubnetId: subnet.GetMetadata().GetId(),
				IpAddress: &computepb.IPAddress{}, PublicIpAddress: &computepb.PublicIPAddress{},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	inst, err := instances.Get(ctx, &computepb.GetInstanceRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("instance: %v", inst)
	if inst.GetStatus().GetState() != computepb.InstanceStatus_RUNNING {
		t.Fatalf("state = %v", inst.GetStatus().GetState())
	}
	nic := inst.GetStatus().GetNetworkInterfaces()[0]
	if nic.GetIpAddress().GetAddress() == "" || nic.GetPublicIpAddress().GetAddress() == "" {
		t.Fatalf("nic = %v", nic)
	}
	boot, err := disks.Get(ctx, &computepb.GetDiskRequest{Id: inst.GetStatus().GetDiskAttachments()[0].GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if boot.GetStatus().GetManagedBy() != inst.GetMetadata().GetId() || boot.GetStatus().GetReadWriteAttachment() != inst.GetMetadata().GetId() {
		t.Fatalf("boot disk status = %v", boot.GetStatus())
	}
	a, err := allocs.Get(ctx, &vpcpb.GetAllocationRequest{Id: nic.GetIpAddress().GetAllocationId()})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetStatus().GetAssignment().GetNetworkInterface().GetInstanceId() != inst.GetMetadata().GetId() {
		t.Fatalf("allocation = %v", a)
	}
	stop, err := instances.Stop(ctx, &computepb.StopInstanceRequest{Id: inst.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	inst, _ = instances.Get(ctx, &computepb.GetInstanceRequest{Id: op.ResourceID()})
	if inst.GetStatus().GetState() != computepb.InstanceStatus_STOPPED || inst.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress() != nil {
		t.Fatalf("after stop: %v", inst.GetStatus())
	}
	start, err := instances.Start(ctx, &computepb.StartInstanceRequest{Id: inst.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := start.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	del, err := instances.Delete(ctx, &computepb.DeleteInstanceRequest{Id: inst.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := del.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := disks.Get(ctx, &computepb.GetDiskRequest{Id: boot.GetMetadata().GetId()}); err == nil {
		t.Fatal("managed boot disk survived instance delete")
	}
	if _, err := allocs.Get(ctx, &vpcpb.GetAllocationRequest{Id: a.GetMetadata().GetId()}); err == nil {
		t.Fatal("dynamic allocation survived instance delete")
	}
}
