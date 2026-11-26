package api

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

const pkgPrefix = "nebius.vpc.v1."

// RegisterAuthzRules declares which permission each VPC RPC needs. Every RPC
// served by this module has a rule: unknown methods are denied under enforce.
func RegisterAuthzRules(r *authz.Rules) {
	type extra struct{ method, field string }
	services := []struct {
		name, resources string
		extras          []extra
	}{
		{"PoolService", "pools", []extra{{"ListAggregated", "parent_id"}, {"ListBySourcePool", "pool_id"}}},
		{"NetworkService", "networks", []extra{{"ListAggregated", "parent_id"}}},
		{"SubnetService", "subnets", []extra{{"ListAggregated", "parent_id"}, {"ListByNetwork", "network_id"}}},
		{"AllocationService", "allocations", []extra{
			{"ListAggregated", "parent_id"}, {"ListByPool", "pool_id"}, {"ListBySubnet", "subnet_id"},
		}},
		{"RouteTableService", "routeTables", []extra{{"ListAggregated", "parent_id"}, {"ListByNetwork", "network_id"}}},
		{"RouteService", "routes", nil},
		{"SecurityGroupService", "securityGroups", []extra{{"ListAggregated", "parent_id"}, {"ListByNetwork", "network_id"}}},
		{"SecurityRuleService", "securityRules", nil},
	}
	for _, s := range services {
		r.CRUD(pkgPrefix+s.name, "vpc", s.resources)
		for _, e := range s.extras {
			r.Add("/"+pkgPrefix+s.name+"/"+e.method, onField(authz.Perm("vpc", s.resources, authz.VerbList), e.field))
		}
	}
	registerAlphaAuthzRules(r)
	network := func(m string) string { return "/" + pkgPrefix + "NetworkService/" + m }
	r.Add(network("GetDefault"), authz.OnParent(authz.Perm("vpc", "networks", authz.VerbGet)))
	r.Add(network("CreateDefault"), authz.OnMetadataParent(authz.Perm("vpc", "networks", authz.VerbCreate)))
}

const alphaPrefix = "nebius.vpc.v1alpha1."

// registerAlphaAuthzRules declares the v1alpha1 RPCs: the same permissions as
// their v1 twins. Everything but allocations is read-only there.
func registerAlphaAuthzRules(r *authz.Rules) {
	r.CRUD(alphaPrefix+"AllocationService", "vpc", "allocations")
	for _, s := range []struct{ name, resources string }{
		{"PoolService", "pools"}, {"NetworkService", "networks"}, {"SubnetService", "subnets"}, {"ScopeService", "scopes"},
	} {
		method := func(m string) string { return "/" + alphaPrefix + s.name + "/" + m }
		r.Add(method("Get"), authz.OnID(authz.Perm("vpc", s.resources, authz.VerbGet)))
		r.Add(method("GetByName"), authz.OnParent(authz.Perm("vpc", s.resources, authz.VerbGet)))
		r.Add(method("List"), authz.OnParent(authz.Perm("vpc", s.resources, authz.VerbList)))
	}
	r.Add("/"+alphaPrefix+"NetworkService/GetDefault", authz.OnParent(authz.Perm("vpc", "networks", authz.VerbGet)))
	r.Add("/"+alphaPrefix+"SubnetService/ListByNetwork", onField(authz.Perm("vpc", "subnets", authz.VerbList), "network_id"))
}

type (
	networkIDGetter interface{ GetNetworkId() string }
	poolIDGetter    interface{ GetPoolId() string }
	subnetIDGetter  interface{ GetSubnetId() string }
	parentIDGetter  interface{ GetParentId() string }
)

// onField checks perm on the resource named by one request field (a ListBy* filter).
func onField(perm authz.Permission, field string) authz.Rule {
	return authz.Rule{Permission: perm, Target: func(_ context.Context, req any) (string, error) {
		var id string
		switch g := req.(type) {
		case networkIDGetter:
			id = g.GetNetworkId()
		case poolIDGetter:
			id = g.GetPoolId()
		case subnetIDGetter:
			id = g.GetSubnetId()
		case parentIDGetter:
			id = g.GetParentId()
		}
		if id == "" {
			return "", errs.BadRequest(errs.Violation{Field: field, Message: "required"})
		}
		return id, nil
	}}
}
