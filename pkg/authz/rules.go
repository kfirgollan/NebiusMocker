package authz

import (
	"context"
	"sync"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Rule says which permission an RPC needs and which resource it targets.
type Rule struct {
	Permission Permission
	// Target extracts the resource the permission is checked on.
	Target func(ctx context.Context, req any) (string, error)
	// AllowIf, when set, short-circuits to "allowed" (e.g. the creator of an
	// operation may always poll it, even after its resource is deleted).
	AllowIf func(ctx context.Context, principalID string, req any) bool
	// Also, when set, returns an extra permission this particular request
	// needs on the same target, or "" for none. Example: Get with view SECRET
	// also needs "<svc>.<resources>.getSensitiveData".
	Also func(req any) Permission
}

// Rules maps full gRPC method names ("/pkg.Service/Method") to rules.
// It is safe for concurrent use.
type Rules struct {
	mu    sync.RWMutex
	rules map[string]Rule
}

// NewRules returns an empty rule set.
func NewRules() *Rules { return &Rules{rules: map[string]Rule{}} }

// Add registers the rule for method.
func (r *Rules) Add(method string, rule Rule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules[method] = rule
}

// Lookup returns the rule for method.
func (r *Rules) Lookup(method string) (Rule, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rule, ok := r.rules[method]
	return rule, ok
}

type (
	idGetter       interface{ GetId() string }
	parentGetter   interface{ GetParentId() string }
	metadataGetter interface {
		GetMetadata() *commonpb.ResourceMetadata
	}
)

// OnID checks perm on the request's id field (Get, Delete).
func OnID(perm Permission) Rule {
	return Rule{Permission: perm, Target: func(_ context.Context, req any) (string, error) {
		if g, ok := req.(idGetter); ok && g.GetId() != "" {
			return g.GetId(), nil
		}
		return "", errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}}
}

// OnParent checks perm on the request's parent_id field (List, GetByName).
func OnParent(perm Permission) Rule {
	return Rule{Permission: perm, Target: func(_ context.Context, req any) (string, error) {
		if g, ok := req.(parentGetter); ok && g.GetParentId() != "" {
			return g.GetParentId(), nil
		}
		return "", errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}}
}

// OnMetadataParent checks perm on metadata.parent_id (Create).
func OnMetadataParent(perm Permission) Rule {
	return Rule{Permission: perm, Target: func(_ context.Context, req any) (string, error) {
		if g, ok := req.(metadataGetter); ok && g.GetMetadata().GetParentId() != "" {
			return g.GetMetadata().GetParentId(), nil
		}
		return "", errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}}
}

// OnMetadataID checks perm on metadata.id (Update).
func OnMetadataID(perm Permission) Rule {
	return Rule{Permission: perm, Target: func(_ context.Context, req any) (string, error) {
		if g, ok := req.(metadataGetter); ok && g.GetMetadata().GetId() != "" {
			return g.GetMetadata().GetId(), nil
		}
		return "", errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}}
}

// CRUD registers the standard Get/GetByName/List/Create/Update/Delete rules of
// service (full name, e.g. "nebius.iam.v1.GroupService") under the permission
// prefix "<svc>.<resources>".
func (r *Rules) CRUD(service, svc, resources string) {
	m := func(method string) string { return "/" + service + "/" + method }
	r.Add(m("Get"), OnID(Perm(svc, resources, VerbGet)))
	r.Add(m("GetByName"), OnParent(Perm(svc, resources, VerbGet)))
	r.Add(m("List"), OnParent(Perm(svc, resources, VerbList)))
	r.Add(m("Create"), OnMetadataParent(Perm(svc, resources, VerbCreate)))
	r.Add(m("Update"), OnMetadataID(Perm(svc, resources, VerbUpdate)))
	r.Add(m("Delete"), OnID(Perm(svc, resources, VerbDelete)))
}
