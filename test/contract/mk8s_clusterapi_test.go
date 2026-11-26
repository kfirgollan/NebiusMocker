package contract

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// newCluster creates a RUNNING cluster with the default network.
func (e *mk8sEnv) newCluster(t *testing.T, name string) string {
	t.Helper()
	return opWait(t)(e.clusters.Create(t.Context(), e.clusterReq(name, nil)))
}

func (e *mk8sEnv) clusterReq(name string, cp *mk8spb.ControlPlaneSpec) *mk8spb.CreateClusterRequest {
	if cp == nil {
		cp = &mk8spb.ControlPlaneSpec{}
	}
	return &mk8spb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.project, Name: name},
		Spec:     &mk8spb.ClusterSpec{ControlPlane: cp},
	}
}

func (e *mk8sEnv) getCluster(t *testing.T, id string) *mk8spb.Cluster {
	t.Helper()
	c, err := e.clusters.Get(t.Context(), &mk8spb.GetClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("get cluster %s: %v", id, err)
	}
	return c
}

// wantFailure asserts that a call (or the operation it started) failed with
// code and that the message names every fragment.
func wantFailure(t *testing.T, what string, err error, code codes.Code, fragments ...string) {
	t.Helper()
	if got := codeOf(err); got != code {
		t.Fatalf("%s: code = %v, want %v (err: %v)", what, got, code, err)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("%s: error %q does not mention %q", what, err, f)
		}
	}
}

