package authz_test

import (
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

// Audit events expose every service's request payloads and principals, so
// only admin and auditor may read them or run exports.
func TestAuditPermissionsByRole(t *testing.T) {
	t.Parallel()
	c := authz.DefaultCatalog()
	perms := []authz.Permission{
		"audit.auditEvents.list",
		"audit.auditEventExports.create",
		"audit.auditEventExports.get",
		"audit.auditEventExports.list",
		"audit.auditEventExports.cancel",
	}
	want := map[string]bool{
		authz.RoleAdmin:   true,
		authz.RoleAuditor: true,
		authz.RoleEditor:  false,
		authz.RoleViewer:  false,
	}
	for role, allowed := range want {
		for _, p := range perms {
			if got := c.Allows(role, p); got != allowed {
				t.Errorf("%s allows %s = %v, want %v", role, p, got, allowed)
			}
		}
	}
	// The auditor reads like a viewer and changes nothing outside audit.
	for p, allowed := range map[authz.Permission]bool{
		"iam.groups.get":           true,
		"compute.instances.list":   true,
		"iam.groups.create":        false,
		"compute.instances.delete": false,
	} {
		if got := c.Allows(authz.RoleAuditor, p); got != allowed {
			t.Errorf("auditor allows %s = %v, want %v", p, got, allowed)
		}
	}
	if !c.Has(authz.RoleAuditor) {
		t.Error("catalog has no auditor role")
	}
}
