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
