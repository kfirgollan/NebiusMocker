// Package api is the mk8s gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

// Service names of the mk8s gRPC services served by this module.
const (
	clusterService   = "nebius.mk8s.v1.ClusterService"
	nodeGroupService = "nebius.mk8s.v1.NodeGroupService"
)

func anyone(context.Context, string, any) bool { return true }

func noTarget(context.Context, any) (string, error) { return "", nil }

type preflightRequest interface {
	GetMetadata() *commonpb.ResourceMetadata
}

// RegisterAuthzRules declares the permission each mk8s RPC needs:
// mk8s.clusters.* on the cluster (or its project) and mk8s.nodeGroups.* on
// the node group (or its cluster). The version catalog and the node
// compatibility matrix are public to any authenticated principal.
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(clusterService, "mk8s", "clusters")
	r.Add("/"+clusterService+"/ListControlPlaneVersions", authz.Rule{
		Permission: authz.Perm("mk8s", "clusters", authz.VerbList), AllowIf: anyone, Target: noTarget,
	})

	r.CRUD(nodeGroupService, "mk8s", "nodeGroups")
	m := func(method string) string { return "/" + nodeGroupService + "/" + method }
	r.Add(m("Upgrade"), authz.OnID(authz.Perm("mk8s", "nodeGroups", authz.VerbUpdate)))
	r.Add(m("GetCompatibilityMatrix"), authz.Rule{
		Permission: authz.Perm("mk8s", "nodeGroups", authz.VerbGet), AllowIf: anyone, Target: noTarget,
	})
	// A preflight reads the node group it would update, or its cluster.
	// Terraform also plans node groups whose cluster does not exist yet
	// (unknown parent): then any authenticated principal may ask.
	r.Add(m("PreflightCheck"), authz.Rule{
		Permission: authz.Perm("mk8s", "nodeGroups", authz.VerbGet),
		AllowIf: func(_ context.Context, _ string, req any) bool {
			p, ok := req.(preflightRequest)
			return !ok || (p.GetMetadata().GetId() == "" && p.GetMetadata().GetParentId() == "")
		},
		Target: func(_ context.Context, req any) (string, error) {
			p, ok := req.(preflightRequest)
			if !ok {
				return "", nil // admitted by AllowIf
			}
			md := p.GetMetadata()
			if md.GetId() != "" {
				return md.GetId(), nil
			}
			return md.GetParentId(), nil
		},
	})
}
