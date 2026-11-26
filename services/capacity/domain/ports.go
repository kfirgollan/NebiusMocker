// Package domain holds the Nebius capacity business logic: the capacity
// block groups (GPU reservations) and capacity intervals seeded from a
// catalog, the capacity allowances (project limits) users manage, the
// reservations compute consumers draw from block groups, and the resource
// advice computed from them. It is transport-free and depends only on the
// ports declared in this file.
package domain

import (
	"context"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// Resource type prefixes (NID types) owned by capacity, and the kinds it
// refers to.
const (
	KindBlockGroup = "capacityblockgroup"
	KindInterval   = "capacityinterval"
	KindAllowance  = "capacityallowance"
	// KindProject is the parent of an allowance (owned by IAM).
	KindProject = "project"
	// KindTenant is the parent of a block group (owned by IAM).
	KindTenant = "tenant"
)

// Page is one page of a listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// AllowanceRepository stores capacity allowances. At most one allowance
// exists per (project, block group).
type AllowanceRepository interface {
	// Get returns the allowance or errs.NotFound.
	Get(ctx context.Context, id string) (*capacitypb.CapacityAllowance, error)
	// ByProject returns every allowance of the project in creation order.
	ByProject(ctx context.Context, projectID string) ([]*capacitypb.CapacityAllowance, error)
	// ByBlockGroup returns every allowance of the block group, of any
	// project, in creation order.
	ByBlockGroup(ctx context.Context, blockGroupID string) ([]*capacitypb.CapacityAllowance, error)
	// Create inserts an allowance; errs.AlreadyExists (naming the existing
	// one) if the project already has an allowance for the block group.
	Create(ctx context.Context, a *capacitypb.CapacityAllowance) error
	// Update replaces an existing allowance; errs.NotFound if it does not
	// exist.
	Update(ctx context.Context, a *capacitypb.CapacityAllowance) error
	// Delete removes the allowance or returns errs.NotFound.
	Delete(ctx context.Context, id string) error
}

// Usage is the capacity one consumer (a compute instance) holds in a block
// group.
type Usage struct {
	ConsumerID   string `json:"consumer_id"`
	ProjectID    string `json:"project_id"`
	BlockGroupID string `json:"block_group_id"`
	Quantity     uint64 `json:"quantity"`
}

// UsageRepository stores reservations by consumer.
type UsageRepository interface {
	// Put records u, replacing the consumer's previous reservation.
	Put(ctx context.Context, u Usage) error
	// Delete removes the consumer's reservation; a missing one is not an
	// error.
	Delete(ctx context.Context, consumerID string) error
	// ByBlockGroup returns the reservations in a block group in creation
	// order.
	ByBlockGroup(ctx context.Context, blockGroupID string) ([]Usage, error)
}

// Hierarchy resolves resource ancestry (satisfied by *authz.Engine). It is
// how the domain finds a project's tenant without importing IAM.
type Hierarchy interface {
	// Parent returns the parent of id, or errs.NotFound if id does not exist.
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
