// Package domain holds the VPC business logic: IP address management (pools,
// subnets, allocations), routing and security groups. It is transport-free and
// depends only on the ports declared in this file.
package domain

import (
	"context"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// Resource type prefixes (NID types) owned by VPC.
const (
	KindPool          = "vpcpool"
	KindNetwork       = "vpcnetwork"
	KindSubnet        = "vpcsubnet"
	KindAllocation    = "vpcallocation"
	KindRouteTable    = "vpcroutetable"
	KindRoute         = "vpcroute"
	KindSecurityGroup = "vpcsecuritygroup"
	KindSecurityRule  = "vpcsecurityrule"
)

// KindProject is the IAM project prefix: the parent of most VPC resources.
const KindProject = "project"

// Resource is any Nebius VPC resource message.
type Resource interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// Page is one page of a listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// Repository stores one resource kind. One repository exists per kind.
type Repository[T Resource] interface {
	Get(ctx context.Context, id string) (T, error)
	GetByName(ctx context.Context, parentID, name string) (T, error)
	// List returns one page of the resources under parentID, in creation order.
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (Page[T], error)
	// ListWhere returns one page of the resources (any parent) for which match
	// is true, in creation order.
	ListWhere(ctx context.Context, match func(T) bool, pageSize int, pageToken string) (Page[T], error)
	// All returns every resource (any parent) for which match is true.
	All(ctx context.Context, match func(T) bool) ([]T, error)
	// Create fails with errs.AlreadyExists if the name is taken in the parent.
	Create(ctx context.Context, r T) error
	// Update fails with errs.NotFound if the resource does not exist.
	Update(ctx context.Context, r T) error
	Delete(ctx context.Context, id string) error
}

// Operations starts long-running operations (satisfied by *operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates NIDs (satisfied by pkg/nid generators).
type IDGenerator interface {
	New(typ string) (string, error)
}

// Hierarchy resolves resource ancestry (satisfied by *authz.Engine). VPC uses
// it to check that a parent project exists without importing IAM.
type Hierarchy interface {
	// Parent returns the parent of id ("" for roots), or errs.NotFound.
	Parent(ctx context.Context, id string) (string, error)
}