func TestMK8sClusterReadRPCs(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	ids := map[string]string{}
	for _, n := range []string{"alpha", "beta", "gamma"} {
		ids[n] = e.newCluster(t, n)
	}

	c := e.getCluster(t, ids["beta"])
	if c.GetMetadata().GetName() != "beta" || c.GetMetadata().GetParentId() != e.project {
		t.Errorf("Get = %v", c.GetMetadata())
	}
	byName, err := e.clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.project, Name: "gamma"})
	if err != nil || byName.GetMetadata().GetId() != ids["gamma"] {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	_, err = e.clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.project, Name: "nope"})
	wantFailure(t, "GetByName unknown", err, codes.NotFound)
	_, err = e.clusters.Get(ctx, &mk8spb.GetClusterRequest{Id: "mk8scluster-e00unknown0000000000"})
	wantFailure(t, "Get unknown", err, codes.NotFound)

	// List pages in creation order.
	var got []string
	token := ""
	for pages := 0; ; pages++ {
		res, err := e.clusters.List(ctx, &mk8spb.ListClustersRequest{ParentId: e.project, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, it := range res.GetItems() {
			got = append(got, it.GetMetadata().GetName())
		}
		if token = res.GetNextPageToken(); token == "" {
			if pages != 1 {
				t.Errorf("pages = %d, want 2", pages+1)
			}
			break
		}
	}
	if strings.Join(got, ",") != "alpha,beta,gamma" {
		t.Errorf("List = %v, want alpha,beta,gamma", got)
	}
	_, err = e.clusters.List(ctx, &mk8spb.ListClustersRequest{ParentId: e.project, PageToken: "garbage!"})
	wantFailure(t, "List bad token", err, codes.InvalidArgument, "page_token")

	vs, err := e.clusters.ListControlPlaneVersions(ctx, &mk8spb.ListClusterControlPlaneVersionsRequest{})
	if err != nil {
		t.Fatalf("ListControlPlaneVersions: %v", err)
	}
	var versions []string
	for _, v := range vs.GetItems() {
		versions = append(versions, v.GetVersion())
	}
	if strings.Join(versions, ",") != "1.34,1.35,1.36" {
		t.Errorf("control plane versions = %v, want 1.34,1.35,1.36", versions)
	}
}

func TestMK8sClusterCreateErrors(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	e.newCluster(t, "taken")

	_, err := e.clusters.Create(ctx, e.clusterReq("taken", nil))
	wantFailure(t, "duplicate name", err, codes.AlreadyExists)

	tests := []struct {
		name string
		cp   *mk8spb.ControlPlaneSpec
		code codes.Code
		want string
	}{
		{"unknown version", &mk8spb.ControlPlaneSpec{Version: "1.20"}, codes.InvalidArgument, "spec.control_plane.version"},
		{"missing subnet", &mk8spb.ControlPlaneSpec{SubnetId: "vpcsubnet-e00missing000000000"}, codes.NotFound, "vpcsubnet-e00missing000000000"},
		{"karpenter", &mk8spb.ControlPlaneSpec{Karpenter: &mk8spb.Karpenter{}}, codes.InvalidArgument, "spec.control_plane.karpenter"},
		{"audit logs", &mk8spb.ControlPlaneSpec{AuditLogs: &mk8spb.AuditLogsSpec{}}, codes.InvalidArgument, "spec.control_plane.audit_logs"},
	}
	for _, tt := range tests {
		_, err := e.clusters.Create(ctx, e.clusterReq("c-"+strings.ReplaceAll(tt.name, " ", "-"), tt.cp))
		wantFailure(t, tt.name, err, tt.code, tt.want)
	}

	// A project that does not exist.
	req := e.clusterReq("orphan", nil)
	req.Metadata.ParentId = "project-e00doesnotexist00000"
	_, err = e.clusters.Create(ctx, req)
	wantFailure(t, "unknown project", err, codes.NotFound)
	// Proto validation: no name, no parent.
	_, err = e.clusters.Create(ctx, &mk8spb.CreateClusterRequest{Metadata: &commonpb.ResourceMetadata{ParentId: e.project}})
	wantFailure(t, "no name", err, codes.InvalidArgument)
}

func TestMK8sClusterUpdate(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	id := e.newCluster(t, "up")
	c := e.getCluster(t, id)
	if got := c.GetStatus().GetControlPlane().GetVersion(); got != "1.36.0-nebius-cp.1" {
		t.Fatalf("version = %q", got)
	}

	// Labels are replaced; a reset mask clears them again.
	md := c.GetMetadata()
	md.Labels = map[string]string{"team": "ml"}
	opWait(t)(e.clusters.Update(ctx, &mk8spb.UpdateClusterRequest{Metadata: md, Spec: c.GetSpec()}))
	c = e.getCluster(t, id)
	if c.GetMetadata().GetLabels()["team"] != "ml" || c.GetMetadata().GetResourceVersion() < 2 {
		t.Errorf("after label update = %v", c.GetMetadata())
	}
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	md = c.GetMetadata()
	md.Labels = nil
	opWait(t)(e.clusters.Update(maskCtx, &mk8spb.UpdateClusterRequest{Metadata: md, Spec: c.GetSpec()}))
	if l := e.getCluster(t, id).GetMetadata().GetLabels(); len(l) != 0 {
		t.Errorf("labels after reset = %v, want none", l)
	}

	update := func(mutate func(*mk8spb.ClusterSpec)) error {
		cur := e.getCluster(t, id)
		spec := cur.GetSpec()
		mutate(spec)
		op, err := e.clusters.Update(t.Context(), &mk8spb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: spec})
		return kmsWait(t.Context(), op, err)
	}
	wantFailure(t, "immutable subnet", update(func(s *mk8spb.ClusterSpec) { s.ControlPlane.SubnetId = "vpcsubnet-e00other0000000000" }),
		codes.InvalidArgument, "spec.control_plane.subnet_id")
	wantFailure(t, "skip a minor", update(func(s *mk8spb.ClusterSpec) { s.ControlPlane.Version = "1.38" }), codes.InvalidArgument, "version")
	wantFailure(t, "karpenter", update(func(s *mk8spb.ClusterSpec) { s.ControlPlane.Karpenter = &mk8spb.Karpenter{} }),
		codes.InvalidArgument, "karpenter")

	// Version rules: downgrade and skipping are refused, +1 minor is accepted.
	e2 := startMK8s(t)
	id2 := opWait(t)(e2.clusters.Create(ctx, e2.clusterReq("old", &mk8spb.ControlPlaneSpec{Version: "1.34"})))
	setVersion := func(v string) error {
		cur := e2.getCluster(t, id2)
		spec := cur.GetSpec()
		spec.ControlPlane.Version = v
		op, err := e2.clusters.Update(t.Context(), &mk8spb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: spec})
		return kmsWait(t.Context(), op, err)
	}
	wantFailure(t, "skip 1.34 -> 1.36", setVersion("1.36"), codes.InvalidArgument, "spec.control_plane.version")
	if err := setVersion("1.35"); err != nil {
		t.Fatalf("upgrade 1.34 -> 1.35: %v", err)
	}
	if got := e2.getCluster(t, id2).GetStatus().GetControlPlane().GetVersion(); !strings.HasPrefix(got, "1.35.") {
		t.Errorf("version after upgrade = %q, want 1.35.x", got)
	}
	wantFailure(t, "downgrade 1.35 -> 1.34", setVersion("1.34"), codes.InvalidArgument, "spec.control_plane.version")

	// A stale resource_version is a conflict.
	cur := e.getCluster(t, id)
	stale := cur.GetMetadata()
	stale.ResourceVersion = 1
	_, err = e.clusters.Update(ctx, &mk8spb.UpdateClusterRequest{Metadata: stale, Spec: cur.GetSpec()})
	wantFailure(t, "stale resource_version", err, codes.Aborted)
	_, err = e.clusters.Update(ctx, &mk8spb.UpdateClusterRequest{Metadata: &commonpb.ResourceMetadata{Id: "mk8scluster-e00unknown0000000000"}, Spec: cur.GetSpec()})
	wantFailure(t, "update unknown", err, codes.NotFound)
}

