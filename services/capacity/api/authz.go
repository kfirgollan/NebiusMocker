// Package api is the capacity gRPC view layer: it converts between protos
// and domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Service names of the capacity gRPC services served by this module.
const (
	allowanceService  = "nebius.capacity.v1.CapacityAllowanceService"
	blockGroupService = "nebius.capacity.v1.CapacityBlockGroupService"
	intervalService   = "nebius.capacity.v1.CapacityIntervalService"
	adviceService     = "nebius.capacity.v1.ResourceAdviceService"
)

type blockGroupIDGetter interface{ GetCapacityBlockGroupId() string }

// RegisterAuthzRules declares which permission each capacity RPC needs.
// Allowances are checked on their project, block groups and resource advice
// on their tenant, intervals on their block group (whose parent is the
// tenant).
func RegisterAuthzRules(r *authz.Rules) {
	m := func(service, method string) string { return "/" + service + "/" + method }

	r.CRUD(allowanceService, "capacity", "allowances")
	r.Add(m(allowanceService, "GetByParentAndCapacityBlockGroup"), authz.OnParent(authz.Perm("capacity", "allowances", authz.VerbGet)))
	// Listing a block group's allowances is checked on the block group.
	r.Add(m(allowanceService, "ListByCapacityBlockGroup"), authz.Rule{
		Permission: authz.Perm("capacity", "allowances", authz.VerbList),
		Target: func(_ context.Context, req any) (string, error) {
			if g, ok := req.(blockGroupIDGetter); ok && g.GetCapacityBlockGroupId() != "" {
				return g.GetCapacityBlockGroupId(), nil
			}
			return "", errs.BadRequest(errs.Violation{Field: "capacity_block_group_id", Message: "required"})
		},
	})

	r.Add(m(blockGroupService, "Get"), authz.OnID(authz.Perm("capacity", "blockGroups", authz.VerbGet)))
	r.Add(m(blockGroupService, "GetByResourceAffinity"), authz.OnParent(authz.Perm("capacity", "blockGroups", authz.VerbGet)))
	r.Add(m(blockGroupService, "List"), authz.OnParent(authz.Perm("capacity", "blockGroups", authz.VerbList)))
	r.Add(m(blockGroupService, "ListResources"), authz.OnID(authz.Perm("capacity", "blockGroups", authz.VerbList)))

	r.Add(m(intervalService, "Get"), authz.OnID(authz.Perm("capacity", "intervals", authz.VerbGet)))
	r.Add(m(intervalService, "List"), authz.OnParent(authz.Perm("capacity", "intervals", authz.VerbList)))

	r.Add(m(adviceService, "List"), authz.OnParent(authz.Perm("capacity", "resourceAdvice", authz.VerbList)))
}
