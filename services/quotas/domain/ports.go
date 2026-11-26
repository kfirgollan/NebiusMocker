// Package domain implements Nebius quota allowances: the QuotaAllowance
// resources of tenants and projects, their live usage and the quota gate that
// other areas pass their creates through.
package domain

import (
	"context"

	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Resource kinds (NID prefixes).
const (
	KindAllowance = "quotaallowance"
	KindTenant    = "tenant"
	KindProject   = "project"
)

// Allowance is a stored quota allowance. Region is the region it applies to;
// Msg.Spec.Region echoes what the client sent (empty when it relied on the
// project's region).
type Allowance struct {
	Msg    *quotaspb.QuotaAllowance
	Region string
}

// Page is one page of allowances.
type Page struct {
	Items         []Allowance
	NextPageToken string
}

// Repository stores allowances. Implemented in dal.
type Repository interface {
	Get(ctx context.Context, id string) (Allowance, error)
	// ByKey returns the allowance of (parentID, name, region) or errs.NotFound.
	ByKey(ctx context.Context, parentID, name, region string) (Allowance, error)
	// List returns one page of a container's allowances in creation order.
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (Page, error)
	// Ensure inserts the allowances that do not exist yet (by ID) and leaves
	// existing ones unchanged.
	Ensure(ctx context.Context, as []Allowance) error
	// Put replaces an existing allowance.
	Put(ctx context.Context, a Allowance) error
}

// Hierarchy resolves resource parents (deps.Authz): a project's tenant, and
// whether a container exists.
type Hierarchy interface {
	Parent(ctx context.Context, id string) (string, error)
}

// ProjectRegions resolves a project's region (peer.ProjectRegions).
type ProjectRegions interface {
	Region(ctx context.Context, projectID string) (string, error)
}

// UsageSources returns the areas' usage sources by area name, looked up at
// call time (peer.QuotaUsageSources).
type UsageSources func() map[string]peer.QuotaUsageSource

// Operations submits long-running operations.
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// Region is one emulated region and its NID routing code.
type Region struct {
	Name        string
	RoutingCode string
}
