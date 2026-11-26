// Package api is the Managed PostgreSQL gRPC view layer: it converts between
// protos and domain inputs and maps domain errors. It makes no business
// decisions.
package api

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Service names of the gRPC services served by this module.
const (
	clusterService = "nebius.msp.postgresql.v1alpha1.ClusterService"
	backupService  = "nebius.msp.postgresql.v1alpha1.BackupService"
)

type clusterIDGetter interface{ GetClusterId() string }

// onClusterID checks perm on the request's cluster_id field: a backup is
// authorized through its source cluster.
func onClusterID(perm authz.Permission) authz.Rule {
	return authz.Rule{Permission: perm, Target: func(_ context.Context, req any) (string, error) {
		if g, ok := req.(clusterIDGetter); ok && g.GetClusterId() != "" {
			return g.GetClusterId(), nil
		}
		return "", errs.BadRequest(errs.Violation{Field: "cluster_id", Message: "required"})
	}}
}

// RegisterAuthzRules declares the permission each RPC needs:
// postgresql.clusters.* on the cluster (or its project) and
// postgresql.backups.* on the backup's source cluster (or the project, for
// List). Stop and Start need the stop and start verbs, Restore creates a
// cluster in the project of the new cluster.
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(clusterService, "postgresql", "clusters")
	c := func(method string) string { return "/" + clusterService + "/" + method }
	r.Add(c("GetForBackup"), authz.OnID(authz.Perm("postgresql", "clusters", authz.VerbGet)))
	r.Add(c("Restore"), authz.OnMetadataParent(authz.Perm("postgresql", "clusters", authz.VerbCreate)))
	r.Add(c("Stop"), authz.OnID(authz.Perm("postgresql", "clusters", authz.VerbStop)))
	r.Add(c("Start"), authz.OnID(authz.Perm("postgresql", "clusters", authz.VerbStart)))

	b := func(method string) string { return "/" + backupService + "/" + method }
	r.Add(b("Get"), onClusterID(authz.Perm("postgresql", "backups", authz.VerbGet)))
	r.Add(b("List"), authz.OnParent(authz.Perm("postgresql", "backups", authz.VerbList)))
	r.Add(b("ListByCluster"), onClusterID(authz.Perm("postgresql", "backups", authz.VerbList)))
	r.Add(b("Create"), onClusterID(authz.Perm("postgresql", "backups", authz.VerbCreate)))
	r.Add(b("Delete"), onClusterID(authz.Perm("postgresql", "backups", authz.VerbDelete)))
}
