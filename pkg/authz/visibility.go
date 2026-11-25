package authz

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Visibility decides which items of a listing the caller may see. List RPCs
// whose results depend on per-item grants (projects of a tenant, tenants of a
// caller) are authorized as "any authenticated caller" and then filtered with
// it, so a principal granted on one project lists exactly that project.
//
// Filtering follows the enforcement mode: under ModeEnforce an item is visible
// when the caller holds the permission on it or an ancestor; under ModeAudit
// and ModeOff nothing is hidden (audit mode never changes responses).
type Visibility struct {
	engine *Engine
	mode   Mode
}

// NewVisibility returns a filter over engine's decisions in mode.
func NewVisibility(engine *Engine, mode Mode) *Visibility {
	return &Visibility{engine: engine, mode: mode}
}

// CanSee reports whether the caller of ctx holds perm on id. A resource that
// no longer exists (deleted while the listing ran) is not visible.
func (v *Visibility) CanSee(ctx context.Context, perm Permission, id string) (bool, error) {
	if v.mode != ModeEnforce {
		return true, nil
	}
	p := reqctx.PrincipalFrom(ctx)
	if p.Anonymous() {
		return false, nil
	}
	d, err := v.engine.Check(ctx, p.ID, perm, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return d.Allowed, nil
}
