package domain_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func TestCreateNodeGroupFixed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	spec := fixedSpec(2)
	spec.Template.NetworkInterfaces = []*mk8spb.NetworkInterfaceTemplate{{PublicIpAddress: &mk8spb.PublicIPAddress{}}}
	spec.Template.Metadata = &mk8spb.NodeMetadataTemplate{Labels: map[string]string{"pool": "cpu", "node.kubernetes.io/role": "x"}}
	spec.Template.InstanceMetadata = &mk8spb.InstanceMetadataTemplate{Labels: map[string]string{"team": "ml", domain.LabelNodeGroupName: "spoof"}}
	spec.Template.Taints = []*mk8spb.NodeTaint{{Key: "gpu", Value: "true", Effect: mk8spb.NodeTaint_NO_SCHEDULE}}
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "Web_Pool.1", spec))
	g := e.group(t, id)

	st := g.GetStatus()
	if st.GetState() != mk8spb.NodeGroupStatus_RUNNING || st.GetNodeCount() != 2 || st.GetReadyNodeCount() != 2 ||
		st.GetTargetNodeCount() != 2 || st.GetReconciling() || st.GetVersion() != "1.36.0-nebius-node.1" {
		t.Fatalf("status = %v, want RUNNING 2/2 on 1.36.0-nebius-node.1", st)
	}
	strategy := st.GetStrategy()
	if strategy.GetMaxUnavailable().GetCount() != 1 || strategy.GetMaxSurge().GetCount() != 0 || strategy.GetDrainTimeout().AsDuration().Minutes() != 10 {
		t.Errorf("status.strategy = %v, want the Q3-2026 defaults", strategy)
	}
	tmpl := g.GetSpec().GetTemplate()
	if tmpl.GetBootDisk().GetSizeGibibytes() != 128 || tmpl.GetBootDisk().GetType() != mk8spb.DiskSpec_NETWORK_SSD {
		t.Errorf("boot disk = %v, want the 128 GiB NETWORK_SSD default", tmpl.GetBootDisk())
	}
	if tmpl.GetNetworkInterfaces()[0].GetSubnetId() != defaultSubnet {
		t.Errorf("node subnet = %q, want the cluster's", tmpl.GetNetworkInterfaces()[0].GetSubnetId())
	}

	ids := e.compute.instanceIDs(id)
	if len(ids) != 2 {
		t.Fatalf("instances = %v, want 2", ids)
	}
	ns := e.compute.spec(ids[0])
	if !strings.HasPrefix(ns.Name, "web-pool-1-") || len(ns.Name) != len("web-pool-1-")+5 {
		t.Errorf("node name = %q, want web-pool-1-<5 chars>", ns.Name)
	}
	if ns.ProjectID != projectID || ns.ClusterID != c.GetMetadata().GetId() || ns.Template.ImageFamily != "ubuntu24.04-driverless" ||
		!ns.Template.PublicIP || ns.Template.SubnetID != defaultSubnet || ns.Template.BootDisk.SizeBytes != 128<<30 {
		t.Errorf("node spec = %+v", ns)
	}
	if ns.Labels["team"] != "ml" || ns.Labels[domain.LabelNodeGroupName] != "Web_Pool.1" || ns.Labels[domain.LabelNodeGroupID] != id {
		t.Errorf("instance labels = %v, want the template's plus the service's (which win)", ns.Labels)
	}
	bg := e.backend.group(id)
	if got := sortedKeys(bg.Labels); !slices.Equal(got, []string{domain.LabelClusterID, domain.LabelNodeGroupID, domain.LabelNodeGroupName, "pool"}) {
		t.Errorf("node labels = %v, want pool plus the service labels (no kubernetes.io)", got)
	}
	if len(bg.Taints) != 1 || bg.Taints[0].Effect != "NoSchedule" || bg.Version != "1.36" || len(bg.Nodes) != 2 || bg.Nodes[0].PublicIP == "" {
		t.Errorf("backend group = %+v", bg)
	}
}

