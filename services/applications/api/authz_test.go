package api_test

import (
	"testing"

	"google.golang.org/grpc"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/applications/api"
)

func newRules() *authz.Rules {
	r := authz.NewRules()
	api.RegisterAuthzRules(r)
	return r
}

func TestEveryRPCHasARule(t *testing.T) {
	t.Parallel()
	rules := newRules()
	for _, d := range []grpc.ServiceDesc{applicationsv1alpha1pb.K8SReleaseService_ServiceDesc, applicationsv1alpha1pb.VmAppTemplateService_ServiceDesc} {
		if len(d.Methods) == 0 {
			t.Fatalf("%s has no methods", d.ServiceName)
		}
		for _, m := range d.Methods {
			full := "/" + d.ServiceName + "/" + m.MethodName
			if _, ok := rules.Lookup(full); !ok {
				t.Errorf("no authz rule for %s", full)
			}
		}
	}
}

func TestPermissionsAndTargets(t *testing.T) {
	t.Parallel()
	rules := newRules()
	const svc = "/nebius.applications.v1alpha1.K8sReleaseService/"
	tests := []struct {
		method, perm string
		req          any
		target       string
	}{
		{"Get", "applications.k8sReleases.get", &applicationsv1alpha1pb.GetK8SReleaseRequest{Id: "applicationrelease-1"}, "applicationrelease-1"},
		{"List", "applications.k8sReleases.list", &applicationsv1alpha1pb.ListK8SReleasesRequest{ParentId: "project-1", ClusterId: "mk8scluster-1"}, "project-1"},
		{"Create", "applications.k8sReleases.create", &applicationsv1alpha1pb.CreateK8SReleaseRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "project-1"}}, "project-1"},
		{"Update", "applications.k8sReleases.update", &applicationsv1alpha1pb.UpdateK8SReleaseRequest{Metadata: &commonpb.ResourceMetadata{Id: "applicationrelease-1"}}, "applicationrelease-1"},
		{"Delete", "applications.k8sReleases.delete", &applicationsv1alpha1pb.DeleteK8SReleaseRequest{Id: "applicationrelease-1"}, "applicationrelease-1"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup(svc + tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			if got := string(rule.Permission); got != tt.perm {
				t.Errorf("permission = %q, want %q", got, tt.perm)
			}
			got, err := rule.Target(t.Context(), tt.req)
			if err != nil || got != tt.target {
				t.Errorf("Target = %q, %v; want %q, nil", got, err, tt.target)
			}
		})
	}
	rule, _ := rules.Lookup(svc + "Get")
	if _, err := rule.Target(t.Context(), &applicationsv1alpha1pb.GetK8SReleaseRequest{}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("empty id error = %v, want bad_request", err)
	}
}

func TestVmAppTemplatesAreOpenToAnyPrincipal(t *testing.T) {
	t.Parallel()
	rules := newRules()
	for _, m := range []string{"Get", "List"} {
		rule, _ := rules.Lookup("/nebius.applications.v1alpha1.VmAppTemplateService/" + m)
		if rule.AllowIf == nil || !rule.AllowIf(t.Context(), "serviceaccount-1", nil) {
			t.Errorf("%s: any authenticated principal must pass", m)
		}
	}
}
