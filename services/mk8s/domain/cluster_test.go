package domain_test

import (
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func TestCreateClusterDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := done(t)(e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "main"},
		Spec: &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{
			Endpoints: &mk8spb.ControlPlaneEndpointsSpec{PublicEndpoint: &mk8spb.PublicEndpointSpec{}},
		}},
	}))
	c, err := e.svc.GetCluster(t.Context(), id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	cp, st := c.GetSpec().GetControlPlane(), c.GetStatus()
	if cp.GetSubnetId() != defaultSubnet || cp.GetVersion() != "" || cp.GetEtcdClusterSize() != 0 {
		t.Errorf("spec.control_plane = %v, want the default subnet filled and the rest as sent", cp)
	}
	if st.GetState() != mk8spb.ClusterStatus_RUNNING || st.GetReconciling() {
		t.Fatalf("status = %v, want RUNNING, not reconciling", st)
	}
	scp := st.GetControlPlane()
	if scp.GetVersion() != "1.36.0-nebius-cp.1" || scp.GetEtcdClusterSize() != 3 {
		t.Errorf("status.control_plane = %v, want version 1.36.0-nebius-cp.1, etcd 3", scp)
	}
	if !strings.HasPrefix(scp.GetEndpoints().GetPublicEndpoint(), "https://") || !strings.HasPrefix(scp.GetEndpoints().GetPrivateEndpoint(), "https://") {
		t.Errorf("endpoints = %v, want https public and private", scp.GetEndpoints())
	}
	if b, _ := pem.Decode([]byte(scp.GetAuth().GetClusterCaCertificate())); b == nil {
		t.Error("cluster CA is not PEM")
	}
	if !e.vpc.isBound(defaultNetwork, id) {
		t.Error("the cluster is not bound to its network")
	}
	peerView, err := e.svc.Cluster(t.Context(), id)
	if err != nil || !peerView.Running || peerView.ProjectID != projectID {
		t.Errorf("Cluster (peer) = %+v, %v; want running in %s", peerView, err, projectID)
	}
}

func TestCreateClusterPrivateOnlyAndExplicitSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := done(t)(e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "private"},
		Spec: &mk8spb.ClusterSpec{
			ControlPlane: &mk8spb.ControlPlaneSpec{Version: "1.34", SubnetId: sameNetSubnet, EtcdClusterSize: 1},
			KubeNetwork:  &mk8spb.KubeNetworkSpec{ServiceCidrs: []string{"/20"}},
		},
	}))
	c, _ := e.svc.GetCluster(t.Context(), id)
	scp := c.GetStatus().GetControlPlane()
	if scp.GetEndpoints().GetPublicEndpoint() != "" || scp.GetVersion() != "1.34.0-nebius-cp.1" || scp.GetEtcdClusterSize() != 1 {
		t.Errorf("status.control_plane = %v, want private only, 1.34, etcd 1", scp)
	}
}

func TestCreateClusterRejections(t *testing.T) {
	t.Parallel()
	cp := func(f func(*mk8spb.ControlPlaneSpec)) *mk8spb.ClusterSpec {
		c := &mk8spb.ControlPlaneSpec{}
		f(c)
		return &mk8spb.ClusterSpec{ControlPlane: c}
	}
	tests := []struct {
		name      string
		parent    string
		spec      *mk8spb.ClusterSpec
		wantKind  errs.Kind
		wantField string
	}{
		{"version not in catalog", projectID, cp(func(c *mk8spb.ControlPlaneSpec) { c.Version = "1.30" }), errs.KindBadRequest, "spec.control_plane.version"},
		{"audit logs", projectID, cp(func(c *mk8spb.ControlPlaneSpec) { c.AuditLogs = &mk8spb.AuditLogsSpec{} }), errs.KindBadRequest, "spec.control_plane.audit_logs"},
		{"karpenter", projectID, cp(func(c *mk8spb.ControlPlaneSpec) { c.Karpenter = &mk8spb.Karpenter{} }), errs.KindBadRequest, "spec.control_plane.karpenter"},
		{"oidc", projectID, cp(func(c *mk8spb.ControlPlaneSpec) {
			c.Authentication = &mk8spb.AuthenticationSpec{Oidc: []*mk8spb.OIDCProvider{{Name: "x"}}}
		}), errs.KindBadRequest, "spec.control_plane.authentication.oidc"},
		{"subnet of another project", projectID, cp(func(c *mk8spb.ControlPlaneSpec) { c.SubnetId = foreignSubnet }), errs.KindBadRequest, "spec.control_plane.subnet_id"},
		{"missing subnet", projectID, cp(func(c *mk8spb.ControlPlaneSpec) { c.SubnetId = "vpcsubnet-e00nope" }), errs.KindNotFound, ""},
		{"no default subnet", otherProjectID, nil, errs.KindBadRequest, "spec.control_plane.subnet_id"},
		{"parent is not a project", "tenant-e00x", nil, errs.KindBadRequest, "metadata.parent_id"},
		{"unknown project", "project-e00nope", nil, errs.KindNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			_, err := e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: tt.parent, Name: "c"}, Spec: tt.spec,
			})
			assertErr(t, err, tt.wantKind, tt.wantField)
			if e.clusters.len() != 0 {
				t.Error("a rejected cluster was stored")
			}
		})
	}
}