func TestCreateNodeGroupAutoscaling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		as       *mk8spb.NodeGroupAutoscalingSpec
		wantSize int64
	}{
		{"initial", &mk8spb.NodeGroupAutoscalingSpec{MinNodeCount: 1, MaxNodeCount: 4, InitialNodeCount: new(int64(3))}, 3},
		{"min by default", &mk8spb.NodeGroupAutoscalingSpec{MinNodeCount: 2, MaxNodeCount: 4}, 2},
		{"zero", &mk8spb.NodeGroupAutoscalingSpec{MaxNodeCount: 4}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c := e.createCluster(t, "c", "")
			spec := fixedSpec(0)
			spec.Size = &mk8spb.NodeGroupSpec_Autoscaling{Autoscaling: tt.as}
			id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", spec))
			if st := e.group(t, id).GetStatus(); st.GetNodeCount() != tt.wantSize || st.GetTargetNodeCount() != tt.wantSize {
				t.Errorf("status = %v, want %d nodes", st, tt.wantSize)
			}
		})
	}
}

func TestNodeGroupVersionRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, cluster, version string
		wantStatus             string // "" means rejected on spec.version
	}{
		{"default is the cluster's", "1.35", "", "1.35.0-nebius-node.1"},
		{"same minor", "1.36", "1.36", "1.36.0-nebius-node.1"},
		{"pinned image", "1.36", "1.36-nebius-node.1", "1.36.0-nebius-node.1"},
		{"one behind", "1.36", "1.35", "1.35.0-nebius-node.1"},
		{"two behind", "1.36", "1.34", "1.34.0-nebius-node.1"},
		{"three behind", "1.36", "1.33", ""},
		{"newer than the cluster", "1.35", "1.36", ""},
		{"not in catalog", "1.36", "1.37", ""},
		{"image that does not exist", "1.36", "1.36-nebius-node.2", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, "1.33", "1.34", "1.35", "1.36")
			c := e.createCluster(t, "c", tt.cluster)
			spec := fixedSpec(1)
			spec.Version = tt.version
			op, err := e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", spec)
			if tt.wantStatus == "" {
				assertErr(t, err, errs.KindBadRequest, "spec.version")
				return
			}
			id := done(t)(op, err)
			if got := e.group(t, id).GetStatus().GetVersion(); got != tt.wantStatus {
				t.Errorf("status.version = %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

func TestCreateNodeGroupRejections(t *testing.T) {
	t.Parallel()
	edit := func(f func(*mk8spb.NodeGroupSpec)) *mk8spb.NodeGroupSpec {
		s := fixedSpec(1)
		f(s)
		return s
	}
	tests := []struct {
		name      string
		spec      *mk8spb.NodeGroupSpec
		wantField string
	}{
		{"nvlink", edit(func(s *mk8spb.NodeGroupSpec) {
			s.Template.Nvlink = &mk8spb.NVLinkSpec{NvlGroup: &mk8spb.NVLinkSpec_NvlInstanceGroupId{NvlInstanceGroupId: "computenvlinstancegroup-1"}}
		}), "spec.template.nvlink"},
		{"reservation policy", edit(func(s *mk8spb.NodeGroupSpec) {
			s.Template.ReservationPolicy = &mk8spb.ReservationPolicy{Policy: mk8spb.ReservationPolicy_STRICT}
		}), "spec.template.reservation_policy"},
		{"local disks", edit(func(s *mk8spb.NodeGroupSpec) { s.Template.LocalDisks = &mk8spb.LocalDisksSpec{} }), "spec.template.local_disks"},
		{"spot pricing", edit(func(s *mk8spb.NodeGroupSpec) {
			s.Template.PricingModel = &mk8spb.NodeTemplate_SpotPricingPolicy{SpotPricingPolicy: &mk8spb.SpotPricingPolicySpec{Id: "p"}}
		}), "spec.template.spot_pricing_policy"},
		{"follows spot price", edit(func(s *mk8spb.NodeGroupSpec) {
			s.Template.PricingModel = &mk8spb.NodeTemplate_FollowsSpotPrice{FollowsSpotPrice: &mk8spb.FollowsSpotPriceSpec{}}
		}), "spec.template.follows_spot_price"},
		{"soci", edit(func(s *mk8spb.NodeGroupSpec) {
			s.Template.ContainerRuntime = &mk8spb.ContainerRuntime{SociPullMode: mk8spb.ContainerRuntime_LAZY}
		}), "spec.template.container_runtime.soci_pull_mode"},
		{"max pods", edit(func(s *mk8spb.NodeGroupSpec) { s.Template.MaxPods = 64 }), "spec.template.max_pods"},
		{"dra", edit(func(s *mk8spb.NodeGroupSpec) { s.Template.GpuSettings = &mk8spb.GpuSettings{Dra: true} }), "spec.template.gpu_settings.dra"},
		{"auto repair conditions", edit(func(s *mk8spb.NodeGroupSpec) {
			s.AutoRepair = &mk8spb.NodeGroupAutoRepairSpec{Conditions: []*mk8spb.NodeAutoRepairCondition{{Type: "Ready"}}}
		}), "spec.auto_repair.conditions"},
		{"zero unavailable and surge", edit(func(s *mk8spb.NodeGroupSpec) {
			s.Strategy = &mk8spb.NodeGroupDeploymentStrategy{MaxUnavailable: &mk8spb.PercentOrCount{Value: &mk8spb.PercentOrCount_Percent{}}}
		}), "spec.strategy"},
		{"unknown os", edit(func(s *mk8spb.NodeGroupSpec) { s.Template.Os = "debian12" }), "spec.template.os"},
		{
			"cuda on cpu", edit(func(s *mk8spb.NodeGroupSpec) { s.Template.GpuSettings = &mk8spb.GpuSettings{DriversPreset: "cuda12"} }),
			"spec.template.gpu_settings.drivers_preset",
		},
		{
			"platform without images", edit(func(s *mk8spb.NodeGroupSpec) { s.Template.Resources = preset("gpu-l40s-d", "1gpu-16vcpu-96gb") }),
			"spec.template.resources.platform",
		},
		{
			"compute preset check", edit(func(s *mk8spb.NodeGroupSpec) { s.Template.Resources = preset("cpu-d3", "1vcpu-1gb") }),
			"spec.template.resources.preset",
		},
		{"subnet in another network", edit(func(s *mk8spb.NodeGroupSpec) {
			s.Template.NetworkInterfaces = []*mk8spb.NetworkInterfaceTemplate{{SubnetId: otherNetSubnet}}
		}), "spec.template.network_interfaces[0].subnet_id"},
		{"missing subnet", edit(func(s *mk8spb.NodeGroupSpec) {
			s.Template.NetworkInterfaces = []*mk8spb.NetworkInterfaceTemplate{{SubnetId: "vpcsubnet-e00nope"}}
		}), "spec.template.network_interfaces[0].subnet_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c := e.createCluster(t, "c", "")
			_, err := e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", tt.spec)
			assertErr(t, err, errs.KindBadRequest, tt.wantField)
			if e.groups.len() != 0 || e.compute.total() != 0 {
				t.Error("a rejected node group left state behind")
			}
		})
	}
}

func TestCreateNodeGroupHonorsSupportedSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	spec := fixedSpec(1)
	spec.Template.Resources = preset("gpu-h100-sxm", "8gpu-128vcpu-1600gb")
	spec.Template.GpuSettings = &mk8spb.GpuSettings{DriversPreset: "cuda12"}
	spec.Template.Os = "ubuntu22.04"
	spec.Template.NetworkInterfaces = []*mk8spb.NetworkInterfaceTemplate{{SubnetId: sameNetSubnet}}
	spec.Template.Preemptible = &mk8spb.PreemptibleSpec{}
	spec.Template.ReservationPolicy = &mk8spb.ReservationPolicy{} // AUTO with no reservations: the default
	spec.Strategy = &mk8spb.NodeGroupDeploymentStrategy{MaxSurge: &mk8spb.PercentOrCount{Value: &mk8spb.PercentOrCount_Percent{Percent: 25}}, DrainTimeout: durationpb.New(0)}
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "gpu", spec))
	ns := e.compute.spec(e.compute.instanceIDs(id)[0])
	if ns.Template.ImageFamily != "ubuntu22.04-cuda12" || !ns.Template.Preemptible || ns.Template.SubnetID != sameNetSubnet {
		t.Errorf("node template = %+v", ns.Template)
	}
	if gpus := e.backend.group(id).Nodes[0].GPUs; gpus != 8 {
		t.Errorf("backend node GPUs = %d, want 8", gpus)
	}
	st := e.group(t, id).GetStatus().GetStrategy()
	if st.GetMaxSurge().GetPercent() != 25 || st.GetMaxUnavailable().GetCount() != 1 || st.GetDrainTimeout().AsDuration() != 0 {
		t.Errorf("status.strategy = %v, want the spec over the defaults", st)
	}
}

