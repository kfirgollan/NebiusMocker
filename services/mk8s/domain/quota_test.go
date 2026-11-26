package domain_test

import (
	"maps"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func TestCreateClusterQuota(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createCluster(t, "ok", "")
	claims := e.quota.claimsWith(quota.MK8sClusterCount)
	if len(claims) != 1 || claims[0].ContainerID != projectID || claims[0].Amounts[quota.MK8sClusterCount] != 1 || e.quota.applied != 1 {
		t.Fatalf("cluster claims = %v (applied %d), want one for 1 cluster in %s", claims, e.quota.applied, projectID)
	}

	e.quota.refuse = quota.MK8sClusterCount
	_, err := e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "refused"},
		Spec:     &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{}},
	})
	if !errs.IsKind(err, errs.KindQuotaExceeded) {
		t.Fatalf("CreateCluster over quota: err = %v, want quota_exceeded", err)
	}
	if _, err := e.svc.GetClusterByName(t.Context(), projectID, "refused"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("refused cluster was stored: %v", err)
	}
}

func TestCreateNodeGroupQuotaClaim(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	spec := fixedSpec(3)
	spec.Template.Resources = preset("gpu-h100-sxm", "8gpu-128vcpu-1600gb")
	spec.Template.BootDisk = &mk8spb.DiskSpec{Type: mk8spb.DiskSpec_NETWORK_SSD, Size: &mk8spb.DiskSpec_SizeGibibytes{SizeGibibytes: 100}}
	done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "gpus", spec))

	claims := e.quota.claimsWith(quota.ComputeInstanceCount)
	if len(claims) != 1 {
		t.Fatalf("node claims = %v, want one", claims)
	}
	want := map[string]int64{
		quota.ComputeInstanceCount:               3,
		quota.ComputeInstanceGPU("gpu-h100-sxm"): 24,
		quota.ComputeDiskCount:                   3,
		quota.ComputeDiskSize("NETWORK_SSD"):     3 * 100 << 30,
	}
	if !maps.Equal(claims[0].Amounts, want) || claims[0].ContainerID != projectID {
		t.Errorf("claim = %v in %s, want %v", claims[0].Amounts, claims[0].ContainerID, want)
	}
}

func TestCreateNodeGroupQuotaPreemptible(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	spec := fixedSpec(2)
	spec.Template.Preemptible = &mk8spb.PreemptibleSpec{}
	done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "spot", spec))
	claims := e.quota.claimsWith(quota.ComputeInstancePreemptibleCount)
	if len(claims) != 1 || claims[0].Amounts[quota.ComputeInstanceNonGPUVCPU] != 4 || claims[0].Amounts[quota.ComputeInstanceCount] != 0 {
		t.Fatalf("claims = %v, want 2 preemptible instances with 4 vCPUs", claims)
	}
}

func TestCreateNodeGroupQuotaRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	e.quota.refuse = quota.ComputeInstanceCount
	_, err := e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(2))
	if !errs.IsKind(err, errs.KindQuotaExceeded) {
		t.Fatalf("CreateNodeGroup over quota: err = %v, want quota_exceeded", err)
	}
	if _, err := e.svc.GetNodeGroupByName(t.Context(), c.GetMetadata().GetId(), "g"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("refused node group was stored: %v", err)
	}
}

func TestUpdateNodeGroupQuotaScaleUp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(2)))

	done(t)(e.update(t.Context(), e.group(t, id), fixedSpec(5)))
	claims := e.quota.claimsWith(quota.ComputeInstanceCount)
	if len(claims) != 2 || claims[1].Amounts[quota.ComputeInstanceCount] != 3 || claims[1].Amounts[quota.ComputeInstanceNonGPUVCPU] != 6 {
		t.Fatalf("claims = %v, want the create (2) then only the 3 added nodes", claims)
	}

	// Scaling in claims nothing.
	done(t)(e.update(t.Context(), e.group(t, id), fixedSpec(1)))
	if n := len(e.quota.claimsWith(quota.ComputeInstanceCount)); n != 2 {
		t.Errorf("scale-in made a claim: %d claims", n)
	}

	// A refused scale-up leaves the group untouched.
	e.quota.refuse = quota.ComputeInstanceCount
	_, err := e.update(t.Context(), e.group(t, id), fixedSpec(4))
	if !errs.IsKind(err, errs.KindQuotaExceeded) {
		t.Fatalf("scale-up over quota: err = %v, want quota_exceeded", err)
	}
	if g := e.group(t, id); g.GetSpec().GetFixedNodeCount() != 1 || g.GetStatus().GetNodeCount() != 1 {
		t.Errorf("refused update changed the group: %v", g)
	}
}

func TestNodeGroupCreateNodeQuotaFailsOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	e.compute.createErr = errs.QuotaExceeded("compute quota", errs.QuotaViolation{Quota: quota.ComputeInstanceCount})
	op, err := e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(1))
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	if !errs.IsKind(op.Err, errs.KindQuotaExceeded) {
		t.Fatalf("operation error = %v, want quota_exceeded from compute", op.Err)
	}
}
