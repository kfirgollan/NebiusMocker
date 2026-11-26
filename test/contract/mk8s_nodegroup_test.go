package contract

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func cpuTemplate(preset string) *mk8spb.NodeTemplate {
	return &mk8spb.NodeTemplate{Resources: &mk8spb.ResourcesSpec{Platform: "cpu-d3", Size: &mk8spb.ResourcesSpec_Preset{Preset: preset}}}
}

func fixedGroup(n int64) *mk8spb.NodeGroupSpec {
	return &mk8spb.NodeGroupSpec{Size: &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: n}, Template: cpuTemplate("2vcpu-8gb")}
}

func (e *mk8sEnv) groupReq(clusterID, name string, spec *mk8spb.NodeGroupSpec) *mk8spb.CreateNodeGroupRequest {
	return &mk8spb.CreateNodeGroupRequest{Metadata: &commonpb.ResourceMetadata{ParentId: clusterID, Name: name}, Spec: spec}
}

func (e *mk8sEnv) newGroup(t *testing.T, clusterID, name string, spec *mk8spb.NodeGroupSpec) string {
	t.Helper()
	return opWait(t)(e.groups.Create(t.Context(), e.groupReq(clusterID, name, spec)))
}

func (e *mk8sEnv) getGroup(t *testing.T, id string) *mk8spb.NodeGroup {
	t.Helper()
	g, err := e.groups.Get(t.Context(), &mk8spb.GetNodeGroupRequest{Id: id})
	if err != nil {
		t.Fatalf("get node group %s: %v", id, err)
	}
	return g
}