// assertErr checks an error's kind and, when field is set, that it names it.
func assertErr(t *testing.T, err error, kind errs.Kind, field string) {
	t.Helper()
	if got := errs.KindOf(err); got != kind {
		t.Fatalf("error kind = %v, want %v (err: %v)", got, kind, err)
	}
	if field == "" {
		return
	}
	e, _ := errors.AsType[*errs.Error](err)
	for _, v := range e.Violations {
		if v.Field == field {
			return
		}
	}
	t.Fatalf("error %v does not name field %s", err, field)
}

func TestCreateClusterBackendFailureStaysProvisioning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.backend.clusterErr = errs.Internal("docker is down")
	op, err := e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "c"},
	})
	if err != nil || op.Err == nil {
		t.Fatalf("CreateCluster = %v, %v; want an accepted operation that fails", op, err)
	}
	c, _ := e.svc.GetCluster(t.Context(), op.ResourceID)
	st := c.GetStatus()
	if st.GetState() != mk8spb.ClusterStatus_PROVISIONING || len(st.GetEvents()) != 1 {
		t.Fatalf("status = %v, want PROVISIONING with one event", st)
	}
	if ev := st.GetEvents()[0].GetLastOccurrence(); ev.GetLevel() != commonpb.ResourceEvent_ERROR || ev.GetCode() != "ProvisioningFailed" {
		t.Errorf("event = %v, want ERROR ProvisioningFailed", ev)
	}
	// A node group cannot be created in it.
	_, err = e.createGroup(t.Context(), op.ResourceID, "g", fixedSpec(1))
	assertErr(t, err, errs.KindBadState, "")
}

func TestUpdateClusterVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, from, to string
		wantKind       errs.Kind
		wantStatus     string
	}{
		{"same version", "1.35", "1.35", errs.KindUnknown, "1.35.0-nebius-cp.1"},
		{"one minor up", "1.35", "1.36", errs.KindUnknown, "1.36.0-nebius-cp.1"},
		{"empty keeps the running version", "1.35", "", errs.KindUnknown, "1.35.0-nebius-cp.1"},
		{"skip a minor", "1.34", "1.36", errs.KindBadRequest, ""},
		{"downgrade", "1.36", "1.35", errs.KindBadRequest, ""},
		{"not in catalog", "1.36", "1.37", errs.KindBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c := e.createCluster(t, "c", tt.from)
			spec := c.GetSpec()
			spec.ControlPlane.Version = tt.to
			op, err := e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: c.GetMetadata(), Spec: spec})
			if tt.wantKind != errs.KindUnknown {
				assertErr(t, err, tt.wantKind, "spec.control_plane.version")
				return
			}
			done(t)(op, err)
			got, _ := e.svc.GetCluster(t.Context(), c.GetMetadata().GetId())
			if v := got.GetStatus().GetControlPlane().GetVersion(); v != tt.wantStatus || got.GetStatus().GetReconciling() {
				t.Errorf("status version = %q (reconciling %v), want %q", v, got.GetStatus().GetReconciling(), tt.wantStatus)
			}
		})
	}
}

func TestUpdateClusterImmutablesAndMetadata(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	id := c.GetMetadata().GetId()
	update := func(md *commonpb.ResourceMetadata, spec *mk8spb.ClusterSpec, rm *mask.Mask) error {
		op, err := e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: md, Spec: spec, ResetMask: rm})
		if err == nil && op.Err != nil {
			return op.Err
		}
		return err
	}
	moved := &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{SubnetId: sameNetSubnet}}
	assertErr(t, update(c.GetMetadata(), moved, nil), errs.KindBadRequest, "spec.control_plane.subnet_id")
	cidrs := &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{}, KubeNetwork: &mk8spb.KubeNetworkSpec{ServiceCidrs: []string{"/18"}}}
	assertErr(t, update(c.GetMetadata(), cidrs, nil), errs.KindBadRequest, "spec.kube_network.service_cidrs")

	// A full replace that omits the defaulted subnet keeps it.
	if err := update(c.GetMetadata(), &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{}}, mask.ParseMust("spec.control_plane.subnet_id")); err != nil {
		t.Fatalf("update without subnet: %v", err)
	}
	same, _ := e.svc.GetCluster(t.Context(), id)
	if same.GetMetadata().GetResourceVersion() != 1 || same.GetSpec().GetControlPlane().GetSubnetId() != defaultSubnet {
		t.Errorf("no-op update changed the cluster: %v", same)
	}
	md := &commonpb.ResourceMetadata{Id: id, Name: "renamed", Labels: map[string]string{"team": "ml"}}
	if err := update(md, same.GetSpec(), nil); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, _ := e.svc.GetCluster(t.Context(), id)
	if got.GetMetadata().GetName() != "renamed" || got.GetMetadata().GetResourceVersion() != 2 || got.GetMetadata().GetLabels()["team"] != "ml" {
		t.Errorf("metadata = %v, want renamed, labeled, resource_version 2", got.GetMetadata())
	}
	stale := &commonpb.ResourceMetadata{Id: id, Name: "x", ResourceVersion: 1}
	assertErr(t, update(stale, got.GetSpec(), nil), errs.KindConflict, "")
	unsupported := &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{Karpenter: &mk8spb.Karpenter{}}}
	assertErr(t, update(got.GetMetadata(), unsupported, nil), errs.KindBadRequest, "spec.control_plane.karpenter")
}

