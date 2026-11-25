package api

import (
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

// Service names of the audit gRPC services served by this module.
const (
	eventService  = "nebius.audit.v2.AuditEventService"
	exportService = "nebius.audit.v2.AuditEventExportService"
)

// verbCancel is the permission verb of AuditEventExportService.Cancel.
const verbCancel = "cancel"

// RegisterAuthzRules declares which permission each audit RPC needs.
func RegisterAuthzRules(r *authz.Rules) {
	r.Add("/"+eventService+"/List", authz.OnParent(authz.Perm("audit", "auditEvents", authz.VerbList)))

	e := func(method string) string { return "/" + exportService + "/" + method }
	r.Add(e("Start"), authz.OnMetadataParent(authz.Perm("audit", "auditEventExports", authz.VerbCreate)))
	r.Add(e("Cancel"), authz.OnID(authz.Perm("audit", "auditEventExports", verbCancel)))
	r.Add(e("Get"), authz.OnID(authz.Perm("audit", "auditEventExports", authz.VerbGet)))
	r.Add(e("List"), authz.OnParent(authz.Perm("audit", "auditEventExports", authz.VerbList)))
}