func TestMK8sNodeGroupReadRPCs(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	cid := e.newCluster(t, "c")
	ids := map[string]string{}
	for _, n := range []string{"a", "b", "c"} {
		ids[n] = e.newGroup(t, cid, n, fixedGroup(1))
	}
	g := e.getGroup(t, ids["b"])
	if g.GetMetadata().GetName() != "b" || g.GetMetadata().GetParentId() != cid {
		t.Errorf("Get = %v", g.GetMetadata())
	}
	// Defaults are filled into the stored spec.
	bd := g.GetSpec().GetTemplate().GetBootDisk()
	if bd.GetSizeGibibytes() != 128 || bd.GetType() != mk8spb.DiskSpec_NETWORK_SSD {
		t.Errorf("boot disk = %v, want 128 GiB NETWORK_SSD", bd)
	}
	by, err := e.groups.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: cid, Name: "c"})
	if err != nil || by.GetMetadata().GetId() != ids["c"] {
		t.Errorf("GetByName = %v, %v", by, err)
	}
	_, err = e.groups.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: cid, Name: "zzz"})
	wantFailure(t, "GetByName unknown", err, codes.NotFound)
	_, err = e.groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: "mk8snodegroup-e00unknown000000000"})
	wantFailure(t, "Get unknown", err, codes.NotFound)

	var names []string
	token := ""
	for {
		res, err := e.groups.List(ctx, &mk8spb.ListNodeGroupsRequest{ParentId: cid, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, it := range res.GetItems() {
			names = append(names, it.GetMetadata().GetName())
		}
		if token = res.GetNextPageToken(); token == "" {
			break
		}
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Errorf("List = %v, want a,b,c", names)
	}
	other := e.newCluster(t, "other")
	res, err := e.groups.List(ctx, &mk8spb.ListNodeGroupsRequest{ParentId: other})
	if err != nil || len(res.GetItems()) != 0 {
		t.Errorf("List of another cluster = %v, %v, want empty", res, err)
	}
}

func TestMK8sNodeGroupCreateErrors(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	cid := e.newCluster(t, "c")
	e.newGroup(t, cid, "taken", fixedGroup(0))

	_, err := e.groups.Create(ctx, e.groupReq(cid, "taken", fixedGroup(0)))
	wantFailure(t, "duplicate name", err, codes.AlreadyExists)
	_, err = e.groups.Create(ctx, e.groupReq("mk8scluster-e00unknown0000000000", "x", fixedGroup(0)))
	wantFailure(t, "unknown cluster", err, codes.NotFound)

	tests := []struct {
		name string
		edit func(*mk8spb.NodeGroupSpec)
		want string
	}{
		{"bad preset", func(s *mk8spb.NodeGroupSpec) {
			s.Template.Resources.Size = &mk8spb.ResourcesSpec_Preset{Preset: "999vcpu"}
		}, "spec.template.resources"},
		{"bad platform", func(s *mk8spb.NodeGroupSpec) { s.Template.Resources.Platform = "cpu-nope" }, "spec.template.resources"},
		{"nvlink", func(s *mk8spb.NodeGroupSpec) { s.Template.Nvlink = &mk8spb.NVLinkSpec{} }, "spec.template.nvlink"},
		{"local disks", func(s *mk8spb.NodeGroupSpec) { s.Template.LocalDisks = &mk8spb.LocalDisksSpec{} }, "spec.template.local_disks"},
		{"max pods", func(s *mk8spb.NodeGroupSpec) { s.Template.MaxPods = 50 }, "spec.template.max_pods"},
		{"version too new", func(s *mk8spb.NodeGroupSpec) { s.Version = "1.40" }, "spec.version"},
		{"unknown subnet", func(s *mk8spb.NodeGroupSpec) {
			s.Template.NetworkInterfaces = []*mk8spb.NetworkInterfaceTemplate{{SubnetId: "vpcsubnet-e00missing000000000"}}
		}, "subnet_id"},
	}
	for _, tt := range tests {
		spec := fixedGroup(1)
		tt.edit(spec)
		_, err := e.groups.Create(ctx, e.groupReq(cid, "g-"+strings.ReplaceAll(tt.name, " ", "-"), spec))
		if codeOf(err) != codes.InvalidArgument && codeOf(err) != codes.NotFound {
			t.Errorf("%s: code = %v, want InvalidArgument (err: %v)", tt.name, codeOf(err), err)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %q does not mention %q", tt.name, err, tt.want)
		}
	}
	// Nothing was created by the failed attempts.
	if n := len(e.nodesOfCluster(t, cid)); n != 0 {
		t.Errorf("instances after failed creates = %d, want 0", n)
	}
}

// nodesOfCluster lists the compute instances labeled with the cluster.
func (e *mk8sEnv) nodesOfCluster(t *testing.T, clusterID string) []*computepb.Instance {
	t.Helper()
	res, err := e.instances.List(t.Context(), &computepb.ListInstancesRequest{ParentId: e.project})
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	var out []*computepb.Instance
	for _, in := range res.GetItems() {
		if in.GetMetadata().GetLabels()["mk8s.nebius.ai/cluster-id"] == clusterID {
			out = append(out, in)
		}
	}
	return out
}

// A node group needs a RUNNING cluster.
func TestMK8sNodeGroupNeedsRunningCluster(t *testing.T) {
	t.Parallel()
	e := startMK8s(t, testkit.WithOperationDelay(400*time.Millisecond))
	op, err := e.clusters.Create(t.Context(), e.clusterReq("slow", nil))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = e.groups.Create(t.Context(), e.groupReq(op.ResourceID(), "early", fixedGroup(1)))
	wantFailure(t, "create in PROVISIONING cluster", err, codes.FailedPrecondition, "PROVISIONING")
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	e.newGroup(t, op.ResourceID(), "late", fixedGroup(1))
}

func TestMK8sNodeGroupScalingAndNodes(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	cid := e.newCluster(t, "c")

	// Autoscaling starts at min.
	auto := fixedGroup(0)
	auto.Size = &mk8spb.NodeGroupSpec_Autoscaling{Autoscaling: &mk8spb.NodeGroupAutoscalingSpec{MinNodeCount: 1, MaxNodeCount: 3}}
	aid := e.newGroup(t, cid, "auto", auto)
	if n := len(e.nodes(t, aid)); n != 1 {
		t.Errorf("autoscaling min=1 nodes = %d, want 1", n)
	}
	if st := e.getGroup(t, aid).GetStatus(); st.GetNodeCount() != 1 || st.GetTargetNodeCount() != 1 {
		t.Errorf("autoscaling status = %v", st)
	}

	// Fixed scaling up and down; every node has a private address, and no
	// public one unless asked for.
	gid := e.newGroup(t, cid, "fixed", fixedGroup(1))
	scaleTo(t, e, e.getGroup(t, gid), 3)
	nodes := e.nodes(t, gid)
	if len(nodes) != 3 {
		t.Fatalf("nodes after scaling to 3 = %d", len(nodes))
	}
	for _, n := range nodes {
		nics := n.GetStatus().GetNetworkInterfaces()
		if len(nics) != 1 || nics[0].GetIpAddress().GetAddress() == "" || nics[0].GetPublicIpAddress().GetAddress() != "" {
			t.Errorf("node %s interfaces = %v, want a private address only", n.GetMetadata().GetName(), nics)
		}
	}
	scaleTo(t, e, e.getGroup(t, gid), 1)
	if n := len(e.nodes(t, gid)); n != 1 {
		t.Errorf("nodes after scaling to 1 = %d", n)
	}
	scaleTo(t, e, e.getGroup(t, gid), 0)
	if n := len(e.nodes(t, gid)); n != 0 {
		t.Errorf("nodes after scaling to 0 = %d", n)
	}

	// Public IPs.
	pub := fixedGroup(2)
	pub.Template.NetworkInterfaces = []*mk8spb.NetworkInterfaceTemplate{{PublicIpAddress: &mk8spb.PublicIPAddress{}}}
	pid := e.newGroup(t, cid, "public", pub)
	for _, n := range e.nodes(t, pid) {
		nics := n.GetStatus().GetNetworkInterfaces()
		if len(nics) != 1 || nics[0].GetIpAddress().GetAddress() == "" || nics[0].GetPublicIpAddress().GetAddress() == "" {
			t.Errorf("public node %s interfaces = %v, want private and public addresses", n.GetMetadata().GetName(), nics)
		}
	}

	// The nodes are compute instances the group manages: the user cannot
	// change them.
	node := e.nodes(t, pid)[0]
	id := node.GetMetadata().GetId()
	_, err := e.instances.Delete(ctx, &computepb.DeleteInstanceRequest{Id: id})
	wantFailure(t, "instance Delete", err, codes.FailedPrecondition)
	_, err = e.instances.Stop(ctx, &computepb.StopInstanceRequest{Id: id})
	wantFailure(t, "instance Stop", err, codes.FailedPrecondition)
	node.Spec.Hostname = "renamed"
	_, err = e.instances.Update(ctx, &computepb.UpdateInstanceRequest{Metadata: node.GetMetadata(), Spec: node.GetSpec()})
	wantFailure(t, "instance Update", err, codes.FailedPrecondition)
	if n := len(e.nodes(t, pid)); n != 2 {
		t.Errorf("nodes after refused changes = %d, want 2", n)
	}
}

func TestMK8sNodeGroupUpdateUpgradeDelete(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	cid := e.newCluster(t, "c")
	gid := e.newGroup(t, cid, "g", fixedGroup(2))
	before := map[string]bool{}
	for _, n := range e.nodes(t, gid) {
		before[n.GetMetadata().GetId()] = true
	}

	// A template change rolls the nodes: they are recreated.
	g := e.getGroup(t, gid)
	spec := g.GetSpec()
	spec.Template.Resources.Size = &mk8spb.ResourcesSpec_Preset{Preset: "4vcpu-16gb"}
	opWait(t)(e.groups.Update(ctx, &mk8spb.UpdateNodeGroupRequest{Metadata: g.GetMetadata(), Spec: spec}))
	nodes := e.nodes(t, gid)
	if len(nodes) != 2 {
		t.Fatalf("nodes after rollout = %d, want 2", len(nodes))
	}
	for _, n := range nodes {
		if before[n.GetMetadata().GetId()] {
			t.Errorf("node %s was not replaced by the rollout", n.GetMetadata().GetName())
		}
		if n.GetSpec().GetResources().GetPreset() != "4vcpu-16gb" {
			t.Errorf("node %s preset = %q, want 4vcpu-16gb", n.GetMetadata().GetName(), n.GetSpec().GetResources().GetPreset())
		}
	}

	// Rejected updates.
	g = e.getGroup(t, gid)
	spec = g.GetSpec()
	spec.Template.MaxPods = 10
	_, err := e.groups.Update(ctx, &mk8spb.UpdateNodeGroupRequest{Metadata: g.GetMetadata(), Spec: spec})
	wantFailure(t, "max_pods update", err, codes.InvalidArgument, "max_pods")
	_, err = e.groups.Update(ctx, &mk8spb.UpdateNodeGroupRequest{Metadata: &commonpb.ResourceMetadata{Id: "mk8snodegroup-e00unknown000000000"}, Spec: fixedGroup(1)})
	wantFailure(t, "update unknown", err, codes.NotFound)

	// Upgrade to the latest node image.
	opWait(t)(e.groups.Upgrade(ctx, &mk8spb.UpgradeNodeGroupRequest{Id: gid, UpgradeType: &mk8spb.UpgradeNodeGroupRequest_LatestInfraVersion{LatestInfraVersion: &emptypb.Empty{}}}))
	if got := e.getGroup(t, gid).GetStatus().GetVersion(); got != "1.36.0-nebius-node.1" {
		t.Errorf("version after upgrade = %q", got)
	}
	_, err = e.groups.Upgrade(ctx, &mk8spb.UpgradeNodeGroupRequest{Id: "mk8snodegroup-e00unknown000000000", UpgradeType: &mk8spb.UpgradeNodeGroupRequest_LatestInfraVersion{LatestInfraVersion: &emptypb.Empty{}}})
	wantFailure(t, "upgrade unknown", err, codes.NotFound)

	opWait(t)(e.groups.Delete(ctx, &mk8spb.DeleteNodeGroupRequest{Id: gid}))
	_, err = e.groups.Get(ctx, &mk8spb.GetNodeGroupRequest{Id: gid})
	wantFailure(t, "Get after Delete", err, codes.NotFound)
	if n := len(e.nodes(t, gid)); n != 0 {
		t.Errorf("nodes after Delete = %d, want 0", n)
	}
	_, err = e.groups.Delete(ctx, &mk8spb.DeleteNodeGroupRequest{Id: gid})
	wantFailure(t, "second Delete", err, codes.NotFound)
}

func TestMK8sPreflightCheck(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	cid := e.newCluster(t, "c")
	check := func(action commonpb.PreflightCheckContext_Action, unknown string, md *commonpb.ResourceMetadata, spec *mk8spb.NodeGroupSpec) *mk8spb.PreflightCheckNodeGroupResponse {
		t.Helper()
		res, err := e.groups.PreflightCheck(ctx, &mk8spb.PreflightCheckNodeGroupRequest{
			Context:  &commonpb.PreflightCheckContext{Action: action, UnknownPaths: unknown},
			Metadata: md, Spec: spec,
		})
		if err != nil {
			t.Fatalf("PreflightCheck: %v", err)
		}
		return res
	}
	create := commonpb.PreflightCheckContext_CREATE
	md := &commonpb.ResourceMetadata{ParentId: cid, Name: "new"}

	if res := check(create, "", md, fixedGroup(1)); len(res.GetDiagnostics()) != 0 {
		t.Errorf("valid CREATE diagnostics = %v, want none", res.GetDiagnostics())
	}
	bad := fixedGroup(1)
	bad.Template.Resources.Size = &mk8spb.ResourcesSpec_Preset{Preset: "999vcpu"}
	res := check(create, "", md, bad)
	if len(res.GetDiagnostics()) == 0 {
		t.Fatal("invalid preset: no diagnostics")
	}
	d := res.GetDiagnostics()[0]
	if d.GetSeverity() != commonpb.PreflightCheckDiagnostic_ERROR || !strings.HasPrefix(d.GetPath(), "spec.template.resources") {
		t.Errorf("diagnostic = %v, want an ERROR at spec.template.resources", d)
	}
	if res := check(create, "spec.template.resources", md, bad); len(res.GetDiagnostics()) != 0 {
		t.Errorf("unknown spec.template.resources still diagnosed: %v", res.GetDiagnostics())
	}

	// UPDATE of an existing group: a partial spec like the CLI sends.
	gid := e.newGroup(t, cid, "existing", fixedGroup(1))
	upd := commonpb.PreflightCheckContext_UPDATE
	res = check(upd, "", &commonpb.ResourceMetadata{Id: gid, ParentId: cid, Name: "existing"}, &mk8spb.NodeGroupSpec{
		Size: &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: 3},
	})
	if len(res.GetDiagnostics()) != 0 || res.GetPathsRequireRecreate() != "" {
		t.Errorf("UPDATE without immutable change = %v, recreate %q, want clean", res.GetDiagnostics(), res.GetPathsRequireRecreate())
	}
	// The preflight changed nothing.
	if g := e.getGroup(t, gid); g.GetStatus().GetNodeCount() != 1 {
		t.Errorf("node count after preflight = %d, want 1", g.GetStatus().GetNodeCount())
	}
}

