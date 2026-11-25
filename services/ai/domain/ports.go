// Package domain holds the AI endpoint business logic: spec validation,
// cross-service reference checks, the lifecycle state machine and the stub
// data-plane decisions. It is transport-free and depends only on the ports
// declared in this file.
package domain

import (
	"context"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Resource type prefixes (NID types) used by the AI area.
const (
	KindEndpoint = "aiendpoint"
	// kindInstance is the type of the synthetic compute instance IDs shown
	// in status.instances. No compute instance is created.
	kindInstance = "computeinstance"
)

// Kinds of other areas that endpoint specs reference.
const (
	kindProject       = "project"
	kindSubnet        = "vpcsubnet"
	kindSecret        = "mbsec"
	kindSecretVersion = "mbsecver"
	kindPricingPolicy = "pricingpolicy"
)

// EndpointPage is one page of an endpoint listing.
type EndpointPage struct {
	Items         []*aipb.Endpoint
	NextPageToken string
}

// EndpointRepository stores endpoints with their full (secret-bearing) spec.
type EndpointRepository interface {
	Get(ctx context.Context, id string) (*aipb.Endpoint, error)
	GetByName(ctx context.Context, parentID, name string) (*aipb.Endpoint, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (EndpointPage, error)
	// Create fails with errs.AlreadyExists if the name is taken in the parent.
	Create(ctx context.Context, e *aipb.Endpoint) error
	// Update fails with errs.NotFound if the endpoint does not exist.
	Update(ctx context.Context, e *aipb.Endpoint) error
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

// Hierarchy resolves resources of any area (satisfied by *authz.Engine).
type Hierarchy interface {
	// HasKind reports whether the service owning kind is enabled.
	HasKind(kind string) bool
	// Parent returns the parent of id, or errs.NotFound if it does not exist.
	Parent(ctx context.Context, id string) (string, error)
}

// SecretSource finds the MysteryBox payload capability, when the mysterybox
// module provides one (looked up per call: it may be disabled).
type SecretSource interface {
	SecretPayloads() (peer.SecretPayloads, bool)
}

// DataPlane tells where the stub data plane serves endpoints.
type DataPlane interface {
	// BaseURL is the scheme and authority of the stub listener, e.g.
	// "http://127.0.0.1:4600".
	BaseURL() string
}