func TestDeleteClusterCascades(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	id := c.GetMetadata().GetId()
	g1 := done(t)(e.createGroup(t.Context(), id, "a", fixedSpec(2)))
	g2 := done(t)(e.createGroup(t.Context(), id, "b", fixedSpec(1)))
	if e.compute.total() != 3 {
		t.Fatalf("instances = %d, want 3", e.compute.total())
	}
	done(t)(e.svc.DeleteCluster(t.Context(), id, nil))
	if _, err := e.svc.GetCluster(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetCluster after delete: %v, want not_found", err)
	}
	for _, g := range []string{g1, g2} {
		if _, err := e.svc.GetNodeGroup(t.Context(), g); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("GetNodeGroup(%s) after cluster delete: %v, want not_found", g, err)
		}
		if n := len(e.backend.group(g).Nodes); n != 0 {
			t.Errorf("backend nodes of %s = %d, want 0", g, n)
		}
	}
	if e.compute.total() != 0 {
		t.Errorf("instances after delete = %d, want 0", e.compute.total())
	}
	if e.vpc.isBound(defaultNetwork, id) {
		t.Error("the network is still bound")
	}
	if len(e.releases.deleted) != 1 || e.releases.deleted[0] != id {
		t.Errorf("releases deleted for %v, want [%s]", e.releases.deleted, id)
	}
	if len(e.backend.deleted) != 1 {
		t.Errorf("backend clusters deleted = %v, want one", e.backend.deleted)
	}
}

// A cluster created on a user's network releases it on delete even when its
// subnet is already gone.
func TestDeleteClusterUnbindsNetworkAfterSubnetDeleted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := done(t)(e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "c"},
		Spec:     &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{SubnetId: otherNetSubnet}},
	}))
	if !e.vpc.isBound("vpcnetwork-e00other", id) {
		t.Fatal("the network is not bound after create")
	}
	e.vpc.mu.Lock()
	delete(e.vpc.subnets, otherNetSubnet)
	e.vpc.mu.Unlock()
	done(t)(e.svc.DeleteCluster(t.Context(), id, nil))
	if e.vpc.isBound("vpcnetwork-e00other", id) {
		t.Error("the network is still bound after the cluster was deleted")
	}
}

func TestControlPlaneVersionsAndMatrix(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "1.33", "1.34", "1.35", "1.36")
	var versions []string
	for _, v := range e.svc.ControlPlaneVersions() {
		versions = append(versions, v.GetVersion())
	}
	if strings.Join(versions, ",") != "1.33,1.34,1.35,1.36" {
		t.Errorf("ControlPlaneVersions = %v", versions)
	}
	m, err := e.svc.CompatibilityMatrix("1.36", "")
	if err != nil {
		t.Fatalf("CompatibilityMatrix: %v", err)
	}
	var got []string
	for _, v := range m.GetVersions() {
		got = append(got, v.GetKubernetesVersion())
		if len(v.GetItems()) != 5 {
			t.Errorf("%s items = %d, want 5", v.GetKubernetesVersion(), len(v.GetItems()))
		}
	}
	if strings.Join(got, ",") != "1.34,1.35,1.36" {
		t.Errorf("matrix versions = %v, want 1.34..1.36", got)
	}
	cpu, _ := e.svc.CompatibilityMatrix("1.35", "cpu-d3")
	if n := len(cpu.GetVersions()); n != 3 || len(cpu.GetVersions()[0].GetItems()) != 2 {
		t.Errorf("cpu-d3 matrix = %v, want 3 versions with the 2 driverless images", cpu)
	}
	b200, _ := e.svc.CompatibilityMatrix("1.36", "gpu-b200-sxm")
	if n := len(b200.GetVersions()[0].GetItems()); n != 4 {
		t.Errorf("gpu-b200-sxm items = %d, want 4 (no ubuntu22.04 cuda12)", n)
	}
	_, err = e.svc.CompatibilityMatrix("1.20", "")
	assertErr(t, err, errs.KindBadRequest, "cluster_kubernetes_version")
}
