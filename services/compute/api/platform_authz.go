package api

import "github.com/kfirgollan/NebiusMocker/pkg/authz"

const platformService = "nebius.compute.v1.PlatformService"

// RegisterPlatformAuthzRules declares the permission each PlatformService RPC
// needs: compute.platforms.get / compute.platforms.list on the project.
func RegisterPlatformAuthzRules(r *authz.Rules) {
	r.Add("/"+platformService+"/GetByName", authz.OnParent(authz.Perm("compute", "platforms", authz.VerbGet)))
	r.Add("/"+platformService+"/List", authz.OnParent(authz.Perm("compute", "platforms", authz.VerbList)))
}
