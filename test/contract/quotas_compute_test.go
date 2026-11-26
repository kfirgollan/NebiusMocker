package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	mk8ssvc "github.com/nebius/gosdk/services/nebius/mk8s/v1"
	quotassvc "github.com/nebius/gosdk/services/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// TestQuotaComputeAndMK8s proves the compute and mk8s wiring through gosdk:
// disk size bytes, GPU clusters and mk8s clusters are refused at their
// allowance, and a node group whose nodes would exceed compute.instance.count
// is refused synchronously; deleting a disk releases its bytes.
func TestQuotaComputeAndMK8s(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New(), quotas.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(20*time.Millisecond), testkit.WithAuthz(authz.ModeEnforce))
	sdk := inst.SDK(t)
	e := quotaEnv{inst: inst, quotas: quotassvc.NewQuotaAllowanceService(sdk)}
	p, ctx := inst.ProjectID, t.Context()
	disks, gpuClusters := computesvc.NewDiskService(sdk), computesvc.NewGpuClusterService(sdk)
	clusters, groups := mk8ssvc.NewClusterService(sdk), mk8ssvc.NewNodeGroupService(sdk)
	meta := func(name string) *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{ParentId: p, Name: name}
	}
	const gib = 1 << 30
	diskSize := quota.ComputeDiskSize("NETWORK_SSD")

	// Disk bytes: 3 GiB of room refuses a 4 GiB disk, admits a 2 GiB one.
	used := e.byName(t, p, diskSize).GetStatus().GetUsage()
	e.setLimit(t, p, diskSize, used+3*gib)
	newDisk := func(name string, size int64) (string, error) {
		op, err := disks.Create(ctx, &computepb.CreateDiskRequest{Metadata: meta(name), Spec: &computepb.DiskSpec{
			Type: computepb.DiskSpec_NETWORK_SSD, Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: size},
		}})
		if err != nil {
			return "", err
		}
		_, err = op.Wait(ctx)
		return op.ResourceID(), err
	}
	if _, err := newDisk("big", 4); err == nil {
		t.Fatal("4 GiB disk admitted with 3 GiB of room")
	} else if v := quotaFailure(t, err).Violations(); v[0].GetQuota() != diskSize || v[0].GetRequested() != "4294967296" {
		t.Fatalf("disk violations = %v", v)
	}
	smallID, err := newDisk("small", 2)
	if err != nil {
		t.Fatalf("2 GiB disk: %v", err)
	}
	if got := e.byName(t, p, diskSize).GetStatus().GetUsage(); got != used+2*gib {
		t.Fatalf("disk bytes = %d, want %d", got, used+2*gib)
	}
	if err := waitOK(ctx)(disks.Delete(ctx, &computepb.DeleteDiskRequest{Id: smallID})); err != nil {
		t.Fatal(err)
	}
	if _, err := newDisk("big", 3); err != nil {
		t.Fatalf("3 GiB disk after the 2 GiB one was deleted: %v", err)
	}
	diskAllowance := e.byName(t, p, diskSize).GetMetadata().GetId()
	if err := waitOK(ctx)(e.quotas.Delete(ctx, &quotaspb.DeleteQuotaAllowanceRequest{Id: diskAllowance})); err != nil {
		t.Fatalf("reset %s: %v", diskSize, err)
	}

	// GPU clusters.
	e.setLimit(t, p, quota.ComputeGPUClusterCount, e.byName(t, p, quota.ComputeGPUClusterCount).GetStatus().GetUsage())
	quotaFailure(t, waitOK(ctx)(gpuClusters.Create(ctx, &computepb.CreateGpuClusterRequest{
		Metadata: meta("ib"), Spec: &computepb.GpuClusterSpec{InfinibandFabric: "fabric-3"},
	})))

	// mk8s clusters.
	clusterSpec := &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{
		Endpoints: &mk8spb.ControlPlaneEndpointsSpec{PublicEndpoint: &mk8spb.PublicEndpointSpec{}},
	}}
	mk8sUsed := e.byName(t, p, quota.MK8sClusterCount).GetStatus().GetUsage()
	e.setLimit(t, p, quota.MK8sClusterCount, mk8sUsed+1)
	clusterID := wait(t)(clusters.Create(ctx, &mk8spb.CreateClusterRequest{Metadata: meta("k8s"), Spec: clusterSpec}))
	quotaFailure(t, waitOK(ctx)(clusters.Create(ctx, &mk8spb.CreateClusterRequest{Metadata: meta("k8s-2"), Spec: clusterSpec})))

	// Node groups: nodes are compute instances. With room for one instance,
	// a 2-node group is refused by the Create call itself; a 1-node group runs.
	instUsed := e.byName(t, p, quota.ComputeInstanceCount).GetStatus().GetUsage()
	e.setLimit(t, p, quota.ComputeInstanceCount, instUsed+1)
	group := func(name string, n int64) *mk8spb.CreateNodeGroupRequest {
		return &mk8spb.CreateNodeGroupRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: clusterID, Name: name},
			Spec: &mk8spb.NodeGroupSpec{
				Size: &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: n},
				Template: &mk8spb.NodeTemplate{
					Resources: &mk8spb.ResourcesSpec{Platform: "cpu-d3", Size: &mk8spb.ResourcesSpec_Preset{Preset: "2vcpu-8gb"}},
				},
			},
		}
	}
	_, err = groups.Create(ctx, group("two", 2))
	if v := quotaFailure(t, err).Violations(); v[0].GetQuota() != quota.ComputeInstanceCount || v[0].GetRequested() != "2" {
		t.Fatalf("node group violations = %v", v)
	}
	if _, err := groups.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: clusterID, Name: "two"}); status.Code(err) != codes.NotFound {
		t.Fatalf("refused node group exists: %v", err)
	}
	wait(t)(groups.Create(ctx, group("one", 1)))
	if got := e.byName(t, p, quota.ComputeInstanceCount).GetStatus().GetUsage(); got != instUsed+1 {
		t.Fatalf("instances = %d, want %d (the node)", got, instUsed+1)
	}
	if got := e.byName(t, p, quota.ComputeInstanceNonGPUVCPU).GetStatus().GetUsage(); got < 2 {
		t.Fatalf("vCPU usage = %d, want at least the node's 2", got)
	}
}