func TestMK8sClusterDelete(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	id := e.newCluster(t, "bye")
	opWait(t)(e.clusters.Delete(ctx, &mk8spb.DeleteClusterRequest{Id: id}))
	_, err := e.clusters.Get(ctx, &mk8spb.GetClusterRequest{Id: id})
	wantFailure(t, "Get after Delete", err, codes.NotFound)
	_, err = e.clusters.Delete(ctx, &mk8spb.DeleteClusterRequest{Id: id})
	wantFailure(t, "second Delete", err, codes.NotFound)
	// The name is free again.
	e.newCluster(t, "bye")
}

// A cluster on a user-made network holds that network; deleting the cluster
// releases it even when the subnet was removed first.
func TestMK8sClusterReleasesNetworkAfterSubnetDeleted(t *testing.T) {
	t.Parallel()
	e := startMK8s(t)
	ctx := t.Context()
	vc := vpcClientsFor(t, e.inst, e.inst.Token)
	netID := newNetwork(t, vc, "k8s-net")
	subnetID := newSubnet(t, vc, "k8s-subnet", netID, "/24")

	id := opWait(t)(e.clusters.Create(ctx, e.clusterReq("netted", &mk8spb.ControlPlaneSpec{SubnetId: subnetID})))
	if got := e.getCluster(t, id).GetSpec().GetControlPlane().GetSubnetId(); got != subnetID {
		t.Fatalf("subnet_id = %q, want %q", got, subnetID)
	}
	_, err := vc.networks.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: netID})
	wantFailure(t, "network delete while a cluster holds it", err, codes.FailedPrecondition, id)

	opWait(t)(vc.subnets.Delete(ctx, &vpcpb.DeleteSubnetRequest{Id: subnetID}))
	opWait(t)(e.clusters.Delete(ctx, &mk8spb.DeleteClusterRequest{Id: id}))
	opWait(t)(vc.networks.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: netID}))
}

// A cluster that is still provisioning is PROVISIONING and reconciling until
// its operation completes.
func TestMK8sClusterProvisioningState(t *testing.T) {
	t.Parallel()
	e := startMK8s(t, testkit.WithOperationDelay(400*time.Millisecond))
	op, err := e.clusters.Create(t.Context(), e.clusterReq("slow", nil))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := e.getCluster(t, op.ResourceID())
	if c.GetStatus().GetState() != mk8spb.ClusterStatus_PROVISIONING || !c.GetStatus().GetReconciling() {
		t.Errorf("status while the operation runs = %v, want PROVISIONING and reconciling", c.GetStatus())
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if st := e.getCluster(t, op.ResourceID()).GetStatus(); st.GetState() != mk8spb.ClusterStatus_RUNNING || st.GetReconciling() {
		t.Errorf("status after = %v, want RUNNING", st)
	}
}
