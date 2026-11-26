// Package errs defines transport-free domain error kinds.
//
// Domain and DAL code return these errors; only the api layer maps them to
// gRPC status codes and nebius.common.v1.ServiceError details (see
// pkg/serviceerror). The kinds mirror the ServiceError detail types.
package errs

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies a domain error.
type Kind int

// Error kinds. KindUnknown is the zero value and means "not a domain error".
const (
	KindUnknown Kind = iota
	KindNotFound
	KindAlreadyExists
	KindBadRequest
	KindBadState
	KindConflict
	KindOperationConflict
	KindPermissionDenied
	KindUnauthenticated
	KindQuotaExceeded
	KindInternal
)

var kindNames = map[Kind]string{
	KindUnknown:           "unknown",
	KindNotFound:          "not_found",
	KindAlreadyExists:     "already_exists",
	KindBadRequest:        "bad_request",
	KindBadState:          "bad_state",
	KindConflict:          "conflict",
	KindOperationConflict: "operation_conflict",
	KindPermissionDenied:  "permission_denied",
	KindUnauthenticated:   "unauthenticated",
	KindQuotaExceeded:     "quota_exceeded",
	KindInternal:          "internal",
}

// String returns the snake_case name of the kind.
func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Violation describes one invalid field, using the proto field path
// (for example "metadata.parent_id").
type Violation struct {
	Field   string
	Message string
}

// Error is a domain error. Use the constructors rather than building it directly.
type Error struct {
	Kind       Kind
	ResourceID string
	// OperationID is set for KindOperationConflict: the operation already running.
	OperationID string
	Message     string
	Violations  []Violation
	// QuotaViolations are set for KindQuotaExceeded: the exhausted quotas.
	QuotaViolations []QuotaViolation
}

// QuotaViolation describes one exhausted quota (nebius.common.v1.QuotaFailure.Violation).
type QuotaViolation struct {
	Quota     string // quota name, e.g. "compute.instance.count"
	Message   string
	Limit     string // the allowance's limit
	Requested string // the amount the request would add
	Region    string
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Kind.String())
	if e.ResourceID != "" {
		b.WriteString(" ")
		b.WriteString(e.ResourceID)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	for _, v := range e.Violations {
		fmt.Fprintf(&b, "; %s: %s", v.Field, v.Message)
	}
	return b.String()
}

// NotFound reports that resource id does not exist (or is not visible).
func NotFound(id string) *Error {
	return &Error{Kind: KindNotFound, ResourceID: id, Message: "resource not found"}
}

// AlreadyExists reports a uniqueness collision with the existing resource id.
func AlreadyExists(existingID, msg string) *Error {
	return &Error{Kind: KindAlreadyExists, ResourceID: existingID, Message: msg}
}

// BadRequest reports one or more invalid request fields.
func BadRequest(violations ...Violation) *Error {
	return &Error{Kind: KindBadRequest, Message: "invalid request", Violations: violations}
}

// BadState reports that resource id is not in a state that allows the request.
func BadState(id, msg string) *Error {
	return &Error{Kind: KindBadState, ResourceID: id, Message: msg}
}

// Conflict reports an optimistic-concurrency conflict (resource_version mismatch).
func Conflict(id, msg string) *Error {
	return &Error{Kind: KindConflict, ResourceID: id, Message: msg}
}

// OperationConflict reports that operation opID is already running on resource id.
func OperationConflict(opID, id string) *Error {
	return &Error{
		Kind: KindOperationConflict, ResourceID: id, OperationID: opID,
		Message: "another operation is in progress for this resource",
	}
}

// PermissionDenied reports that the caller may not act on resource id.
func PermissionDenied(id string) *Error {
	return &Error{Kind: KindPermissionDenied, ResourceID: id, Message: "permission denied"}
}

// Unauthenticated reports missing or invalid credentials.
func Unauthenticated(msg string) *Error {
	return &Error{Kind: KindUnauthenticated, Message: msg}
}

// QuotaExceeded reports that a request would exceed one or more quota allowances.
func QuotaExceeded(msg string, violations ...QuotaViolation) *Error {
	return &Error{Kind: KindQuotaExceeded, Message: msg, QuotaViolations: violations}
}

// Internal reports an unexpected failure. msg must not contain sensitive data.
func Internal(msg string) *Error {
	return &Error{Kind: KindInternal, Message: msg}
}

// KindOf returns the kind of the first *Error in err's chain, or KindUnknown.
func KindOf(err error) Kind {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind
	}
	return KindUnknown
}

// IsKind reports whether err's chain contains a domain error of kind k.
func IsKind(err error, k Kind) bool {
	return err != nil && KindOf(err) == k
}
