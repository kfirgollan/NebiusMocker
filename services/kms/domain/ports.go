// Package domain holds the KMS business logic: key lifecycle (create, update,
// rotate, delayed deletion, undelete) and the cryptographic operations. It is
// transport-free and depends only on the ports declared in this file.
package domain

import (
	"context"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// Resource type prefixes (NID types) owned by KMS.
const (
	KindSymmetricKey  = "kmssymkey"
	KindAsymmetricKey = "kmsasymkey"
	// KindProject is the only parent kind keys may live in.
	KindProject = "project"
)

// Page is one page of a key listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// KeyRepository stores keys of one kind (T is *kmspb.SymmetricKey or
// *kmspb.AsymmetricKey) together with their secret key material.
//
// Names are unique among the ACTIVE keys of a parent only: a key in any other
// state (SCHEDULED_FOR_DELETION) is not name-indexed, so its name may be reused
// and GetByName does not find it.
//
// Material is an opaque blob the domain encodes. Implementations must never log
// it or return it from anything but Material.
//
// Every write of an existing key is a compare-and-swap: prev is the key as the
// caller read it, and the write fails with errs.Conflict (changing nothing)
// unless the stored key is still equal to prev. Not every change bumps
// resource_version (deletion state, deletion delay), so the whole record is
// compared, not only the version.
type KeyRepository[T any] interface {
	// Get returns the key in any state, or errs.NotFound.
	Get(ctx context.Context, id string) (T, error)
	// GetByName returns the ACTIVE key named name in parentID, or errs.NotFound.
	GetByName(ctx context.Context, parentID, name string) (T, error)
	// List returns one page of the parent's keys (all states) in creation order.
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (Page[T], error)
	// Create stores a new key and its material atomically. It fails with
	// errs.AlreadyExists if an ACTIVE key of the parent has the same name.
	Create(ctx context.Context, key T, material []byte) error
	// Update replaces an existing key (not its material) if it still equals
	// prev. It fails with errs.NotFound if the key does not exist, errs.Conflict
	// if it changed, and errs.AlreadyExists if the key is ACTIVE and another
	// ACTIVE key of the parent has its name.
	Update(ctx context.Context, key, prev T) error
	// Material returns the key's material, or errs.NotFound.
	Material(ctx context.Context, id string) ([]byte, error)
	// UpdateWithMaterial replaces an existing key and its material atomically
	// (rotation) if the key still equals prev; errors as for Update.
	UpdateWithMaterial(ctx context.Context, key T, material []byte, prev T) error
	// Delete permanently removes the key and its material (purge) if the key
	// still equals prev; errs.NotFound or errs.Conflict otherwise.
	Delete(ctx context.Context, prev T) error
}

// SymmetricKeyRepository stores symmetric keys.
type SymmetricKeyRepository = KeyRepository[*kmspb.SymmetricKey]

// AsymmetricKeyRepository stores asymmetric keys.
type AsymmetricKeyRepository = KeyRepository[*kmspb.AsymmetricKey]

// Hierarchy resolves resource ancestry (satisfied by *authz.Engine).
type Hierarchy interface {
	// Parent returns the parent of id ("" for roots), or errs.NotFound if id
	// does not exist.
	Parent(ctx context.Context, id string) (string, error)
}

// Operations starts long-running operations (satisfied by *operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates NIDs (satisfied by pkg/nid generators).
type IDGenerator interface {
	New(typ string) (string, error)
}
