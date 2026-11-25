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
	KindTenant            = "tenant"
	KindGroup             = "group"
	KindGroupMembership   = "groupmembership"
	KindAccessPermit      = "accesspermit"
	KindTenantUserAccount = "tenantuseraccount"
	KindServiceAccount    = "serviceaccount"
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

// MembershipPage is one page of a membership listing.
type MembershipPage struct {
	Items         []*iampb.GroupMembership
	NextPageToken string
}

// MembershipRepository stores group memberships.
type MembershipRepository interface {
	Get(ctx context.Context, id string) (*iampb.GroupMembership, error)
	// Create fails with errs.AlreadyExists if the group already has the member.
	Create(ctx context.Context, m *iampb.GroupMembership) error
	Delete(ctx context.Context, id string) error
	ListByGroup(ctx context.Context, groupID string, pageSize int, pageToken string) (MembershipPage, error)
	// ListByMember returns every membership of a principal, across groups.
	ListByMember(ctx context.Context, memberID string) ([]*iampb.GroupMembership, error)
}

// PermitPage is one page of an access-permit listing.
type PermitPage struct {
	Items         []*iampb.AccessPermit
	NextPageToken string
}

// PermitRepository stores access permits.
type PermitRepository interface {
	Get(ctx context.Context, id string) (*iampb.AccessPermit, error)
	// Create fails with errs.AlreadyExists if the group already has the same
	// role on the same resource.
	Create(ctx context.Context, p *iampb.AccessPermit) error
	Delete(ctx context.Context, id string) error
	ListByGroup(ctx context.Context, groupID string, pageSize int, pageToken string) (PermitPage, error)
}

// Hierarchy resolves resource ancestry (satisfied by *authz.Engine).
type Hierarchy interface {
	// Root returns the top-most ancestor (the tenant) of id, or errs.NotFound.
	Root(ctx context.Context, id string) (string, error)
}

// RoleCatalog knows which roles exist (satisfied by *authz.Catalog).
type RoleCatalog interface {
	Has(role string) bool
}