func TestCreateNodeGroupClusterChecks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.createGroup(t.Context(), "mk8scluster-e00nope", "g", fixedSpec(1))
	assertErr(t, err, errs.KindNotFound, "")
	c := e.createCluster(t, "c", "")
	done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(1)))
	_, err = e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(1))
	assertErr(t, err, errs.KindAlreadyExists, "")
}

func TestNodeGroupNotReady(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.backend.notReady = "host capacity: NEBIUS_MOCKER_MK8S_MAX_AGENTS reached"
	c := e.createCluster(t, "c", "")
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(2)))
	st := e.group(t, id).GetStatus()
	if st.GetNodeCount() != 2 || st.GetReadyNodeCount() != 0 || !st.GetReconciling() || st.GetState() != mk8spb.NodeGroupStatus_RUNNING {
		t.Fatalf("status = %v, want 2 nodes, 0 ready, reconciling", st)
	}
	if len(st.GetEvents()) != 1 || st.GetEvents()[0].GetLastOccurrence().GetCode() != "NodesNotReady" ||
		!strings.Contains(st.GetEvents()[0].GetLastOccurrence().GetMessage(), "MAX_AGENTS") {
		t.Errorf("events = %v, want one NodesNotReady with the reason", st.GetEvents())
	}
}

func TestUpdateNodeGroupScaling(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(3)))
	first := e.compute.instanceIDs(id)

	g := e.group(t, id)
	done(t)(e.update(t.Context(), g, fixedSpec(1)))
	if got := e.compute.instanceIDs(id); !slices.Equal(got, first[:1]) {
		t.Errorf("instances after scale-in = %v, want the oldest %v kept", got, first[:1])
	}
	g = e.group(t, id)
	if g.GetMetadata().GetResourceVersion() != 2 || g.GetStatus().GetNodeCount() != 1 {
		t.Errorf("after scale-in: %v", g)
	}

	// Switching to autoscaling starts from the current count, clamped.
	auto := fixedSpec(0)
	auto.Size = &mk8spb.NodeGroupSpec_Autoscaling{Autoscaling: &mk8spb.NodeGroupAutoscalingSpec{MinNodeCount: 2, MaxNodeCount: 5, InitialNodeCount: new(int64(4))}}
	done(t)(e.update(t.Context(), g, auto))
	if st := e.group(t, id).GetStatus(); st.GetNodeCount() != 2 || st.GetTargetNodeCount() != 2 {
		t.Errorf("after switching to autoscaling: %v, want 2 (current 1 clamped to min; initial ignored)", st)
	}
	if got := e.compute.instanceIDs(id); got[0] != first[0] {
		t.Error("scaling recreated an existing node")
	}

	// An identical update changes nothing.
	g = e.group(t, id)
	done(t)(e.update(t.Context(), g, auto))
	if again := e.group(t, id); !proto.Equal(again, g) {
		t.Errorf("no-op update changed the node group:\n%v\n%v", g, again)
	}
}

