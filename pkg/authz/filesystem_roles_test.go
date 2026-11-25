package authz_test

import (
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

// Exposing a filesystem through a filesystem bucket needs the editor role or
// compute.use-via-object-storage; viewers and auditors cannot.
func TestUseViaObjectStoragePermission(t *testing.T) {
	t.Parallel()
	c := authz.DefaultCatalog()
	perm := authz.Perm("compute", "filesystems", authz.VerbUseViaObjectStorage)
	if perm != "compute.filesystems.useViaObjectStorage" {
		t.Fatalf("Perm = %q", perm)
	}
	for role, want := range map[string]bool{
		authz.RoleEditor:                     true,
		authz.RoleComputeUseViaObjectStorage: true,
		authz.RoleAdmin:                      true,
		authz.RoleViewer:                     false,
		authz.RoleAuditor:                    false,
	} {
		if got := c.Allows(role, perm); got != want {
			t.Errorf("%s allows %s = %v, want %v", role, perm, got, want)
		}
	}
	// The dedicated role grants nothing else.
	for _, p := range []authz.Permission{"compute.filesystems.get", "compute.filesystems.update", "compute.instances.create"} {
		if c.Allows(authz.RoleComputeUseViaObjectStorage, p) {
			t.Errorf("%s allows %s, want only useViaObjectStorage", authz.RoleComputeUseViaObjectStorage, p)
		}
	}
}
