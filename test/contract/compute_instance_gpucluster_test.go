package contract

import (
	"slices"
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
)

func TestInstanceGPUCluster(t *testing.T) {
	t.Parallel()
	c := startInstances(t)
	ctx := t.Context()
	clusters := computesvc.NewGpuClusterService(c.env.Inst.SDK(t))

	op, err := clusters.Create(ctx, &computepb.CreateGpuClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: "ib"},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	clusterID := op.ResourceID()
	members := func() []string {
		gc, err := clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: clusterID})
		if err != nil {
			t.Fatal(err)
		}
		return gc.GetStatus().GetInstances()
	}

	spec := c.gpuSpec("g")
	spec.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: clusterID}
	inst := c.mustCreate(t, "g", spec)
	id := inst.GetMetadata().GetId()
	protoEqual(t, "spec echo", inst.GetSpec(), spec)
	if len(inst.GetStatus().GetInfinibandTopologyPath().GetPath()) == 0 {
		t.Errorf("no infiniband topology path: %v", inst.GetStatus())
	}
	if got := members(); !slices.Equal(got, []string{id}) {
		t.Errorf("cluster instances = %v, want [%s]", got, id)
	}

	delOp, err := clusters.Delete(ctx, &computepb.DeleteGpuClusterRequest{Id: clusterID})
	if err == nil {
		_, err = delOp.Wait(ctx)
	}
	wantCode(t, "delete cluster with members", err, codes.FailedPrecondition, "")

	c.mustDelete(t, id)
	if got := members(); len(got) != 0 {
		t.Errorf("cluster instances after delete = %v", got)
	}
	delOp, err = clusters.Delete(ctx, &computepb.DeleteGpuClusterRequest{Id: clusterID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delOp.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// A fresh cluster rejects instances that cannot be clustered.
	op, err = clusters.Create(ctx, &computepb.CreateGpuClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: "ib2"},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	one := c.gpuSpec("one")
	one.Resources = preset("gpu-h100-sxm", "1gpu-16vcpu-200gb")
	one.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: op.ResourceID()}
	_, err = c.create(ctx, "one", one)
	wantCode(t, "1gpu preset with a cluster", err, codes.InvalidArgument, "")
}
