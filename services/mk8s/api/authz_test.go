package api_test

import (
	"testing"

	"google.golang.org/grpc"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/api"
)

func newRules() *authz.Rules {
	r := authz.NewRules()
	api.RegisterAuthzRules(r)
	return r
}

func TestEveryRPCHasARule(t *testing.T) {
	t.Parallel()
	rules := newRules()
	for _, d := range []grpc.ServiceDesc{mk8spb.ClusterService_ServiceDesc, mk8spb.NodeGroupService_ServiceDesc} {
		for _, m := range d.Methods {
			full := "/" + d.ServiceName + "/" + m.MethodName
			if _, ok := rules.Lookup(full); !ok {
				t.Errorf("no authz rule for %s", full)
			}
		}
	}
}

func TestPermissions(t *testing.T) {
	t.Parallel()
	rules := newRules()
	tests := []struct{ method, perm string }{
		{"/nebius.mk8s.v1.ClusterService/Create", "mk8s.clusters.create"},
		{"/nebius.mk8s.v1.ClusterService/Update", "mk8s.clusters.update"},
		{"/nebius.mk8s.v1.ClusterService/ListControlPlaneVersions", "mk8s.clusters.list"},
		{"/nebius.mk8s.v1.NodeGroupService/Get", "mk8s.nodeGroups.get"},
		{"/nebius.mk8s.v1.NodeGroupService/Delete", "mk8s.nodeGroups.delete"},
		{"/nebius.mk8s.v1.NodeGroupService/Upgrade", "mk8s.nodeGroups.update"},
		{"/nebius.mk8s.v1.NodeGroupService/PreflightCheck", "mk8s.nodeGroups.get"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup(tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			if got := string(rule.Permission); got != tt.perm {
				t.Errorf("permission = %q, want %q", got, tt.perm)
			}
		})
	}
}

func TestTargets(t *testing.T) {
	t.Parallel()
	rules := newRules()
	tests := []struct {
		name   string
		method string
		req    any
		want   string
	}{
		{
			"cluster create on project", "/nebius.mk8s.v1.ClusterService/Create",
			&mk8spb.CreateClusterRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "project-1"}}, "project-1",
		},
		{
			"node group create on cluster", "/nebius.mk8s.v1.NodeGroupService/Create",
			&mk8spb.CreateNodeGroupRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "mk8scluster-1"}}, "mk8scluster-1",
		},
		{
			"node group list on cluster", "/nebius.mk8s.v1.NodeGroupService/List",
			&mk8spb.ListNodeGroupsRequest{ParentId: "mk8scluster-1"}, "mk8scluster-1",
		},
		{
			"upgrade on node group", "/nebius.mk8s.v1.NodeGroupService/Upgrade",
			&mk8spb.UpgradeNodeGroupRequest{Id: "mk8snodegroup-1"}, "mk8snodegroup-1",
		},
		{
			"preflight update on node group", "/nebius.mk8s.v1.NodeGroupService/PreflightCheck",
			&mk8spb.PreflightCheckNodeGroupRequest{Metadata: &commonpb.ResourceMetadata{Id: "mk8snodegroup-1", ParentId: "mk8scluster-1"}}, "mk8snodegroup-1",
		},
		{
			"preflight create on cluster", "/nebius.mk8s.v1.NodeGroupService/PreflightCheck",
			&mk8spb.PreflightCheckNodeGroupRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "mk8scluster-1"}}, "mk8scluster-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rule, _ := rules.Lookup(tt.method)
			if rule.AllowIf != nil && rule.AllowIf(t.Context(), "user-1", tt.req) {
				t.Fatal("AllowIf short-circuits a targeted request")
			}
			got, err := rule.Target(t.Context(), tt.req)
			if err != nil || got != tt.want {
				t.Fatalf("Target = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestPublicRPCsAllowAnyPrincipal(t *testing.T) {
	t.Parallel()
	rules := newRules()
	for method, req := range map[string]any{
		"/nebius.mk8s.v1.ClusterService/ListControlPlaneVersions": &mk8spb.ListClusterControlPlaneVersionsRequest{},
		"/nebius.mk8s.v1.NodeGroupService/GetCompatibilityMatrix": &mk8spb.GetNodeGroupCompatibilityMatrixRequest{ClusterKubernetesVersion: "1.36"},
		"/nebius.mk8s.v1.NodeGroupService/PreflightCheck":         &mk8spb.PreflightCheckNodeGroupRequest{}, // unknown cluster
	} {
		rule, _ := rules.Lookup(method)
		if rule.AllowIf == nil || !rule.AllowIf(t.Context(), "user-1", req) {
			t.Errorf("%s: AllowIf = false, want true", method)
		}
	}
}

func TestEditorAndViewerRoles(t *testing.T) {
	t.Parallel()
	c := authz.DefaultCatalog()
	for _, p := range []authz.Permission{"mk8s.clusters.create", "mk8s.nodeGroups.update", "mk8s.nodeGroups.delete"} {
		if !c.Allows(authz.RoleEditor, p) {
			t.Errorf("editor must hold %s", p)
		}
		if c.Allows(authz.RoleViewer, p) {
			t.Errorf("viewer must not hold %s", p)
		}
	}
	if !c.Allows(authz.RoleViewer, "mk8s.nodeGroups.get") {
		t.Error("viewer must read node groups")
	}
}
