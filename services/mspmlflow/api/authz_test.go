package api_test

import (
	"testing"

	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/api"
)

func TestEveryRPCHasARule(t *testing.T) {
	t.Parallel()
	rules := authz.NewRules()
	api.RegisterAuthzRules(rules)
	for _, m := range mlflowpb.ClusterService_ServiceDesc.Methods {
		full := "/" + mlflowpb.ClusterService_ServiceDesc.ServiceName + "/" + m.MethodName
		if _, ok := rules.Lookup(full); !ok {
			t.Errorf("no authz rule for %s", full)
		}
	}
}

func TestPermissions(t *testing.T) {
	t.Parallel()
	rules := authz.NewRules()
	api.RegisterAuthzRules(rules)
	catalog := authz.DefaultCatalog()
	tests := []struct {
		method string
		perm   string
		viewer bool
		editor bool
	}{
		{"Get", "mlflow.clusters.get", true, true},
		{"GetByName", "mlflow.clusters.get", true, true},
		{"List", "mlflow.clusters.list", true, true},
		{"Create", "mlflow.clusters.create", false, true},
		{"Delete", "mlflow.clusters.delete", false, true},
		{"Stop", "mlflow.clusters.stop", false, true},
		{"Start", "mlflow.clusters.start", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup("/nebius.msp.mlflow.v1alpha1.ClusterService/" + tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			if string(rule.Permission) != tt.perm {
				t.Fatalf("permission = %q, want %q", rule.Permission, tt.perm)
			}
			if got := catalog.Allows(authz.RoleViewer, rule.Permission); got != tt.viewer {
				t.Errorf("viewer allowed = %v, want %v", got, tt.viewer)
			}
			if got := catalog.Allows(authz.RoleEditor, rule.Permission); got != tt.editor {
				t.Errorf("editor allowed = %v, want %v", got, tt.editor)
			}
		})
	}
}

func TestTargets(t *testing.T) {
	t.Parallel()
	rules := authz.NewRules()
	api.RegisterAuthzRules(rules)
	tests := []struct {
		method string
		req    any
		want   string
	}{
		{"Get", &mlflowpb.GetClusterRequest{Id: "mlflow-1"}, "mlflow-1"},
		{"Stop", &mlflowpb.StopClusterRequest{Id: "mlflow-1"}, "mlflow-1"},
		{"Start", &mlflowpb.StartClusterRequest{Id: "mlflow-1"}, "mlflow-1"},
		{"Delete", &mlflowpb.DeleteClusterRequest{Id: "mlflow-1"}, "mlflow-1"},
		{"GetByName", &mlflowpb.GetClusterByNameRequest{ParentId: "project-1", Name: "x"}, "project-1"},
		{"List", &mlflowpb.ListClustersRequest{ParentId: "project-1"}, "project-1"},
	}
	for _, tt := range tests {
		rule, _ := rules.Lookup("/nebius.msp.mlflow.v1alpha1.ClusterService/" + tt.method)
		got, err := rule.Target(t.Context(), tt.req)
		if err != nil || got != tt.want {
			t.Errorf("%s target = %q, %v, want %q", tt.method, got, err, tt.want)
		}
	}
}
