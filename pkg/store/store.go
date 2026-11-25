// Package store is NebiusMocker's storage engine: a transactional record store
// with name and parent indexes.
//
// Only DAL packages and composition roots may import it (enforced by depguard).
// Records hold proto-marshaled bytes, so the store is independent of message types.
package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Tx.Get and Tx.ByName when no record matches.
var ErrNotFound = errors.New("store: record not found")

// ErrNameTaken matches (via errors.Is) the *NameTakenError returned by Tx.Put
// when another record of the same kind and parent already uses the name.
var ErrNameTaken = errors.New("store: name already used in parent")

// NameTakenError reports which record holds the name.
type NameTakenError struct {
	Kind, ParentID, Name, ExistingID string
}

// Error implements error.
func (e *NameTakenError) Error() string {
	return "store: " + e.Kind + " name " + e.Name + " in " + e.ParentID + " is used by " + e.ExistingID
}

// Is makes errors.Is(err, ErrNameTaken) true.
func (e *NameTakenError) Is(target error) bool { return target == ErrNameTaken }

// Record is one stored resource.
type Record struct {
	Kind     string // resource type prefix, e.g. "group"
	ID       string
	ParentID string
	Name     string // optional; unique per (Kind, ParentID) when non-empty
	Data     []byte // proto-marshaled resource
	// Seq is assigned by the store on first insert and gives a stable creation order.
	Seq uint64
}

// ListQuery selects records of one kind under one parent (or under any parent
// when AnyParent is set), after a cursor. AnyKind ignores Kind and selects the
// records of every kind under ParentID (e.g. "is this project empty?").
type ListQuery struct {
	Kind      string
	ParentID  string
	AnyParent bool
	AnyKind   bool
	AfterSeq  uint64 // exclusive; 0 starts from the beginning
	Limit     int    // 0 means no limit
}

// Tx is a read-write view of the store inside a transaction.
type Tx interface {
	Get(kind, id string) (Record, error)
	ByName(kind, parentID, name string) (Record, error)
	List(q ListQuery) ([]Record, error)
	Put(rec Record) (Record, error)
	Delete(kind, id string) error
}

// Store runs transactions. Update transactions are serializable; a non-nil error
// from fn discards every write made in it.
type Store interface {
	View(ctx context.Context, fn func(tx Tx) error) error
	Update(ctx context.Context, fn func(tx Tx) error) error
}