func TestMK8sCompatibilityMatrix(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	all, err := e.groups.GetCompatibilityMatrix(ctx, &mk8spb.GetNodeGroupCompatibilityMatrixRequest{ClusterKubernetesVersion: "1.36"})
	if err != nil {
		t.Fatalf("GetCompatibilityMatrix: %v", err)
	}
	var versions []string
	for _, v := range all.GetVersions() {
		versions = append(versions, v.GetKubernetesVersion())
		if len(v.GetItems()) == 0 {
			t.Errorf("version %s has no items", v.GetKubernetesVersion())
		}
	}
	if strings.Join(versions, ",") != "1.34,1.35,1.36" {
		t.Errorf("versions = %v, want 1.34,1.35,1.36", versions)
	}
	cpu, err := e.groups.GetCompatibilityMatrix(ctx, &mk8spb.GetNodeGroupCompatibilityMatrixRequest{ClusterKubernetesVersion: "1.36", Platform: "cpu-d3"})
	if err != nil {
		t.Fatalf("GetCompatibilityMatrix(cpu-d3): %v", err)
	}
	for _, v := range cpu.GetVersions() {
		if len(v.GetItems()) == 0 || len(v.GetItems()) >= len(all.GetVersions()[0].GetItems()) {
			t.Errorf("cpu-d3 items for %s = %d, want a non-empty subset", v.GetKubernetesVersion(), len(v.GetItems()))
		}
		for _, it := range v.GetItems() {
			found := false
			for _, p := range it.GetCompatiblePlatforms() {
				found = found || p == "cpu-d3"
			}
			if !found {
				t.Errorf("item %v does not list cpu-d3", it)
			}
		}
	}
	_, err = e.groups.GetCompatibilityMatrix(ctx, &mk8spb.GetNodeGroupCompatibilityMatrixRequest{ClusterKubernetesVersion: "1.20"})
	wantFailure(t, "unknown cluster version", err, codes.InvalidArgument)
}
