package authz_test

import (
	"context"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func TestDefaultRoles(t *testing.T) {
	t.Parallel()
	c := authz.DefaultCatalog()
	tests := []struct {
		role string
		perm authz.Permission
		want bool
	}{
		{authz.RoleViewer, "iam.groups.get", true},
		{authz.RoleViewer, "iam.groups.list", true},
		{authz.RoleViewer, "iam.groups.create", false},
		{authz.RoleEditor, "iam.groups.create", true},
		{authz.RoleEditor, "compute.instances.delete", true},
		{authz.RoleEditor, "iam.accessPermits.create", false},
		{authz.RoleEditor, "iam.groupMemberships.delete", false},
		{authz.RoleEditor, "iam.accessPermits.get", true},
		{authz.RoleViewer, "kms.symmetricKeys.get", true},
		{authz.RoleViewer, "kms.asymmetricKeys.get", true},
		{authz.RoleViewer, "kms.symmetricKeys.encrypt", false},
		{authz.RoleViewer, "kms.symmetricKeys.decrypt", false},
		{authz.RoleViewer, "kms.asymmetricKeys.sign", false},
		{authz.RoleViewer, "kms.asymmetricKeys.decrypt", false},
		{authz.RoleViewer, "kms.symmetricKeys.rotate", false},
		{authz.RoleViewer, "kms.symmetricKeys.undelete", false},
		{authz.RoleEditor, "kms.symmetricKeys.encrypt", true},
		{authz.RoleEditor, "kms.symmetricKeys.decrypt", true},
		{authz.RoleEditor, "kms.asymmetricKeys.sign", true},
		{authz.RoleEditor, "kms.asymmetricKeys.decrypt", true},
		{authz.RoleEditor, "kms.symmetricKeys.rotate", true},
		{authz.RoleEditor, "kms.asymmetricKeys.undelete", true},
		{authz.RoleEditor, "compute.disks.encrypt", false}, // the KMS grants do not leak to other services
		{authz.RoleAdmin, "kms.symmetricKeys.decrypt", true},
		{authz.RoleAdmin, "iam.accessPermits.create", true},
		{"nobody", "iam.groups.get", false},
		{authz.RoleViewer, "mysterybox.secrets.get", true},
		{authz.RoleViewer, "mysterybox.payload.read", false},
		{authz.RoleEditor, "mysterybox.secrets.create", true},
		{authz.RoleEditor, "mysterybox.payload.read", false},
		{authz.RoleAdmin, "mysterybox.payload.read", true},
		{authz.RoleMysteryBoxPayloadViewer, "mysterybox.payload.read", true},
		{authz.RoleMysteryBoxPayloadViewer, "mysterybox.secrets.get", false},
	}
	for _, tt := range tests {
		if got := c.Allows(tt.role, tt.perm); got != tt.want {
			t.Errorf("%s allows %s = %v, want %v", tt.role, tt.perm, got, tt.want)
		}
	}
}

// bindings grants roles from a fixed table: principal → resource → roles.
type bindings map[string]map[string][]string

func (b bindings) RolesOn(_ context.Context, principal, resource string) ([]string, error) {
	return b[principal][resource], nil
}

func newEngine() *authz.Engine {
	e := authz.NewEngine(authz.DefaultCatalog())
	parents := map[string]string{"group-a": "tenant-t", "groupmembership-m": "group-a"}
	parent := func(_ context.Context, id string) (string, error) {
		if id == "tenant-t" {
			return "", nil
		}
		p, ok := parents[id]
		if !ok {
			return "", errs.NotFound(id)
		}
		return p, nil
	}
	for _, kind := range []string{"tenant", "group", "groupmembership"} {
		e.RegisterKind(kind, parent)
	}
	e.SetBindings(bindings{
		"alice": {"tenant-t": {authz.RoleViewer}},
		"bob":   {"group-a": {authz.RoleEditor}},
	})
	return e
}

func TestEngineInheritance(t *testing.T) {
	t.Parallel()
	e := newEngine()
	tests := []struct {
		principal, target string
		perm              authz.Permission
		want              bool
	}{
		{"alice", "groupmembership-m", "iam.groupMemberships.get", true}, // viewer on tenant reaches grandchild
		{"alice", "group-a", "iam.groups.delete", false},
		{"bob", "groupmembership-m", "iam.groups.update", true}, // editor on group reaches child
		{"bob", "tenant-t", "iam.groups.create", false},         // but not the parent
		{"carol", "group-a", "iam.groups.get", false},
	}
	for _, tt := range tests {
		d, err := e.Check(t.Context(), tt.principal, tt.perm, tt.target)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if d.Allowed != tt.want {
			t.Errorf("%s %s on %s = %v (%s), want %v", tt.principal, tt.perm, tt.target, d.Allowed, d.Reason, tt.want)
		}
	}
}

func TestEngineUnknownTarget(t *testing.T) {
	t.Parallel()
	_, err := newEngine().Check(t.Context(), "alice", "iam.groups.get", "group-missing")
	if !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("err = %v, want NotFound", err)
	}
	root, err := newEngine().Root(t.Context(), "groupmembership-m")
	if err != nil || root != "tenant-t" {
		t.Fatalf("Root = %q, %v; want tenant-t", root, err)
	}
}
