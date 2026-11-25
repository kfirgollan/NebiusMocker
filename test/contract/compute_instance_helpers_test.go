package contract

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// instClients are the SDK clients of one principal against a mocker running
// IAM, Compute and VPC.
type instClients struct {
	env       *imagescenario.Env
	instances computesvc.InstanceService
	disks     computesvc.DiskService
	allocs    vpcsvc.AllocationService
	networks  vpcsvc.NetworkService
	groups    vpcsvc.SecurityGroupService
	project   string
	subnet    string // the seeded default subnet
	network   string // its network
}

func startInstances(t *testing.T, opts ...testkit.Option) instClients {
	t.Helper()
	env := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{vpc.New()}, opts...)
	return instClientsFor(t, env, "")
}

// instClientsFor builds the clients for token (the dev admin when empty).
func instClientsFor(t *testing.T, env *imagescenario.Env, token string) instClients {
	t.Helper()
	sdk := env.Inst.SDK(t)
	if token != "" {
		sdk = env.Inst.SDKAs(t, token)
	}
	c := instClients{
		env: env, instances: computesvc.NewInstanceService(sdk), disks: computesvc.NewDiskService(sdk),
		allocs: vpcsvc.NewAllocationService(sdk), networks: vpcsvc.NewNetworkService(sdk),
		groups: vpcsvc.NewSecurityGroupService(sdk), project: env.Inst.ProjectID,
	}
	sn, err := vpcsvc.NewSubnetService(sdk).GetByName(t.Context(), &vpcpb.GetSubnetByNameRequest{ParentId: c.project, Name: "default-subnet"})
	if err == nil {
		c.subnet, c.network = sn.GetMetadata().GetId(), sn.GetSpec().GetNetworkId()
	}
	return c
}

func imageDisk(name, family string, size int64) *computepb.AttachedDiskSpec {
	return &computepb.AttachedDiskSpec{
		AttachMode: computepb.AttachedDiskSpec_READ_WRITE,
		Type: &computepb.AttachedDiskSpec_ManagedDisk{ManagedDisk: &computepb.ManagedDisk{
			Name: name,
			Spec: &computepb.DiskSpec{
				Type: computepb.DiskSpec_NETWORK_SSD, Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: size},
				Source: &computepb.DiskSpec_SourceImageFamily{SourceImageFamily: &computepb.SourceImageFamily{ImageFamily: family}},
			},
		}},
	}
}

func preset(platform, name string) *computepb.ResourcesSpec {
	return &computepb.ResourcesSpec{Platform: platform, Size: &computepb.ResourcesSpec_Preset{Preset: name}}
}

func nic(name, subnet string) *computepb.NetworkInterfaceSpec {
	return &computepb.NetworkInterfaceSpec{Name: name, SubnetId: subnet, IpAddress: &computepb.IPAddress{}}
}

// cpuSpec is a minimal valid CPU instance: managed boot disk, one dynamic
// private NIC.
func (c instClients) cpuSpec(name string) *computepb.InstanceSpec {
	return &computepb.InstanceSpec{
		Resources:         preset("cpu-e2", "2vcpu-8gb"),
		BootDisk:          imageDisk(name+"-boot", "ubuntu24.04-driverless", 64),
		NetworkInterfaces: []*computepb.NetworkInterfaceSpec{nic("eth0", c.subnet)},
	}
}

func (c instClients) gpuSpec(name string) *computepb.InstanceSpec {
	return &computepb.InstanceSpec{
		Resources:         preset("gpu-h100-sxm", "8gpu-128vcpu-1600gb"),
		BootDisk:          imageDisk(name+"-boot", "ubuntu24.04-cuda12", 200),
		NetworkInterfaces: []*computepb.NetworkInterfaceSpec{nic("eth0", c.subnet)},
	}
}

func (c instClients) createReq(name string, spec *computepb.InstanceSpec) *computepb.CreateInstanceRequest {
	return &computepb.CreateInstanceRequest{Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: name}, Spec: spec}
}

// create creates an instance and waits for the operation.
func (c instClients) create(ctx context.Context, name string, spec *computepb.InstanceSpec) (*computepb.Instance, error) {
	op, err := c.instances.Create(ctx, c.createReq(name, spec))
	if err != nil {
		return nil, err
	}
	if _, err := op.Wait(ctx); err != nil {
		return nil, err
	}
	return c.instances.Get(ctx, &computepb.GetInstanceRequest{Id: op.ResourceID()})
}

