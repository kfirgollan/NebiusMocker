package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// maxDepth bounds ancestry walks so a broken parent chain cannot loop forever.
const maxDepth = 16

// Mode controls enforcement.
type Mode int

// Enforcement modes.
const (
	// ModeAudit evaluates and logs every decision but never blocks.
	ModeAudit Mode = iota
	// ModeOff skips authorization.
	ModeOff
	// ModeEnforce blocks denied requests.
	ModeEnforce
)

// ParentFunc returns the parent ID of a resource ("" for a root such as a
// tenant), or an errs.NotFound error if the resource does not exist.
type ParentFunc func(ctx context.Context, id string) (string, error)

// Bindings reports which roles a principal holds directly on one resource
// (no inheritance; the engine walks ancestors itself).
type Bindings interface {
	RolesOn(ctx context.Context, principalID, resourceID string) ([]string, error)
}

// Decision is the outcome of a check.
type Decision struct {
	Allowed    bool
	Permission Permission
	Target     string
	// Reason explains the outcome, e.g. "role editor on tenant-…".
	Reason string
}

// Engine evaluates permissions. Modules register resource kinds and the IAM
// module installs the bindings source. It is safe for concurrent use.
type Engine struct {
	catalog *Catalog

	mu       sync.RWMutex
	parents  map[string]ParentFunc // NID type prefix → parent lookup
	bindings Bindings
}

// NewEngine returns an engine over the role catalog.
func NewEngine(catalog *Catalog) *Engine {
	return &Engine{catalog: catalog, parents: map[string]ParentFunc{}}
}

// Catalog returns the role catalog.
func (e *Engine) Catalog() *Catalog { return e.catalog }

// RegisterKind tells the engine how to find the parent of resources whose
// NID type prefix is kind (e.g. "group").
func (e *Engine) RegisterKind(kind string, parent ParentFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.parents[kind] = parent
}

// SetBindings installs the bindings source (the IAM module).
func (e *Engine) SetBindings(b Bindings) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bindings = b
}

// Check decides whether principalID holds perm on target or any ancestor.
// It returns an error only when the decision cannot be made, e.g. the target
// does not exist (errs.NotFound).
func (e *Engine) Check(ctx context.Context, principalID string, perm Permission, target string) (Decision, error) {
	d := Decision{Permission: perm, Target: target}
	e.mu.RLock()
	bindings := e.bindings
	e.mu.RUnlock()
	if bindings == nil {
		d.Reason = "no bindings source (the iam module is not enabled)"
		return d, nil
	}
	id := target
	for range maxDepth {
		roles, err := bindings.RolesOn(ctx, principalID, id)
		if err != nil {
			return d, fmt.Errorf("roles of %s on %s: %w", principalID, id, err)
		}
		for _, r := range roles {
			if e.catalog.Allows(r, perm) {
				d.Allowed = true
				d.Reason = "role " + r + " on " + id
				return d, nil
			}
		}
		parent, err := e.Parent(ctx, id)
		if err != nil {
			return d, err
		}
		if parent == "" {
			d.Reason = "no role granting " + string(perm) + " on " + target + " or its ancestors"
			return d, nil
		}
		id = parent
	}
	return d, fmt.Errorf("authz: ancestry of %s deeper than %d", target, maxDepth)
}

// Parent returns the registered parent of id ("" for roots).
func (e *Engine) Parent(ctx context.Context, id string) (string, error) {
	kind, _, ok := strings.Cut(id, "-")
	if !ok {
		return "", errs.NotFound(id)
	}
	e.mu.RLock()
	fn, known := e.parents[kind]
	e.mu.RUnlock()
	if !known {
		return "", errs.NotFound(id)
	}
	return fn(ctx, id)
}

// Root returns the top-most ancestor of id (normally its tenant).
func (e *Engine) Root(ctx context.Context, id string) (string, error) {
	for range maxDepth {
		parent, err := e.Parent(ctx, id)
		if err != nil {
			return "", err
		}
		if parent == "" {
			return id, nil
		}
		id = parent
	}
	return "", errors.New("authz: ancestry too deep")
}
