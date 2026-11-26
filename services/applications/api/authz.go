package api

import (
	"context"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

// Service names of the Applications gRPC services served by this module.
const (
	releaseService     = "nebius.applications.v1alpha1.K8sReleaseService"
	vmAppTemplateSvc   = "nebius.applications.v1alpha1.VmAppTemplateService"
	vmAppTemplateGet   = "/" + vmAppTemplateSvc + "/Get"
	vmAppTemplateList  = "/" + vmAppTemplateSvc + "/List"
	releasePermService = "applications"
	releasePermKind    = "k8sReleases"
)

// RegisterAuthzRules declares which permission each Applications RPC needs.
//
// The VM app template catalog is not served (its handler answers
// UNIMPLEMENTED); its rules let any authenticated principal through so that
// callers see UNIMPLEMENTED rather than PERMISSION_DENIED under enforce.
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(releaseService, releasePermService, releasePermKind)
	anyone := authz.Rule{
		Permission: authz.Perm("applications", "vmAppTemplates", authz.VerbGet),
		Target:     func(context.Context, any) (string, error) { return "", nil },
		AllowIf:    func(context.Context, string, any) bool { return true },
	}
	r.Add(vmAppTemplateGet, anyone)
	anyone.Permission = authz.Perm("applications", "vmAppTemplates", authz.VerbList)
	r.Add(vmAppTemplateList, anyone)
}

// VmAppTemplateHandler registers nebius.applications.v1alpha1.VmAppTemplateService;
// every RPC answers UNIMPLEMENTED.
type VmAppTemplateHandler struct {
	applicationsv1alpha1pb.UnimplementedVmAppTemplateServiceServer
}

var _ applicationsv1alpha1pb.VmAppTemplateServiceServer = VmAppTemplateHandler{}