func (c instClients) mustCreate(t *testing.T, name string, spec *computepb.InstanceSpec) *computepb.Instance {
	t.Helper()
	i, err := c.create(t.Context(), name, spec)
	if err != nil {
		t.Fatalf("create instance %s: %v", name, err)
	}
	return i
}

func (c instClients) get(t *testing.T, id string) *computepb.Instance {
	t.Helper()
	i, err := c.instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: id})
	if err != nil {
		t.Fatalf("get instance %s: %v", id, err)
	}
	return i
}

// update resends the instance with change applied and a full reset mask.
func (c instClients) update(ctx context.Context, t *testing.T, i *computepb.Instance, change func(*computepb.InstanceSpec)) error {
	t.Helper()
	next := proto.Clone(i).(*computepb.Instance)
	change(next.GetSpec())
	op, err := c.instances.Update(resetCtx(ctx, t, instResetMask), &computepb.UpdateInstanceRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c instClients) mustUpdate(t *testing.T, i *computepb.Instance, change func(*computepb.InstanceSpec)) *computepb.Instance {
	t.Helper()
	if err := c.update(t.Context(), t, i, change); err != nil {
		t.Fatalf("update instance %s: %v", i.GetMetadata().GetName(), err)
	}
	return c.get(t, i.GetMetadata().GetId())
}

func (c instClients) del(ctx context.Context, id string) error {
	op, err := c.instances.Delete(ctx, &computepb.DeleteInstanceRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c instClients) mustDelete(t *testing.T, id string) {
	t.Helper()
	if err := c.del(t.Context(), id); err != nil {
		t.Fatalf("delete instance %s: %v", id, err)
	}
}

func (c instClients) start(ctx context.Context, id string) error {
	op, err := c.instances.Start(ctx, &computepb.StartInstanceRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c instClients) stop(ctx context.Context, id string) error {
	op, err := c.instances.Stop(ctx, &computepb.StopInstanceRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// newDisk creates a standalone SSD disk.
func (c instClients) newDisk(t *testing.T, name string, size int64) *computepb.Disk {
	t.Helper()
	op, err := c.disks.Create(t.Context(), &computepb.CreateDiskRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: name}, Spec: ssdSpec(size),
	})
	if err != nil {
		t.Fatalf("create disk %s: %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("create disk %s: %v", name, err)
	}
	return c.disk(t, op.ResourceID())
}

func (c instClients) disk(t *testing.T, id string) *computepb.Disk {
	t.Helper()
	d, err := c.disks.Get(t.Context(), &computepb.GetDiskRequest{Id: id})
	if err != nil {
		t.Fatalf("get disk %s: %v", id, err)
	}
	return d
}

func (c instClients) alloc(t *testing.T, id string) *vpcpb.Allocation {
	t.Helper()
	a, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: id})
	if err != nil {
		t.Fatalf("get allocation %s: %v", id, err)
	}
	return a
}

// newAllocation creates an allocation.
func (c instClients) newAllocation(t *testing.T, name string, spec *vpcpb.AllocationSpec) string {
	t.Helper()
	op, err := c.allocs.Create(t.Context(), &vpcpb.CreateAllocationRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: name}, Spec: spec,
	})
	if err != nil {
		t.Fatalf("create allocation %s: %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("create allocation %s: %v", name, err)
	}
	return op.ResourceID()
}

// wantCode asserts the gRPC code of err; when field is set it also checks
// that the message or the BadRequest violations name it.
func wantCode(t *testing.T, what string, err error, code codes.Code, field string) {
	t.Helper()
	if got := status.Code(err); got != code {
		t.Errorf("%s: code = %v, want %v (err: %v)", what, got, code, err)
		return
	}
	if field == "" {
		return
	}
	if !strings.Contains(err.Error(), field) && !slices.ContainsFunc(authKeyViolationFields(err), func(f string) bool { return strings.Contains(f, field) }) {
		t.Errorf("%s: error does not name %q: %v", what, field, err)
	}
}

// instResetMask lists the fields whose zero values the tests send.
const instResetMask = "spec.secondary_disks,spec.stopped,spec.hostname"
