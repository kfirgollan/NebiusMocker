package arch

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestRepositoryLayering loads every package in the module (with tests) and
// fails on any layering violation.
func TestRepositoryLayering(t *testing.T) {
	t.Parallel()
	cfg := &packages.Config{
		Context: t.Context(),
		Mode:    packages.NeedName | packages.NeedImports | packages.NeedFiles,
		Dir:     filepath.Join("..", ".."),
		Tests:   true,
	}
	loaded, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	byPath := map[string]*Package{}
	for _, lp := range loaded {
		for _, e := range lp.Errors {
			t.Errorf("load %s: %v", lp.PkgPath, e)
		}
		path := strings.TrimSuffix(lp.PkgPath, "_test") // external test packages share the rules
		p := byPath[path]
		if p == nil {
			p = &Package{Path: path}
			byPath[path] = p
		}
		for imp := range lp.Imports {
			p.Imports = append(p.Imports, imp)
		}
		for _, f := range lp.GoFiles {
			p.Files = append(p.Files, filepath.Base(f))
		}
	}
	if len(byPath) < 10 {
		t.Fatalf("loaded only %d packages; is the module root right?", len(byPath))
	}
	pkgs := make([]Package, 0, len(byPath))
	for _, p := range byPath {
		pkgs = append(pkgs, *p)
	}
	for _, v := range Check(pkgs) {
		t.Error(v)
	}
}

// TestRulesFire proves each rule detects its violation on a synthetic graph.
func TestRulesFire(t *testing.T) {
	t.Parallel()
	m := func(rel string) string { return Module + "/" + rel }
	pkgs := []Package{
		{Path: m("services/compute/domain"), Files: []string{"x.go"}, Imports: []string{m("services/vpc/domain"), m("services/compute/dal")}},
		{Path: m("services/compute/api"), Files: []string{"x.go"}, Imports: []string{m("services/compute/dal")}},
		{Path: m("services/compute/dal"), Files: []string{"x.go"}, Imports: []string{m("services/compute/api")}},
		{Path: m("pkg/store"), Files: []string{"x.go"}, Imports: []string{m("services/iam")}},
		{Path: m("pkg/mocker"), Files: []string{"x.go"}, Imports: []string{m("services/iam"), m("services/vpc")}},
		{Path: m("cmd/nebius-mocker"), Files: []string{"main.go"}, Imports: []string{m("services/iam"), m("services/vpc")}},
		{Path: m("services/iam/domain"), Files: []string{"ports.go"}, Imports: []string{"context"}},
	}
	fired := map[int]bool{}
	for _, v := range Check(pkgs) {
		fired[v.Rule] = true
	}
	for rule := 1; rule <= 7; rule++ {
		if !fired[rule] {
			t.Errorf("rule %d did not fire on its violation", rule)
		}
	}
}
