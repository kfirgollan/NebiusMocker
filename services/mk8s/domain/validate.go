package domain

import (
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Node template defaults (node_group.proto NON_EMPTY_DEFAULT fields).
const (
	defaultBootDiskGiB = 128
	// Deployment strategy defaults from Q3 2026 (node_group.proto
	// NodeGroupDeploymentStrategy).
	defaultMaxUnavailable = 1
	defaultMaxSurge       = 0
	defaultDrainTimeout   = 10 * time.Minute
)

func notImplemented(field, what string) errs.Violation {
	return errs.Violation{Field: field, Message: what + " is not implemented in NebiusMocker yet"}
}

func violations(v []errs.Violation) error {
	if len(v) == 0 {
		return nil
	}
	return errs.BadRequest(v...)
}

// checkClusterSupported rejects the control-plane features the mock cannot
// honor.
func checkClusterSupported(cp *mk8spb.ControlPlaneSpec) error {
	var v []errs.Violation
	if cp.GetAuditLogs() != nil {
		v = append(v, notImplemented("spec.control_plane.audit_logs", "pushing Kubernetes audit logs"))
	}
	if cp.GetKarpenter() != nil {
		v = append(v, notImplemented("spec.control_plane.karpenter", "installing Karpenter"))
	}
	if len(cp.GetAuthentication().GetOidc()) > 0 {
		v = append(v, notImplemented("spec.control_plane.authentication.oidc", "OIDC authentication"))
	}
	return violations(v)
}

// checkNodeGroupSupported rejects the node group features the mock cannot
// honor.
func checkNodeGroupSupported(spec *mk8spb.NodeGroupSpec) error {
	const p = "spec.template."
	t := spec.GetTemplate()
	var v []errs.Violation
	add := func(cond bool, field, what string) {
		if cond {
			v = append(v, notImplemented(field, what))
		}
	}
	rp := t.GetReservationPolicy()
	add(t.GetNvlink() != nil, p+"nvlink", "NVLink")
	add(rp.GetPolicy() != mk8spb.ReservationPolicy_AUTO || len(rp.GetReservationIds()) > 0, p+"reservation_policy", "capacity block reservations")
	add(t.GetLocalDisks() != nil, p+"local_disks", "local disks")
	add(t.GetSpotPricingPolicy() != nil, p+"spot_pricing_policy", "spot pricing")
	add(t.GetFollowsSpotPrice() != nil, p+"follows_spot_price", "spot pricing")
	add(t.GetContainerRuntime().GetSociPullMode() != mk8spb.ContainerRuntime_SOCI_PULL_MODE_UNSPECIFIED,
		p+"container_runtime.soci_pull_mode", "the SOCI snapshotter")
	add(t.GetGpuSettings().GetDra(), p+"gpu_settings.dra", "Dynamic Resource Allocation")
	add(len(spec.GetAutoRepair().GetConditions()) > 0, "spec.auto_repair.conditions", "custom auto-repair conditions")
	return violations(v)
}

// effectiveStrategy is NodeGroupStatus.strategy: the spec's values over the
// service defaults. Both max_unavailable and max_surge 0 is rejected.
func effectiveStrategy(st *mk8spb.NodeGroupDeploymentStrategy) (*mk8spb.NodeGroupDeploymentStrategy, error) {
	out := &mk8spb.NodeGroupDeploymentStrategy{
		MaxUnavailable: count(defaultMaxUnavailable),
		MaxSurge:       count(defaultMaxSurge),
		DrainTimeout:   durationpb.New(defaultDrainTimeout),
	}
	if st.GetMaxUnavailable() != nil {
		out.MaxUnavailable = proto.CloneOf(st.GetMaxUnavailable())
	}
	if st.GetMaxSurge() != nil {
		out.MaxSurge = proto.CloneOf(st.GetMaxSurge())
	}
	if st.GetDrainTimeout() != nil {
		out.DrainTimeout = proto.CloneOf(st.GetDrainTimeout())
	}
	if isZero(out.GetMaxUnavailable()) && isZero(out.GetMaxSurge()) {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.strategy", Message: "max_unavailable and max_surge cannot both be 0"})
	}
	return out, nil
}

func count(n int64) *mk8spb.PercentOrCount {
	return &mk8spb.PercentOrCount{Value: &mk8spb.PercentOrCount_Count{Count: n}}
}

func isZero(p *mk8spb.PercentOrCount) bool { return p.GetCount() == 0 && p.GetPercent() == 0 }

// normalizeTemplate fills the NON_EMPTY_DEFAULT template fields in place:
// the boot disk, and the network interface subnet (the cluster's).
func normalizeTemplate(t *mk8spb.NodeTemplate, clusterSubnet string) {
	if t.GetBootDisk() == nil {
		t.BootDisk = &mk8spb.DiskSpec{Size: &mk8spb.DiskSpec_SizeGibibytes{SizeGibibytes: defaultBootDiskGiB}}
	}
	if t.GetBootDisk().GetType() == mk8spb.DiskSpec_UNSPECIFIED {
		t.BootDisk.Type = mk8spb.DiskSpec_NETWORK_SSD
	}
	if len(t.GetNetworkInterfaces()) == 0 {
		t.NetworkInterfaces = []*mk8spb.NetworkInterfaceTemplate{{}}
	}
	for _, nic := range t.GetNetworkInterfaces() {
		if nic.GetSubnetId() == "" {
			nic.SubnetId = clusterSubnet
		}
	}
}

// targetOnCreate is the node count a new node group starts with: the fixed
// count, or autoscaling initial_node_count (defaults to min_node_count).
func targetOnCreate(spec *mk8spb.NodeGroupSpec) int64 {
	if as := spec.GetAutoscaling(); as != nil {
		if as.InitialNodeCount != nil {
			return as.GetInitialNodeCount()
		}
		return as.GetMinNodeCount()
	}
	return spec.GetFixedNodeCount()
}

// targetOnUpdate is the node count after an update: the fixed count, or the
// current count within the autoscaling bounds (initial_node_count has no
// effect after creation, node_group.proto).
func targetOnUpdate(spec *mk8spb.NodeGroupSpec, current int64) int64 {
	if as := spec.GetAutoscaling(); as != nil {
		return min(max(current, as.GetMinNodeCount()), as.GetMaxNodeCount())
	}
	return spec.GetFixedNodeCount()
}

// templateNeedsRollout reports whether a template change recreates the
// nodes: every field does except taints and Kubernetes node labels
// (node_group.proto: "Field change will NOT trigger NodeGroup roll out").
func templateNeedsRollout(prev, next *mk8spb.NodeTemplate) bool {
	a, b := proto.CloneOf(prev), proto.CloneOf(next)
	for _, t := range []*mk8spb.NodeTemplate{a, b} {
		if t != nil {
			t.Taints, t.Metadata = nil, nil
		}
	}
	return !proto.Equal(a, b)
}
