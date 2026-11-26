// Package api is the DNS gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Service names of the DNS gRPC services served by this module.
const (
	zoneService   = "nebius.dns.v1.ZoneService"
	recordService = "nebius.dns.v1.RecordService"
)

type networkIDGetter interface{ GetNetworkId() string }

// RegisterAuthzRules declares which permission each DNS RPC needs. Records
// have their own permissions (dns.records.*), so a role can read zones but
// not records (docs: the auditor role).
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(zoneService, "dns", "zones")
	r.CRUD(recordService, "dns", "records")
	// Listing a network's zones is checked on the network (whose parent is
	// its project).
	r.Add("/"+zoneService+"/ListByNetwork", authz.Rule{
		Permission: authz.Perm("dns", "zones", authz.VerbList),
		Target: func(_ context.Context, req any) (string, error) {
			if g, ok := req.(networkIDGetter); ok && g.GetNetworkId() != "" {
				return g.GetNetworkId(), nil
			}
			return "", errs.BadRequest(errs.Violation{Field: "network_id", Message: "required"})
		},
	})
}
