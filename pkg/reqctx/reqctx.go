// Package reqctx carries request-scoped metadata (request ID, principal) in a
// context.Context behind typed accessors. It is transport-free, so domain code
// may read the principal (for example to fill Operation.created_by).
package reqctx

import "context"

type key struct{}

// Principal identifies the authenticated caller.
type Principal struct {
	// ID is the NID of the caller (user account or service account), or
	// empty for anonymous callers.
	ID string
}

// Anonymous reports whether the principal is unauthenticated.
func (p Principal) Anonymous() bool { return p.ID == "" }

// Info is the per-request record. The outermost interceptor installs it, inner
// interceptors fill it before the handler runs, and outer interceptors (such as
// logging) read it after the handler returns. It is not safe to mutate it
// concurrently with handler execution.
type Info struct {
	RequestID string
	Principal Principal
	// Denied is set by the authorization interceptor when it rejects the
	// request, so outer interceptors (audit) can tell a denial from a
	// handler error with the same code.
	Denied bool
}

// WithInfo returns ctx carrying info.
func WithInfo(ctx context.Context, info *Info) context.Context {
	return context.WithValue(ctx, key{}, info)
}

// InfoFrom returns the request record, or nil outside a request.
func InfoFrom(ctx context.Context) *Info {
	info, _ := ctx.Value(key{}).(*Info)
	return info
}

// RequestID returns the request ID, or "" if none is set.
func RequestID(ctx context.Context) string {
	if info := InfoFrom(ctx); info != nil {
		return info.RequestID
	}
	return ""
}

// PrincipalFrom returns the caller's principal (anonymous if none is set).
func PrincipalFrom(ctx context.Context) Principal {
	if info := InfoFrom(ctx); info != nil {
		return info.Principal
	}
	return Principal{}
}
