package api

import (
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

// Service names of the deprecated v1alpha1 services, served as a second view
// of the v1 resources.
const (
	alphaClusterService   = "nebius.mk8s.v1alpha1.ClusterService"
	alphaNodeGroupService = "nebius.mk8s.v1alpha1.NodeGroupService"
)

// RegisterAlphaAuthzRules declares the v1alpha1 RPCs: the same permissions as
// their v1 twins (v1alpha1 has no preflight check or compatibility matrix).
func RegisterAlphaAuthzRules(r *authz.Rules) {
	r.CRUD(alphaClusterService, "mk8s", "clusters")
	r.Add("/"+alphaClusterService+"/ListControlPlaneVersions", authz.Rule{
		Permission: authz.Perm("mk8s", "clusters", authz.VerbList), AllowIf: anyone, Target: noTarget,
	})
	r.CRUD(alphaNodeGroupService, "mk8s", "nodeGroups")
	r.Add("/"+alphaNodeGroupService+"/Upgrade", authz.OnID(authz.Perm("mk8s", "nodeGroups", authz.VerbUpdate)))
}
