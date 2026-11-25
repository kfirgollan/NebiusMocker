package authz_test

import (
	"context"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

func TestEngineHasKind(t *testing.T) {
	t.Parallel()
	e := authz.NewEngine(authz.DefaultCatalog())
	if e.HasKind("vpcsubnet") {
		t.Fatal("HasKind before RegisterKind = true")
	}
	e.RegisterKind("vpcsubnet", func(context.Context, string) (string, error) { return "", nil })
	if !e.HasKind("vpcsubnet") {
		t.Fatal("HasKind after RegisterKind = false")
	}
}

func TestLifecycleVerbsGrantedToEditors(t *testing.T) {
	t.Parallel()
	c := authz.DefaultCatalog()
	for _, verb := range []string{authz.VerbStart, authz.VerbStop, authz.VerbRestart} {
		p := authz.Perm("ai", "endpoints", verb)
		if !c.Allows(authz.RoleEditor, p) || !c.Allows(authz.RoleAdmin, p) {
			t.Errorf("%s: editor and admin must be allowed", p)
		}
		if c.Allows(authz.RoleViewer, p) {
			t.Errorf("%s: viewer must not be allowed", p)
		}
	}
}
