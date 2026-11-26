// Package domain holds the billing logic: pricing policies (prices of
// preemptible VMs), the spot market, and the cost calculator, all driven by
// one price list (prices.yaml, NEBIUS_MOCKER_BILLING_PRICES_FILE).
package domain

import (
	"context"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// Resource kinds (NID prefixes).
const (
	KindPricingPolicy = "pricingpolicy"
	KindProject       = "project"
)

// Page is one page of a listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// PricingPolicyRepository stores pricing policies (dal.PricingPolicyRepo).
// Get/GetByName/Update/Delete of a missing policy are errs.NotFound; Create
// and Update of a name taken in the parent are errs.AlreadyExists.
type PricingPolicyRepository interface {
	Get(ctx context.Context, id string) (*billingpb.PricingPolicy, error)
	GetByName(ctx context.Context, parentID, name string) (*billingpb.PricingPolicy, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (Page[*billingpb.PricingPolicy], error)
	Create(ctx context.Context, p *billingpb.PricingPolicy) error
	Update(ctx context.Context, p *billingpb.PricingPolicy) error
	Delete(ctx context.Context, id string) error
}

// Regions resolves the region of a project (the iam module's project
// spec.region, else the routing code of the project NID).
type Regions interface {
	Region(ctx context.Context, projectID string) (string, error)
}

// Platform is a compute platform as offered in a project's region.
type Platform struct {
	Name                   string
	AllowedForPreemptibles bool
	Presets                []Preset
}

// Preset is what one preset of a platform provides.
type Preset struct {
	Name      string
	GPUs      int
	VCPUs     int
	MemoryGiB int64
}

// Platforms resolves compute platforms (the compute module's catalog).
type Platforms interface {
	// Platform returns the platform offered in the project's region.
	// ok is false when the compute module is not enabled, so the platform
	// cannot be checked. A platform the region does not offer is
	// errs.NotFound.
	Platform(ctx context.Context, projectID, name string) (p Platform, ok bool, err error)
}

// RunningVMs counts the compute instances running under a pricing policy
// (status.running_vm_count), from the compute module's store
// (peer.PricingPolicyVMs). It returns 0 when compute is not enabled.
type RunningVMs interface {
	RunningVMCount(ctx context.Context, projectID, policyID string) (int64, error)
}

// Hierarchy resolves parents of resources (authz engine).
type Hierarchy interface {
	Parent(ctx context.Context, id string) (string, error)
}

// Operations submits long-running operations (pkg/operations engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates NIDs.
type IDGenerator interface {
	New(typ string) (string, error)
}
