// Package domain holds the IAM business logic. It is transport-free and depends
// only on the ports declared in this file.
package domain

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// Resource type prefixes (NID types) owned by IAM.
const (
	KindTenant = "tenant"
	KindGroup  = "group"
)

// TenantRepository stores tenants (iam.v1.Container).
type TenantRepository interface {
	Get(ctx context.Context, id string) (*iampb.Container, error)
	// Ensure inserts the tenant if no tenant with its ID exists.
	Ensure(ctx context.Context, t *iampb.Container) error
}

// GroupPage is one page of a group listing.
type GroupPage struct {
	Items         []*iampb.Group
	NextPageToken string
}

// GroupRepository stores groups.
type GroupRepository interface {
	Get(ctx context.Context, id string) (*iampb.Group, error)
	GetByName(ctx context.Context, parentID, name string) (*iampb.Group, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (GroupPage, error)
	// Create fails with errs.AlreadyExists if the name is taken in the parent.
	Create(ctx context.Context, g *iampb.Group) error
	// Update fails with errs.NotFound if the group does not exist.
	Update(ctx context.Context, g *iampb.Group) error
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