func TestUpdateNodeGroupRollout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		edit        func(*mk8spb.NodeGroupSpec)
		wantRollout bool
	}{
		{"taints", func(s *mk8spb.NodeGroupSpec) {
			s.Template.Taints = []*mk8spb.NodeTaint{{Key: "k", Value: "v", Effect: mk8spb.NodeTaint_NO_EXECUTE}}
		}, false},
		{"node labels", func(s *mk8spb.NodeGroupSpec) {
			s.Template.Metadata = &mk8spb.NodeMetadataTemplate{Labels: map[string]string{"a": "b"}}
		}, false},
		{"preset", func(s *mk8spb.NodeGroupSpec) { s.Template.Resources = preset("cpu-d3", "4vcpu-16gb") }, true},
		{"instance labels", func(s *mk8spb.NodeGroupSpec) {
			s.Template.InstanceMetadata = &mk8spb.InstanceMetadataTemplate{Labels: map[string]string{"a": "b"}}
		}, true},
		{"version", func(s *mk8spb.NodeGroupSpec) { s.Version = "1.35" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c := e.createCluster(t, "c", "")
			id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(2)))
			before := e.compute.instanceIDs(id)
			g := e.group(t, id)
			spec := proto.CloneOf(g.GetSpec())
			tt.edit(spec)
			done(t)(e.update(t.Context(), g, spec))
			after := e.compute.instanceIDs(id)
			if len(after) != 2 || slices.Equal(before, after) != !tt.wantRollout {
				t.Errorf("instances %v → %v, want rollout %v", before, after, tt.wantRollout)
			}
			if st := e.group(t, id).GetStatus(); st.GetOutdatedNodeCount() != 0 || st.GetReadyNodeCount() != 2 {
				t.Errorf("status after update = %v", st)
			}
		})
	}
}

