// Package arch enforces NebiusMocker's layering rules that import-ban linters
// cannot express (see the go-lint skill and ADR-0008):
//
//  1. services/<A> must not import services/<B> for B ≠ A.
//  2. services/<A>/api must not import services/<A>/dal.
//  3. services/<A>/domain must not import services/<A>/api or services/<A>/dal.
//  4. services/<A>/dal must not import services/<A>/api.
//  5. pkg/... must not import services/...
//  6. Only cmd/... and test/... may import more than one service area.
//  7. Every services/<A>/domain package has a ports.go file.
package arch

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Module is the module path all rules are relative to.
const Module = "github.com/kfirgollan/NebiusMocker"

// Package is the minimal view of a Go package the rules need.
type Package struct {
	Path    string   // import path
	Imports []string // direct imports
	Files   []string // Go file base names (non-test and test)
}

// Violation is one broken rule.
type Violation struct {
	Rule    int
	Package string
	Detail  string
}

func (v Violation) String() string {
	return fmt.Sprintf("rule %d: %s: %s", v.Rule, v.Package, v.Detail)
}

// exceptions lists allowed edges as "from → to", each with a reason and issue.
var exceptions = map[string]string{}

// Check applies every rule to pkgs.
func Check(pkgs []Package) []Violation {
	var out []Violation
	for _, p := range pkgs {
		rel, ok := relative(p.Path)
		if !ok {
			continue
		}
		out = append(out, checkImports(rel, p)...)
		if area, layer := serviceLayer(rel); area != "" && layer == "domain" && !slices.Contains(p.Files, "ports.go") {
			out = append(out, Violation{7, rel, "domain package has no ports.go"})
		}
	}
	slices.SortFunc(out, func(a, b Violation) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func checkImports(rel string, p Package) []Violation {
	var out []Violation
	fromArea, fromLayer := serviceLayer(rel)
	areas := map[string]bool{}
	for _, imp := range p.Imports {
		to, ok := relative(imp)
		if !ok {
			continue
		}
		if _, allowed := exceptions[rel+" → "+to]; allowed {
			continue
		}
		toArea, toLayer := serviceLayer(to)
		if toArea != "" {
			areas[toArea] = true
		}
		if v, bad := edgeViolation(rel, fromArea, fromLayer, to, toArea, toLayer); bad {
			out = append(out, v)
		}
	}
	if len(areas) > 1 && !strings.HasPrefix(rel, "cmd/") && !strings.HasPrefix(rel, "test/") {
		out = append(out, Violation{6, rel, fmt.Sprintf("imports %d service areas", len(areas))})
	}
	return out
}

func edgeViolation(rel, fromArea, fromLayer, to, toArea, toLayer string) (Violation, bool) {
	switch {
	case strings.HasPrefix(rel, "pkg/") && toArea != "":
		return Violation{5, rel, "pkg must not import services: " + to}, true
	case fromArea == "" || toArea == "":
		return Violation{}, false
	case fromArea != toArea:
		return Violation{1, rel, "cross-area import: " + to}, true
	case fromLayer == "api" && toLayer == "dal":
		return Violation{2, rel, "api must not import dal: " + to}, true
	case fromLayer == "domain" && (toLayer == "api" || toLayer == "dal"):
		return Violation{3, rel, "domain must not import " + toLayer + ": " + to}, true
	case fromLayer == "dal" && toLayer == "api":
		return Violation{4, rel, "dal must not import api: " + to}, true
	}
	return Violation{}, false
}

// relative strips the module prefix; ok is false for external packages.
func relative(path string) (string, bool) {
	if path == Module {
		return "", true
	}
	rel, ok := strings.CutPrefix(path, Module+"/")
	return rel, ok
}

// serviceLayer splits "services/<area>/<layer>/..." into area and layer
// ("" for the module root package of the area).
func serviceLayer(rel string) (area, layer string) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || parts[0] != "services" {
		return "", ""
	}
	if len(parts) == 2 {
		return parts[1], ""
	}
	return parts[1], parts[2]
}
