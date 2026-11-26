package api

import "github.com/kfirgollan/NebiusMocker/pkg/authz"

// Service and permission names of the Tunnels gRPC service.
const (
	tunnelService   = "nebius.tunnel.v1.TunnelService"
	tunnelPermSvc   = "applicationtunnel"
	tunnelPermKinds = "tunnels"
)

// RegisterAuthzRules declares which permission each TunnelService RPC needs
// (applicationtunnel.tunnels.<verb>).
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(tunnelService, tunnelPermSvc, tunnelPermKinds)
}