func TestUpdateNodeGroupRejections(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "1.35")
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(1)))
	g := e.group(t, id)
	newer := fixedSpec(1)
	newer.Version = "1.36"
	_, err := e.update(t.Context(), g, newer)
	assertErr(t, err, errs.KindBadRequest, "spec.version")
	karp := fixedSpec(1)
	karp.Template.MaxPods = 32
	_, err = e.update(t.Context(), g, karp)
	assertErr(t, err, errs.KindBadRequest, "spec.template.max_pods")
	moved := proto.CloneOf(g.GetMetadata())
	moved.ParentId = "mk8scluster-e00other"
	_, err = e.svc.UpdateNodeGroup(t.Context(), domain.UpdateNodeGroupCmd{Metadata: moved, Spec: fixedSpec(1)})
	assertErr(t, err, errs.KindBadRequest, "metadata.parent_id")
}

func TestUpgradeAndDeleteNodeGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(2)))
	before := e.compute.instanceIDs(id)
	done(t)(e.svc.UpgradeNodeGroup(t.Context(), id, nil))
	if got := e.compute.instanceIDs(id); !slices.Equal(got, before) {
		t.Errorf("upgrading a node group already on the latest image recreated nodes: %v → %v", before, got)
	}
	done(t)(e.svc.DeleteNodeGroup(t.Context(), id, nil))
	if _, err := e.svc.GetNodeGroup(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetNodeGroup after delete: %v, want not_found", err)
	}
	if e.compute.total() != 0 || len(e.backend.group(id).Nodes) != 0 {
		t.Error("deleting the node group left nodes")
	}
}
