// Package operations implements the Nebius long-running operation model.
//
// Every mutating RPC returns a nebius.common.v1.Operation. The Engine runs a
// synchronous Prepare step (validation and the state change that must be visible
// immediately) under a per-resource lock, then completes the operation either
// inline (delay 0) or later on a single scheduler goroutine driven by an
// injected clock. Only one operation may run per resource at a time, matching
// the upstream rule "Nebius AI Cloud does not support concurrent operations on
// the same resource".
//
// This package is transport-free: the gRPC OperationService lives in
// pkg/operations/api.
package operations

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"
)

// Operation is the domain form of nebius.common.v1.Operation.
type Operation struct {
	ID          string
	Description string
	CreatedAt   time.Time
	CreatedBy   string
	// FinishedAt is zero while the operation is running.
	FinishedAt time.Time
	ResourceID string
	Request    proto.Message
	// Err is the failure of a finished operation; nil means success.
	Err error
}

// Done reports whether the operation has finished.
func (o *Operation) Done() bool { return !o.FinishedAt.IsZero() }

// clone returns a copy that callers may hold without racing the engine.
func (o *Operation) clone() *Operation {
	c := *o
	return &c
}

// Spec describes an operation to run.
type Spec struct {
	ResourceID  string
	Description string
	CreatedBy   string
	Request     proto.Message
	// Prepare runs synchronously under the resource lock. Its error is returned
	// from Submit (and so from the RPC) and no operation is created.
	Prepare func(ctx context.Context) error
	// Work runs when the operation completes. Its error fails the operation.
	Work func(ctx context.Context) error
	// Detached runs Work on its own goroutine instead of inline (zero delay)
	// or on the scheduler goroutine, for work that may block for long, such
	// as starting containers: Submit returns the operation still running,
	// and other operations keep completing meanwhile. The per-resource lock
	// still holds until Work returns. Work's ctx is canceled when the engine
	// stops; an operation interrupted that way stays unfinished, as after a
	// control-plane restart. Detached operations need a started engine.
	Detached bool
}

// Repository stores operations. Implemented in pkg/operations/dal.
type Repository interface {
	Put(ctx context.Context, op *Operation) error
	Get(ctx context.Context, id string) (*Operation, error)
	ListByResource(ctx context.Context, resourceID string) ([]*Operation, error)
}

// IDGenerator creates operation IDs.
type IDGenerator interface {
	New(typ string) (string, error)
}
